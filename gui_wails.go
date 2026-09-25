//go:build wails

package main

// wails 版本地窗口（medreview_ui.exe）。
//
// 用 WebView2 渲染 HTML/CSS 当界面 —— 比手搓 Win32 控件好看得多，
// 但它**不是浏览器**：页面跑在本进程里，通过 wails 绑定直接调 Go 函数，
// 状态、日志都是从进程内读的，不走 HTTP、不需要口令，也就不会把口令暴露到网页上。
//
// 窗口里能干的事：
//   · 看地址（本机审阅页/下载页、局域网地址）和口令，点一下就能在浏览器里打开
//   · 就地改素材根目录 → 立刻重新扫描
//   · 就地改监听地址（含端口）→ 立刻重新绑定，不用重启程序
//   · 就地改下载口令 → 立刻生效，地址同步更新
//   · 实时日志 + 打开 medreview.log + 停止服务

import (
	"context"
	"embed"
	"log"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:ui
var uiEmbed embed.FS

// UI 是暴露给前端的一组方法。前端里叫 window.go.main.UI.xxx（全部返回 Promise）。
type UI struct {
	ctx context.Context
	a   *app
}

// Status 拉一份当前状态快照。
func (ui *UI) Status() uiStatus { return ui.a.Status() }

// logBatch 一次拉回来的增量日志。
// 用结构体而不是多返回值：wails 的调用约定只认「(值, error)」这一种两返回组合，
// 写成 ([]logLine, int64) 会被当成别的东西解。
type logBatch struct {
	Lines []logLine `json:"lines"`
	Seq   int64     `json:"seq"`
}

// Logs 增量拉日志（since = 上次拿到的最大 seq，传 0 表示全量）。
// 为了不让 wails 的事件机制成为唯一通路，这里用最简单的轮询：600ms 一次，够用。
func (ui *UI) Logs(since int64) logBatch {
	if ui.a.logs == nil {
		return logBatch{Lines: []logLine{}}
	}
	lines, seq := ui.a.logs.Since(since)
	return logBatch{Lines: lines, Seq: seq}
}

// PickFolder 弹系统「选择文件夹」对话框，返回绝对路径（取消则返回空串）。
func (ui *UI) PickFolder() string {
	p, err := wailsruntime.OpenDirectoryDialog(ui.ctx, wailsruntime.OpenDialogOptions{
		Title: "选择素材根目录",
	})
	if err != nil {
		log.Printf("选择文件夹失败: %v", err)
		return ""
	}
	return p
}

// ScanRoot 换素材根目录并重新扫描。返回最新的状态。
func (ui *UI) ScanRoot(p string) (uiStatus, error) {
	if err := ui.a.SetRoot(p); err != nil {
		return uiStatus{}, err
	}
	return ui.a.Status(), nil
}

// SetAddr 换监听地址（含端口），旧的 http 服务会被替换掉。
func (ui *UI) SetAddr(addr string) (uiStatus, error) {
	if err := ui.a.Rebind(addr); err != nil {
		return uiStatus{}, err
	}
	return ui.a.Status(), nil
}

// SetToken 换下载口令；地址里带的 ?t= 会同步更新。
func (ui *UI) SetToken(t string) (uiStatus, error) {
	if err := ui.a.SetToken(t); err != nil {
		return uiStatus{}, err
	}
	return ui.a.Status(), nil
}

// RandomToken 随机换一个新口令。
func (ui *UI) RandomToken() (uiStatus, error) {
	t, err := ui.a.NewToken()
	if err != nil {
		return uiStatus{}, err
	}
	log.Printf("已生成新下载口令: %s", t)
	return ui.a.Status(), nil
}

// Open 用系统默认浏览器打开一个地址。
func (ui *UI) Open(url string) { openBrowser(url) }

// OpenAppDir 在资源管理器里打开某个本地路径（素材目录 / 日志文件所在目录）。
func (ui *UI) OpenAppDir(p string) {
	if p == "" {
		return
	}
	openPathWithShell(p)
}

// Quit 停掉整个服务并退出进程（先做退出清理，再退出）。
func (ui *UI) Quit() {
	log.Println("本地窗口点了「停止服务」，退出")
	ui.a.Close()
	os.Exit(0)
}

// webviewUserDataPath WebView2 用户数据目录（所有 exe 副本共用）。
func webviewUserDataPath() string {
	base, err := os.UserCacheDir() // Windows = %LocalAppData%
	if err != nil {
		return ""
	}
	return filepath.Join(base, "medreview", "WebView2")
}

// runWails 弹出 WebView2 本地窗口并阻塞到窗口关闭。
//
// 兜底：万一本机 WebView2 初始化失败（没有运行时、策略禁用等），
// 退回到纯 Win32 窗口（gui_win32.go 的 runGUI），至少地址/口令/日志还能看，
// 服务也照常在跑 —— 用户不会因为界面起不来就彻底没法用。
func runWails(a *app) {
	api := &UI{a: a}

	err := wails.Run(&options.App{
		Title:            "medreview 控制台",
		Width:            860,
		Height:           760,
		MinWidth:         680,
		MinHeight:        520,
		BackgroundColour: &options.RGBA{R: 0x10, G: 0x13, B: 0x1a, A: 255},
		AssetServer: &assetserver.Options{
			Assets: uiEmbed,
		},
		Windows: &windows.Options{
			// 用户数据目录显式放到 LocalAppData：
			// ① 默认会跟着 exe 走，exe 在桌面/只读位置时容易出问题；
			// ② 多个 exe 副本共用一份，不会互相锁用户数据目录
			//   （锁冲突的典型症状就是「WebView2 进程已崩溃，需要重启」）。
			WebviewUserDataPath: webviewUserDataPath(),
			// 某些安全软件会强制渲染器代码完整性（RendererCodeIntegrity），
			// 导致 WebView2 渲染进程一启动就被杀、无限崩溃重启 —— 显式关掉。
			WebviewDisableRendererCodeIntegrity: true,
			// 本机实测 GPU 进程反复退出（process failed kind 6）；控制台页面
			// 用不上 GPU 加速，直接禁掉换取稳定。
			WebviewGpuIsDisabled: true,
		},
		Bind: []any{api},
		OnStartup: func(ctx context.Context) {
			api.ctx = ctx
		},
	})

	if err != nil {
		log.Printf("WebView2 窗口启动失败（%v），退回纯 Win32 本地窗口", err)
		st := a.Status()
		runGUI(guiConfig{
			ReviewURL:   st.ReviewURL,
			DownloadURL: st.DownloadURL,
			LANURLs:     st.LANURLs,
			Token:       st.Token,
			NoToken:     st.NoToken,
			LogPath:     st.LogPath,
			Logs:        a.logs,
			OpenBrowser: openBrowser,
			Shutdown:    func() { os.Exit(0) },
		})
		return
	}
	log.Println("控制台窗口已关闭，服务停止")
}
