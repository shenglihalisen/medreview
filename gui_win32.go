//go:build gui || wails

package main

// 纯 Windows API（user32/gdi32/kernel32）本地桌面窗口，替代之前用 walk 库的实现。
//
// walk 库在本机某些环境里 MainWindow.Create() 会静默卡死（不报错也不弹窗），
// 导致 ui 版（medreview_ui.exe）双击后「啥界面都没有」。这里改用手写 Win32
// 标准控件（STATIC / EDIT / BUTTON），不依赖任何第三方 GUI 库，兼容性最好。
//
// wails 构建里也带着它：万一目标机器上 WebView2 初始化失败（极老的 Windows），
// 还能退回到这套纯 Win32 窗口，服务照样能用（见 gui_wails.go 的兜底分支）。
//
// 窗口里显示：本机审阅页地址、下载页地址、下载口令、局域网地址、实时滚动日志，
// 外加「打开下载页」「停止服务」两个按钮。关窗口 / 点「停止服务」即退出服务。

import (
	"log"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	modUser32   = syscall.NewLazyDLL("user32.dll")
	modKernel32 = syscall.NewLazyDLL("kernel32.dll")
	modGdi32    = syscall.NewLazyDLL("gdi32.dll")

	pRegisterClassExW = modUser32.NewProc("RegisterClassExW")
	pCreateWindowExW  = modUser32.NewProc("CreateWindowExW")
	pDefWindowProcW   = modUser32.NewProc("DefWindowProcW")
	pShowWindow       = modUser32.NewProc("ShowWindow")
	pUpdateWindow     = modUser32.NewProc("UpdateWindow")
	pGetMessageW      = modUser32.NewProc("GetMessageW")
	pTranslateMessage = modUser32.NewProc("TranslateMessage")
	pDispatchMessageW = modUser32.NewProc("DispatchMessageW")
	pPostQuitMessage  = modUser32.NewProc("PostQuitMessage")
	pSetWindowTextW   = modUser32.NewProc("SetWindowTextW")
	pSendMessageW     = modUser32.NewProc("SendMessageW")
	pLoadCursorW      = modUser32.NewProc("LoadCursorW")
	pGetStockObject   = modGdi32.NewProc("GetStockObject")
	pGetModuleHandleW = modKernel32.NewProc("GetModuleHandleW")
)

const (
	cwUseDefault      = 0x80000000
	wsOverlappedWindow = 0x00CF0000
	wsVisible         = 0x10000000
	wsChild           = 0x40000000
	wsBorder          = 0x00800000
	wsVScroll         = 0x00200000
	esMultiline       = 0x0004
	esReadOnly        = 0x0800
	esAutoVScroll     = 0x0040
	wmCommand         = 0x0111
	wmDestroy         = 0x0002
	wmClose           = 0x0010
	wmSetFont         = 0x0030
	wmVScroll         = 0x0115
	emSetSel          = 0x00B1
	emReplaceSel      = 0x00C2
	swShow            = 5
	sbBottom          = 7
	defaultGuiFont    = 17
	idcArrow          = 32512
	idcOpen           = 1001
	idcStop           = 1002
)

// wndclassEx 是与 WNDCLASSEXW 等价的 Go 结构体（64 位布局）。
type wndclassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

// msg 与 MSG 等价。
type msg struct {
	hwnd    uintptr
	message uint32
	wparam  uintptr
	lparam  uintptr
	time    uint32
	ptX     int32
	ptY     int32
}

var (
	gCfg      *guiConfig
	gLogEdit  uintptr
	gFont     uintptr
	wndProcCB uintptr
)

// guiConfig 与 gui_stub.go（!gui 构建）保持同款结构，main.go 两种构建都能编译。
type guiConfig struct {
	ReviewURL   string
	DownloadURL string
	LANURLs     []string
	Token       string
	NoToken     bool
	LogPath     string
	Logs        *logRing
	OpenBrowser func(string)
	Shutdown    func()
}

func wndProc(hwnd, message, wparam, lparam uintptr) uintptr {
	switch message {
	case wmCommand:
		id := uint16(wparam & 0xffff)
		switch id {
		case idcOpen:
			if gCfg != nil && gCfg.OpenBrowser != nil {
				gCfg.OpenBrowser(gCfg.DownloadURL)
			}
		case idcStop:
			if gCfg != nil && gCfg.Shutdown != nil {
				gCfg.Shutdown()
			}
			pPostQuitMessage.Call(0)
		}
	case wmDestroy, wmClose:
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, message, wparam, lparam)
	return r
}

func setText(hwnd uintptr, text string) {
	p, _ := syscall.UTF16PtrFromString(text)
	pSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(p)))
}

// appendLog 把一段文本追加到日志编辑框末尾并滚到底。
func appendLog(text string) {
	if gLogEdit == 0 {
		return
	}
	p, _ := syscall.UTF16PtrFromString(text)
	pSendMessageW.Call(gLogEdit, emSetSel, 0xFFFFFFFF, 0xFFFFFFFF) // 选中末尾
	pSendMessageW.Call(gLogEdit, emReplaceSel, 0, uintptr(unsafe.Pointer(p)))
	pSendMessageW.Call(gLogEdit, wmVScroll, sbBottom, 0)
}

// createControl 创建一个子控件并返回其句柄。
func createControl(parent, hInst, font uintptr, class, text string, x, y, w, h, style, id uintptr) uintptr {
	cls, _ := syscall.UTF16PtrFromString(class)
	txt, _ := syscall.UTF16PtrFromString(text)
	child, _, _ := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(txt)),
		wsChild|wsVisible|style,
		x, y, w, h,
		parent, id, hInst, 0,
	)
	if font != 0 && child != 0 {
		pSendMessageW.Call(child, wmSetFont, font, 1)
	}
	return child
}

func buildControls(hwnd, hInst, font uintptr) {
	label := func(t string, x, y int) {
		createControl(hwnd, hInst, font, "STATIC", t, uintptr(x), uintptr(y), 720, 20, 0, 0)
	}
	label("审阅页（只能标记）", 16, 10)
	createControl(hwnd, hInst, font, "EDIT", gCfg.ReviewURL, 16, 30, 720, 22, wsBorder|esReadOnly, 0)
	label("下载页（标记 + 下载）", 16, 64)
	createControl(hwnd, hInst, font, "EDIT", gCfg.DownloadURL, 16, 84, 720, 22, wsBorder|esReadOnly, 0)
	label("下载口令", 16, 118)
	tok := gCfg.Token
	if gCfg.NoToken {
		tok = "（未启用口令，-open-dl）"
	}
	createControl(hwnd, hInst, font, "EDIT", tok, 16, 138, 720, 22, wsBorder|esReadOnly, 0)
	label("局域网地址（手机连同一 Wi-Fi 打开，记得横屏）", 16, 182)
	lan := strings.Join(gCfg.LANURLs, "\r\n")
	if lan == "" {
		lan = "（没找到可用的局域网地址，确认电脑已连 Wi-Fi / 有线网）"
	}
	createControl(hwnd, hInst, font, "EDIT", lan, 16, 202, 720, 46, wsBorder|esReadOnly|esMultiline|esAutoVScroll|wsVScroll, 0)
	label("服务端日志（实时）", 16, 262)
	gLogEdit = createControl(hwnd, hInst, font, "EDIT", "", 16, 282, 720, 272, wsBorder|esReadOnly|esMultiline|esAutoVScroll|wsVScroll, 0)
	createControl(hwnd, hInst, font, "BUTTON", "打开下载页", 16, 568, 150, 32, 0, idcOpen)
	createControl(hwnd, hInst, font, "BUTTON", "停止服务", 176, 568, 150, 32, 0, idcStop)
}

// logPump 每 400ms 把日志环形缓冲里的新行刷进窗口。
func logPump() {
	var lastSeq int64
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		if gCfg == nil || gCfg.Logs == nil {
			return
		}
		lines, seq := gCfg.Logs.Since(lastSeq)
		if len(lines) == 0 {
			continue
		}
		lastSeq = seq
		var b strings.Builder
		for _, l := range lines {
			b.WriteString(l.Text)
			b.WriteString("\r\n")
		}
		appendLog(b.String())
	}
}

// runGUI 弹出本地桌面窗口并阻塞到窗口关闭。
func runGUI(cfg guiConfig) {
	gCfg = &cfg

	hInst, _, _ := pGetModuleHandleW.Call(0)
	if hInst == 0 {
		log.Printf("获取模块句柄失败")
		select {}
	}

	className, _ := syscall.UTF16PtrFromString("MedreviewGUIClass")
	cursor, _, _ := pLoadCursorW.Call(0, idcArrow)
	wndProcCB = syscall.NewCallback(wndProc)
	wc := wndclassEx{
		cbSize:        uint32(unsafe.Sizeof(wndclassEx{})),
		lpfnWndProc:   wndProcCB,
		hInstance:     hInst,
		hCursor:       cursor,
		hbrBackground: 6, // (HBRUSH)(COLOR_WINDOW + 1)
		lpszClassName: className,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	ret, _, err := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if ret == 0 {
		log.Printf("注册窗口类失败: %v", err)
		select {}
	}

	title, _ := syscall.UTF16PtrFromString("medreview 控制台")
	hwnd, _, err := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		wsOverlappedWindow|wsVisible,
		cwUseDefault, cwUseDefault, 760, 632,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		log.Printf("创建窗口失败: %v", err)
		// 窗口建不出来就保底让服务继续跑（不退出进程），用户还能靠浏览器/端口用。
		select {}
	}

	gFont, _, _ = pGetStockObject.Call(defaultGuiFont)
	if gFont != 0 {
		pSendMessageW.Call(hwnd, wmSetFont, gFont, 1)
	}
	buildControls(hwnd, hInst, gFont)
	pShowWindow.Call(hwnd, swShow)
	pUpdateWindow.Call(hwnd)

	go logPump()

	var m msg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 { // WM_QUIT
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}
