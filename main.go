package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

//go:embed all:web
var webEmbed embed.FS

// checkAddr 检查监听地址能不能用：必须是 host:port，且端口是 1~65535 的数字。
// 目的不是做安全过滤，而是把 `lookup tcp/=: unknown port` 这种 Go 原始报错
// 提前换成一句人话（启动脚本拼错参数时真的会发生）。
func checkAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("格式要写成 :端口 或 IP:端口，例如 :8080")
	}
	if strings.TrimSpace(host) != host {
		return fmt.Errorf("地址前后多了空格")
	}
	// 0 是合法的：**让系统挑一个空闲端口**（app.go 里靠它拿真实端口），别把它挡了。
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("端口 %q 不是 0~65535 的数字（0 = 让系统挑一个空闲端口）", port)
	}
	return nil
}

func main() {
	root := flag.String("root", "", "素材根目录；留空则从上次记录读取，或在网页里选择")
	addr := flag.String("addr", ":8080", "监听地址，例如 :8080 或 0.0.0.0:8080")
	dbPath := flag.String("db", "medreview.db", "审阅状态数据库路径；默认每次启动自动清空（退出即全清），想跨次保留得显式指定同一个文件")
	csvPath := flag.String("csv", "", "退出时把审阅标记导出成 CSV 的路径；留空 = 与数据库同目录的 <库名>-review.csv（每轮覆盖同一份）")
	cacheDir := flag.String("cache", "cache", "转码缓存目录")
	token := flag.String("token", "", "下载页口令；留空则每次启动随机生成")
	openDL := flag.Bool("open-dl", false, "关闭下载权限校验（任何页面都能打包下载）")
	vjobs := flag.Int("vjobs", 3, "视频预转码并发数，即预热时同时转几个视频")
	encFlag := flag.String("enc", "", "视频转码编码：cpu=libx264 软编（默认）；nvenc/qsv/amf=只用指定的这些（逗号分隔，如 nvenc,qsv）；auto=都比速度，快 20%+ 才换")
	qcFlag := flag.String("qcdec", "", "视频 QC 解码：cpu=软解（默认）；cuda/qsv/d3d11va=只用指定的这些（逗号分隔）；auto=探测后择优")
	gpuqc := flag.String("gpuqc", "", "【已拆细，新脚本请用 -enc / -qcdec】兼容开关：y=两处都让程序择优用显卡；n=两处都软解")
	vres := flag.Int("vres", 720, "转码分辨率：720p（把较小的一边封顶到 720，另一边等比）；0 = 保持原分辨率")
	ires := flag.Int("ires", 1600, "图片预览分辨率：把较小的一边封顶到这个值（点开时现转+缓存，原图在灯箱里用「看原图」看）；0 = 一律原图")
	vkeep := flag.Bool("vkeep", false, "保留上次启动留下的转码产物。默认每次启动都清掉上一轮的产物（避免残留占地方）；加了这个开关才会复用，代价是可能用到老产物")
	autoOpen := flag.Bool("open", false, "启动后自动打开审阅页（想开下载页就从审阅页切，或手动加 -addr 后缀）")
	autoClose := flag.Bool("autoclose", false, "QC 全部完成后自动退出（硬件测试脚本用，配合 -gpuqc 对比耗时）")
	logFile := flag.String("log", "", "日志文件路径；留空自动挑：当前目录 → 程序所在目录 → 临时目录")
	gpuInfo := flag.Bool("gpuinfo", false, "只探测并打印本机显卡（独显/核显）然后退出；start.bat 用它来决定问哪几个问题")
	flag.Parse()

	// ---------- 命令行里可能带的手工引号，统一在这里收口 ----------
	// 粘贴路径十有八九会把 "D:\我的 素材" 整段带引号粘进来。
	// 以前是靠 start.bat 里 `set "X=!X:"=!"` 去引号的，那招有两个问题：
	//   1. **留空时会变成 "="**（实测：变量为空 → 结果是 `"=` 两个字），
	//      于是"启动后在网页里选目录"这条正路被堵死；
	//   2. 只对 bat 那条路生效，拖图标进来的参数、别的调用方各写各的。
	// 现在统一在程序侧 trim，一处管全部入口。
	*root = strings.Trim(strings.TrimSpace(*root), `"`)
	*token = strings.Trim(strings.TrimSpace(*token), `"`)
	*addr = strings.Trim(strings.TrimSpace(*addr), `"`)

	// ---------- -gpuinfo：只列显卡就退出 ----------
	// 必须排在建日志和 wipeDB **之前**：这是一个纯查询，不能碰数据库、
	// 更不能把上次那个库删了（bat 启动时第一件事就是调它）。
	if *gpuInfo {
		printGPUInfo()
		return
	}

	// ---------- 拖文件夹到图标上启动 ----------
	// Windows 把被拖的那个文件夹路径作为命令行参数传进来（可能带引号），
	// 直接当素材根目录用。显式写了 -root 时以 -root 为准。
	// 这条支持的是「把文件夹拖到 medreview.exe / 快捷方式图标上就开工」。
	if *root == "" {
		if args := flag.Args(); len(args) > 0 {
			p := strings.Trim(strings.TrimSpace(args[0]), `"`)
			if p != "" {
				*root = p
			}
		}
	}

	// ---------- 日志 ----------
	// 三份：黑窗口（console 版）/ 本地窗口或网页面板（gui 版）/ medreview.log（落盘）。
	// 无黑窗版没有第一份，后两份就是唯一的信息出口，所以这一步必须放在最前面。
	logRing500 := NewLogRing(500)
	logFilePath = setupLogging(*logFile, logRing500)

	// ---------- 监听地址校验 ----------
	// 启动脚本万一拼错参数，会把乱七八糟的东西塞进 -addr（实测见过 ":="），
	// 结果是一句 Go 的原始报错 `lookup tcp/=: unknown port`，根本看不懂。
	// 这里先查一遍，给人话。
	if err := checkAddr(*addr); err != nil {
		fatalf("监听地址不对：%s（%v）\n  端口填 1~65535 的数字就行，例如 8080；直接回车用默认的 8080。", *addr, err)
	}

	// ---------- 素材根目录有效性校验 ----------
	// -root 参数 / 拖入的路径可能因为启动脚本的参数拼接出错而变成一串非法字符
	// （实测：start.bat 引号错位时传进来过 `D:" -token 1123 -addr :=`）。
	// 以前是照单全收 → 扫描失败、页面上显示一串乱码路径，看着像程序坏了。
	// 现在：无效就忽略掉，启动后让用户在网页里重新选目录，服务照常可用。
	if *root != "" {
		if st, serr := os.Stat(*root); serr != nil || !st.IsDir() {
			log.Printf("警告：素材根目录无效，已忽略 —— %s", *root)
			log.Printf("      启动后请在审阅页里重新选择素材目录。")
			*root = ""
		}
	}

	// ---------- 启动兜底清库 ----------
	// 上轮被硬杀（叉掉黑窗口 / taskkill）时来不及清，库里留着一地残渣，
	// 下次打开还是上一轮的老界面（root_path 被复用、素材/标记/QC 全在）。
	// 所以开库前先清一遍；用完这次的库由 Close() 再清一次，双保险。
	//
	// ⚠️ 但**只有在确认没别的活实例在跑**时才敢删：端口一被占就说明有实例正用这个库，
	//    这时候删等于把人家那轮审阅弄丢 —— 而且 Windows 上对方还会照常往已删的文件里写，
	//    表面看不出异常，记录直接读不回来了。所以先探一下端口：
	//    探不动 → 库原样留着（下面 bind() 会给「端口已被占用」的人话提示并退出）；
	//    探得动 → 没人在用 → 此刻 openDB 还没跑，删得干干净净。
	if ln, verr := net.Listen("tcp", *addr); verr != nil {
		log.Printf("端口被占用（%s），跳过本次清库 —— 不碰上一个实例的数据库", *addr)
	} else {
		_ = ln.Close()
		wipeDB(*dbPath)
	}

	// ---------- 硬件选择：优先级 显式 -enc/-qcdec > 兼容 -gpuqc > 默认软解 ----------
	//
	// 2026-10-04 拆细：转码编码与 QC 解码分开选（bat 问四个问题）。
	// 默认两处都是 cpu（软解）—— 用户定的「默认有软解，显卡要主动开」。
	// -gpuqc 是老开关（qc-bench.bat 还在用）：y=两处 auto，n=两处 cpu。
	encHW, qcHW := "cpu", "cpu"
	switch strings.ToLower(strings.TrimSpace(*gpuqc)) {
	case "y", "yes", "1":
		encHW, qcHW = "auto", "auto"
	}
	if s := strings.TrimSpace(*encFlag); s != "" {
		encHW = s
	}
	if s := strings.TrimSpace(*qcFlag); s != "" {
		qcHW = s
	}
	log.Printf("硬件选择：视频转码编码=%s / 视频 QC 解码=%s   （cpu=软解；具体卡名=只用那一张；auto=程序实测后择优）",
		encHW, qcHW)

	sub, err := fs.Sub(webEmbed, "web")
	if err != nil {
		fatalf("内嵌资源异常: %v", err)
	}
	webFS = sub

	cfg := appConfig{
		root:      *root,
		addr:      *addr,
		dbPath:    *dbPath,
		cacheDir:  *cacheDir,
		token:     *token,
		openDL:    *openDL,
		vjobs:     *vjobs,
		qcHW:      qcHW,
		encHW:     encHW,
		vres:      *vres,
		ires:      *ires,
		vkeep:     *vkeep,
		autoOpen:  *autoOpen,
		autoClose: *autoClose,
		logFile:   *logFile,
		csvPath:   *csvPath,
	}

	a, err := newApp(cfg, logRing500, logFilePath)
	if err != nil {
		fatalf("初始化失败: %v", err)
	}
	defer a.Close()

	// 端口必须先占住，才能报地址/口令/写 urls.txt（理由见 app.bind 的注释）。
	// 起不来就直接退出，gui 版会把日志弹给用户看。
	if err := a.bind(); err != nil {
		fatalExit()
	}

	a.printBanner()

	// 只有显式传 -open 才会开浏览器（gui 版默认不开，本地窗口就是控制台）。
	// 开过一次就置位，避免预热结束时再弹一次。
	if *autoOpen {
		a.openedBrowser = true
		// 默认打开审阅页（2026-09-25 用户定稿）：下载页从审阅页里切过去
		openBrowser(a.Status().ReviewURL)
	}

	a.prepareCache()
	// 扫描放在 banner 之后启动，控制台顺序就是「地址 → 扫描 → 预检 → 启动成功」，
	// 不会出现"扫描完成"打在地址前面那种前后颠倒的观感。
	a.startScan()

	// console 版 Ctrl+C：优雅退出（顺带清理临时缓存），而不是直接被杀。
	// 黑窗口被直接叉掉 / taskkill 属于硬杀，没有机会清理 —— 留给下次启动兜底。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		if _, ok := <-sigCh; ok {
			log.Println("收到 Ctrl+C，正在停止服务并清理临时缓存与数据库...")
			a.Close()
			os.Exit(0)
		}
	}()

	switch buildMode {
	case "gui":
		// gui 版（纯 Win32 本地窗口，备用版）：HTTP 服务跑在后台，
		// 主线程交给本地桌面窗口（runGUI 阻塞）。关窗口 = 停服务。
		a.serveBackground()
		st := a.Status()
		runGUI(guiConfig{
			ReviewURL:   st.ReviewURL,
			DownloadURL: st.DownloadURL,
			LANURLs:     st.LANURLs,
			Token:       st.Token,
			NoToken:     st.NoToken,
			LogPath:     logFilePath,
			Logs:        logRing500,
			OpenBrowser: openBrowser,
			Shutdown:    func() { os.Exit(0) },
		})
		return

	case "wails":
		// wails 版（WebView2 网页级本地窗口）：同上，但窗口内容是 HTML/CSS，
		// 而且能就地改素材根目录 / 监听地址 / 下载口令（见 gui_wails.go）。
		a.serveBackground()
		runWails(a)
		return

	default:
		a.printListenLine()
		if err := a.serveNow(); err != nil {
			fatalf("服务退出: %v", err)
		}
	}
}

// printListenLine console 版的最后一行：告诉用户服务真的起来了。
func (a *app) printListenLine() {
	log.Printf("服务已启动: %s", a.cfg.addr)
}

// ---------------------------------------------------------------- 日志与退出
//
// 无黑窗版没有控制台：log.Printf 打到 os.Stderr 等于进了黑洞，
// 用户既看不到地址也看不到为什么挂了。所以日志必须同时落到：
//   · os.Stderr —— console 版的黑窗口
//   · ring      —— 本地窗口 / 网页面板的环形缓冲
//   · 日志文件  —— 出问题/事后复盘时打开 medreview.log 看

// logFilePath 是本次启动真正用到的日志文件路径（给 fatalExit 弹窗用）
var logFilePath string

// nopErrWriter 包装一个 writer，把写错误吞掉。
// 给 stderr 用：GUI 子系统的进程从资源管理器双击时 stderr 句柄无效，
// io.MultiWriter 见错即停，会把后面正常的文件写入也一起带没了。
type nopErrWriter struct{ w io.Writer }

func (n nopErrWriter) Write(p []byte) (int, error) {
	n.w.Write(p) // 错误忽略
	return len(p), nil
}

// setupLogging 打开日志文件并把 log 输出重定向到「黑窗口 + 环形缓冲 + 文件」。
// 返回实际用到的路径；一个都打不开就只保留前两个，返回空串。
//
// 路径优先级：命令行 -log → 当前目录 → 程序所在目录 → 临时目录。
// 先试当前目录是为了让验收脚本各自跑在隔离目录里互不干扰。
func setupLogging(want string, ring io.Writer) string {
	cands := []string{}
	if want != "" {
		cands = append(cands, want)
	} else {
		cands = append(cands, "medreview.log")
		if exe, err := os.Executable(); err == nil {
			cands = append(cands, filepath.Join(filepath.Dir(exe), "medreview.log"))
		}
		cands = append(cands, filepath.Join(os.TempDir(), "medreview.log"))
	}
	for _, p := range cands {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			continue
		}
		fmt.Fprintf(f, "=== medreview 启动 %s ===\n", time.Now().Format("2006-01-02 15:04:05"))
		// stderr 必须包一层「吞错误」的 writer：无黑窗版从资源管理器双击时
		// stderr 是无效句柄，而 io.MultiWriter 只要有一个 writer 报错就整体中止 ——
		// 不包的话每条 log.Printf 都会在 stderr 上失败，**日志文件从此一行都进不来**
		// （只剩 setupLogging 直写的启动头），用户拿着一个空日志根本没法排查。
		log.SetOutput(io.MultiWriter(nopErrWriter{os.Stderr}, f, ring))
		return p
	}
	log.SetOutput(io.MultiWriter(os.Stderr, ring))
	log.Printf("警告：日志文件打不开（试过 %v），本次只输出到窗口和网页控制台", cands)
	return ""
}

// fatalf 打印一条错误后退出。无黑窗版会把日志文件弹出来——
// 否则用户双击完没反应，连"端口被占"还是"数据库坏了"都无从得知。
func fatalf(format string, args ...any) {
	log.Printf(format, args...)
	fatalExit()
}

// fatalExit 退出。无黑窗版先把日志弹给用户看。
func fatalExit() {
	if buildMode != "console" && logFilePath != "" {
		openPathWithShell(logFilePath)
	}
	os.Exit(1)
}

// flagWasSet 判断命令行里有没有显式写过某个开关。
// 只看 flag 的默认值分不清"没写"和"写了 false"，需要它的时候在这儿。
func flagWasSet(name string) bool {
	hit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			hit = true
		}
	})
	return hit
}

// isAddrInUse 判断监听失败是不是"端口被占"（Go 的报错文本跨平台不统一，两种都认）。
func isAddrInUse(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "only one usage") || // Windows
		strings.Contains(s, "address already in use") || // Linux/macOS
		strings.Contains(s, "permission denied")
}

// randomToken 生成 24 个 hex 字符（12 字节熵）的下载口令。
//
// 注意：以前读随机源失败时会静默返回 "letmein" —— 那等于把下载权限直接送人，
// 而且控制台上完全看不出来（地址照样打印，口令打印成 letmein 也没人注意）。
// 现在直接失败退出：拿不到真随机就别开一个有下载权限的服务。
func randomToken() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		fatalf("无法生成随机下载口令（%v）。请显式指定 -token <口令>，或检查系统随机数源", err)
	}
	return hex.EncodeToString(b)
}

// writeURLFile 写 urls.txt：前两行固定是 canonical 的本机地址（老版本就只有这两行），
// 后面追加局域网地址，方便手机端直接复制。这个文件全项目只写不读，没有兼容负担。
func writeURLFile(reviewURL, downloadURL string, lanLines []string) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s\n", reviewURL, downloadURL)
	for _, l := range lanLines {
		fmt.Fprintf(&b, "%s\n", l)
	}
	_ = os.WriteFile("urls.txt", []byte(b.String()), 0o644)
}

func displayPath(p string) string {
	if p == "" {
		return "(未设置，请在网页里选择)"
	}
	return p
}

func openBrowser(url string) {
	if runtime.GOOS != "windows" {
		opener := "open"
		if runtime.GOOS == "linux" {
			opener = "xdg-open"
		}
		_ = exec.Command(opener, url).Start()
		return
	}
	// Windows 注意：
	//  1. rundll32 会被本机安全策略拦截（Start-Process/WMI 同理）→ 只能兜底。
	//  2. **不要优先用 explorer.exe 带 URL**：下载页地址里有 "?t=口令"，
	//     Explorer 的命令行把 ? 当通配符解析，匹配不到就退回默认目录 ——
	//     表现就是"转码完/启动完莫名弹出 Documents 文件夹"。
	//     URL 一律交给 ShellExecute：`cmd /c start "" <url>`（空标题是必须的，
	//     否则被引号包住的 URL 会被当成窗口标题）。
	candidates := [][]string{
		{"cmd", "/c", "start", "", url},
		{"rundll32", "url.dll,FileProtocolHandler", url},
		{"explorer.exe", url},
	}
	for _, c := range candidates {
		cmd := exec.Command(c[0], c[1:]...)
		hideWindow(cmd) // 无黑窗版不能因为"开个浏览器"就闪一下黑框
		if err := cmd.Start(); err == nil {
			log.Printf("已用 %s 打开浏览器: %s", c[0], url)
			go func() { _ = cmd.Wait() }() // 收尸，不阻塞
			return
		}
	}
	log.Printf("打开浏览器失败，请手动访问: %s", url)
}
