package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- 运行参数

// appConfig 一次运行要用的全部参数（命令行标志解析后的结果）。
//
// gui / console 两种构建共用同一套启动流程，差别只在最后一步：
// console 版阻塞在 srv.Serve 上，gui 版把 HTTP 服务丢到后台、主线程交给本地窗口。
type appConfig struct {
	root     string // 素材根目录（命令行 -root，或拖到图标上的那个文件夹）
	addr     string // 监听地址，例如 :8080
	dbPath   string // 审阅状态数据库
	cacheDir string // 转码缓存目录
	token    string // 下载口令，空 = 随机生成
	openDL   bool   // 关闭下载口令校验
	vjobs    int    // 视频预热并发数
	vres     int    // 视频转码规格（较小边封顶）
	ires     int    // 图片预览规格（较小边封顶）
	vkeep    bool   // 保留上一轮的转码产物
	autoOpen bool   // 启动后自动打开下载页
	logFile  string // 日志文件路径
}

// app 一次运行起来的全部零件。
//
// 之所以把这些东西从 main() 里拆出来：本地窗口（wails 版控制台）要能**就地改配置** ——
// 换素材根目录、改下载口令、改监听地址都不用重启程序：
//
//	换根目录 → SetRoot：store 内部换个指针，再扫一遍（ scanner 会顺手持久化）
//	改口令   → SetToken：Handler 里的口令换成新的，带口令的下载页地址跟着重算
//	改地址   → Rebind：  关掉旧的 listener/server，按新地址重新占端口再 Serve
//
// console 版完全用不到后面三个，但走的是同一份启动代码，行为不会长歪。
type app struct {
	cfg appConfig

	db      *sql.DB
	store   *Store
	scanner *Scanner
	videos  *VideoService
	images  *ImageService
	hub     *Hub
	h       *Handler

	logs    *logRing // 日志环形缓冲：本地窗口从这里拉实时日志
	logPath string   // 日志文件落盘路径

	// netMu 护住 listener 和 server 这一对：改地址要「先关旧的、再起新的」，
	// 不能和网络线程同时动。
	netMu sync.Mutex
	ln    net.Listener
	srv   *http.Server

	// urlMu 护住下面这堆对外地址：它们在绑定端口后算出，
	// 改口令/改地址时会重算，本地窗口读的时候不能读到半新半旧的一组。
	urlMu       sync.RWMutex
	token       string
	reviewURL   string
	downloadURL string
	lanURLs     []string
	lanDLURL    string

	// 预热状态：换素材目录会取消上一轮，免得两轮抢 CPU
	warmMu     sync.Mutex
	warmCancel context.CancelFunc
	warmSeq    int

	cleanupOnce sync.Once // 退出清缓存只做一次（defer Close / Ctrl+C / 停止按钮 多个入口）

	openedBrowser bool // 启动阶段已经弹过浏览器就不要再弹第二次
}

// newApp 打开数据库并建好全部服务。这里**不占端口** —— 占端口是 bind() 的事。
func newApp(cfg appConfig, logs *logRing, logPath string) (*app, error) {
	a := &app{cfg: cfg, logs: logs, logPath: logPath}

	db, err := openDB(cfg.dbPath)
	if err != nil {
		return nil, err
	}
	a.db = db

	a.hub = NewHub()
	a.scanner = NewScanner(db, a.hub)
	a.store = NewStore(db, cfg.root)
	a.videos, err = NewVideoService(a.store, cfg.cacheDir, cfg.vres)
	if err != nil {
		return nil, err
	}
	a.images, err = NewImageService(a.store, cfg.cacheDir, cfg.ires)
	if err != nil {
		return nil, err
	}

	tok := cfg.token
	if tok == "" {
		tok = randomToken()
	}
	a.token = tok

	a.h = &Handler{
		store:   a.store,
		scanner: a.scanner,
		video:   a.videos,
		images:  a.images,
		hub:     a.hub,
		dlToken: tok,
		noToken: cfg.openDL,
		logs:    logs,
		logPath: logPath,
		// 「有没有本地窗口」：gui / wails 版都有，console 版只有黑窗口
		gui: buildMode != "console",
	}
	// reviewURL / downloadURL / lanURLs 要等端口真正占住、拿到真实端口后才填

	// 扫描完成后 invalidate 掉目录计数缓存，并把视频预热顶起来
	a.scanner.onFinish = func() {
		a.store.invalidate()
		a.startWarmup()
	}
	return a, nil
}

// Close 进程退出前的收尾：取消预热、清空临时缓存、释放数据库。
// 正常返回路径（窗口关闭 / main return）都会走到；被硬杀（关黑窗口/taskkill）
// 没机会执行 —— 那种情况留给下次启动的 PrepareCache 兜底。
func (a *app) Close() {
	a.cleanupTemp()
	a.warmMu.Lock()
	if a.warmCancel != nil {
		a.warmCancel()
		a.warmCancel = nil
	}
	a.warmMu.Unlock()
	if a.db != nil {
		_ = a.db.Close()
	}
}

// ---------------------------------------------------------------- 绑定端口与地址

// bind 占住端口、算出对外地址、把它们交给网页Handler并写 urls.txt。
//
// 顺序很讲究：**必须先占住端口，再报地址和口令**。反过来的话，端口被占用时
// 会先打印一个"看起来能用"的口令，用户拿着它去访问，命中的却是已经在跑的那个实例，
// 现象是"当前页面无下载权限"—— 很难排查。
func (a *app) bind() error {
	a.netMu.Lock()
	defer a.netMu.Unlock()

	ln, err := net.Listen("tcp", a.cfg.addr)
	if err != nil {
		log.Printf("无法监听 %s: %v", a.cfg.addr, err)
		if isAddrInUse(err) {
			log.Println("端口已被占用：很可能已经有一个审阅服务在跑（例如上一个窗口还没关）。")
			log.Println("请关掉那个窗口后重新启动；如果确实要同时跑两个，换端口：-addr :8081")
		}
		return err
	}
	a.ln = ln
	a.computeURLsLocked()
	a.publishURLsLocked()
	return nil
}

// computeURLsLocked 从**真实监听地址**算出对外URL。必须在 netMu 下调用。
//
// 用真实端口而不是解析 -addr 字符串：-addr :0（让系统挑端口）时字符串里的端口是 0，
// 直接拿去拼会给出一个打不开的地址；-addr 0.0.0.0:8080 也统一换成 127.0.0.1，
// "0.0.0.0" 在浏览器里不是个能访问的地址。
func (a *app) computeURLsLocked() {
	port := 0
	if a.ln != nil {
		if ta, ok := a.ln.Addr().(*net.TCPAddr); ok {
			port = ta.Port
		}
	}

	host := a.cfg.addr
	if host[0] == ':' {
		host = "127.0.0.1" + host
	}
	reviewURL := "http://" + host + "/review.html"
	downloadURL := "http://" + host + "/download.html"
	if !a.cfg.openDL {
		downloadURL += "?t=" + a.token
	}

	if port > 0 {
		if sh, _, serr := net.SplitHostPort(a.cfg.addr); serr == nil {
			if sh == "" || sh == "0.0.0.0" || sh == "::" || sh == "[::]" {
				sh = "127.0.0.1"
			}
			reviewURL = fmt.Sprintf("http://%s:%d/review.html", sh, port)
			downloadURL = fmt.Sprintf("http://%s:%d/download.html", sh, port)
			if !a.cfg.openDL {
				downloadURL += "?t=" + a.token
			}
		}
	}

	// 只有"绑了全网卡"才去枚举局域网地址：显式绑了某个具体 IP，
	// 说明用户知道自己在干什么，再列一堆别的网卡地址只会制造混乱。
	bindHost := a.cfg.addr
	if i := strings.LastIndex(bindHost, ":"); i >= 0 {
		bindHost = bindHost[:i]
	}
	allIface := bindHost == "" || bindHost == "0.0.0.0" || bindHost == "::" || bindHost == "[::]"

	var lanReviewURLs []string
	lanDLURL := ""
	if allIface {
		p := port
		if p == 0 {
			p = 8080
		}
		ips := lanIPv4s()
		for _, ip := range ips {
			lanReviewURLs = append(lanReviewURLs, fmt.Sprintf("http://%s:%d/review.html", ip, p))
		}
		if len(ips) > 0 {
			lanDLURL = fmt.Sprintf("http://%s:%d/download.html", ips[0], p)
			if !a.cfg.openDL {
				lanDLURL += "?t=" + a.token
			}
		}
	}

	a.urlMu.Lock()
	a.reviewURL = reviewURL
	a.downloadURL = downloadURL
	a.lanURLs = lanReviewURLs
	a.lanDLURL = lanDLURL
	a.urlMu.Unlock()
}

// publishURLsLocked 把地址交给网页Handler（/api/status 会用到 root，
// 其余字段留着给将来的控制台用），并写 urls.txt。
// 端口确定占住了才写 urls.txt，避免把没起来的这次的口令写进去、
// 覆盖掉正在跑的那个实例写进去的。
func (a *app) publishURLsLocked() {
	a.urlMu.RLock()
	reviewURL := a.reviewURL
	downloadURL := a.downloadURL
	lanLines := append([]string{}, a.lanURLs...)
	if a.lanDLURL != "" {
		lanLines = append(lanLines, a.lanDLURL)
	}
	a.urlMu.RUnlock()

	a.h.mu.Lock()
	a.h.reviewURL = reviewURL
	a.h.downloadURL = downloadURL
	a.h.lanURLs = lanLines
	a.h.mu.Unlock()

	writeURLFile(reviewURL, downloadURL, lanLines)
}

// ---------------------------------------------------------------- 启动流程

// printBanner 打印「地址 / 口令 / 局域网地址」这一段。
// console 版的老规矩：扫描必须在 banner 之后启动，否则控制台顺序会前后颠倒
// （"扫描完成"打在地址前面）。
func (a *app) printBanner() {
	a.urlMu.RLock()
	reviewURL := a.reviewURL
	downloadURL := a.downloadURL
	lanURLs := a.lanURLs
	lanDLURL := a.lanDLURL
	a.urlMu.RUnlock()

	log.Println("--------------------------------------------------")
	log.Printf("素材根目录: %s", displayPath(a.store.Root()))
	log.Printf("审阅页（只能标记）: %s", reviewURL)
	if a.cfg.openDL {
		log.Printf("下载页（可标记+下载）: %s  [未启用口令]", downloadURL)
	} else {
		log.Printf("下载页（可标记+下载）: %s", downloadURL)
		log.Printf("下载口令: %s", a.token)
	}

	bindHost := a.cfg.addr
	if i := strings.LastIndex(bindHost, ":"); i >= 0 {
		bindHost = bindHost[:i]
	}
	allIface := bindHost == "" || bindHost == "0.0.0.0" || bindHost == "::" || bindHost == "[::]"
	log.Println("--------------------------------------------------")
	switch {
	case len(lanURLs) > 0:
		log.Println("局域网地址（手机连同一个 Wi-Fi 直接打开，记得横屏）：")
		for i, u := range lanURLs {
			log.Printf("  %d) %s", i+1, u)
		}
		log.Printf("  下载页: %s", lanDLURL)
		log.Println("  手机打不开？在 Windows 防火墙里放行本程序的「专用网络」入站。")
	case allIface:
		log.Println("未找到可用的局域网地址（确认电脑已连上 Wi-Fi 或有线网）。")
	default:
		p := 8080
		if a.ln != nil {
			if ta, ok := a.ln.Addr().(*net.TCPAddr); ok && ta.Port > 0 {
				p = ta.Port
			}
		}
		log.Printf("当前只监听了 %s，手机访问不到；要让手机也能用，请改成 -addr :%d", a.cfg.addr, p)
	}
	log.Println("--------------------------------------------------")
	log.Println("正在预检视频：编码能直接播的会跳过，播不了的才转码；")
	log.Println("全部就绪后会提示「启动成功」（期间上面的地址已经可以正常打开使用）。")
	log.Println("--------------------------------------------------")
}

// prepareCache 清掉上一轮留下的转码产物（除非 -vkeep）。
// 放在「占住端口之后」：双击第二次启动的那个"注定起不来"的实例不能把正在跑的缓存删掉。
func (a *app) prepareCache() {
	if err := a.videos.PrepareCache(a.cfg.vkeep); err != nil {
		log.Printf("清理转码缓存失败: %v", err)
	}
	if err := a.images.PrepareCache(); err != nil {
		log.Printf("清理图片预览缓存失败: %v", err)
	}
}

// startScan 按上次记录的 / 命令行给的根目录启动首轮扫描。
// 没有根目录就跑一轮空的预热，好让「启动成功」的提示照常出现。
func (a *app) startScan() {
	startRoot := a.cfg.root
	if startRoot == "" {
		if saved := metaGet(a.db, "root_path"); saved != "" {
			startRoot = saved
		}
	}
	if startRoot != "" {
		if abs, aerr := filepath.Abs(startRoot); aerr == nil {
			a.store.SetRoot(abs)
		}
		if err := a.scanner.Start(startRoot); err != nil {
			log.Printf("启动时扫描 %s 失败: %v", startRoot, err)
			// 换电脑 / 移动了素材盘之后最常见的情况：记录里的老目录不在了。
			// 讲清楚三件事：数据没丢、老记录还在库里、指到新目录会自动接上
			// （相同相对路径 + 相同文件的标记自动保留，文件变了才会作废）。
			if errors.Is(err, errNotDir) || os.IsNotExist(err) {
				log.Println("上一次使用的素材目录现在不存在了（可能移动了硬盘 / 换了电脑 / 改了目录名）。")
				log.Println("放心：已审阅的记录都还保存在 medreview.db 里，一条没丢。")
				log.Println("重新指到新位置即可：把新的素材文件夹拖到启动器图标上重新打开，")
				log.Println("或在浏览器页面的「选择素材目录」里选新位置。相同文件的标记会自动接上。")
			}
			a.startWarmup()
		}
		return
	}
	a.startWarmup()
}

// server 造一个新的 http.Server。
// WriteTimeout 必须为 0：流式打包 80G 素材可能持续很久，设了会被掐断。
func (a *app) server() *http.Server {
	return &http.Server{
		Handler:           a.h.Routes(),
		ReadHeaderTimeout: 30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}
}

// serveBackground 把 HTTP 服务丢到后台 goroutine（gui 版主线程要给本地窗口让位）。
func (a *app) serveBackground() {
	a.netMu.Lock()
	defer a.netMu.Unlock()
	if a.ln == nil {
		return
	}
	if a.srv != nil {
		return // 已经在服务了（重复调用多半是配置重绑定后又调了一次）
	}
	a.srv = a.server()
	ln := a.ln
	go func() {
		if err := a.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("服务退出: %v", err)
		}
	}()
}

// serveNow 阻塞在当前协程里服务（console 版用它）。
func (a *app) serveNow() error {
	a.netMu.Lock()
	ln := a.ln
	if ln == nil {
		a.netMu.Unlock()
		return errors.New("尚未绑定端口")
	}
	if a.srv == nil {
		a.srv = a.server()
	}
	srv := a.srv
	a.netMu.Unlock()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// ---------------------------------------------------------------- 视频预热

// startWarmup 预检 + 预转码全部视频：启动时、以及每次扫描完成后都会跑一轮。
// 换素材目录会先取消上一轮，免得两轮抢 CPU。
func (a *app) startWarmup() {
	a.warmMu.Lock()
	if a.warmCancel != nil {
		a.warmCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.warmCancel = cancel
	a.warmSeq++
	seq := a.warmSeq
	a.warmMu.Unlock()

	go func() {
		a.videos.WarmUp(ctx, a.cfg.vjobs)
		if ctx.Err() != nil {
			return // 被下一轮取代了，不报"就绪"
		}
		st := a.videos.Warmup()
		log.Println("==================================================")
		if seq == 1 {
			log.Printf("启动成功 —— 视频已全部就绪（共 %d 个，本次转码 %d 个）", st.Total, st.Done)
		} else {
			log.Printf("视频已全部就绪（共 %d 个，本次转码 %d 个）", st.Total, st.Done)
		}
		log.Println("==================================================")
		if a.cfg.autoOpen && !a.openedBrowser && seq == 1 {
			a.openedBrowser = true
			a.urlMu.RLock()
			u := a.reviewURL // 默认打开审阅页（用户定稿），下载页从页面里切
			a.urlMu.RUnlock()
			openBrowser(u)
		}
	}()
}

// ---------------------------------------------------------------- 运行时改配置（本地窗口用）

// cleanupTemp 清空可再生的临时产物：视频转码产物 + 图片预览缓存（用户要求：关闭时自动清理）。
// 只删这些**可再生的缓存**，绝不动 medreview.db（审阅记录）和素材（全程只读）。
// 转码产物反正下次启动也会被 PrepareCache 清掉，关的时候清掉只是让磁盘早点腾出来。
func (a *app) cleanupTemp() {
	a.cleanupOnce.Do(func() {
		log.Printf("正在清理临时缓存（视频转码产物 / 图片预览缓存）...")
		if err := a.videos.Purge(); err != nil {
			log.Printf("清理视频转码缓存失败: %v", err)
		}
		if err := a.images.Purge(); err != nil {
			log.Printf("清理图片预览缓存失败: %v", err)
		}
		log.Printf("临时缓存已清理")
	})
}

// SetRoot 换素材根目录并重新扫描。
//
// 不需要重启服务：Store 只是换个根路径指针，Scanner 重扫一遍并把新路径持久化到
// meta（下次启动默认用它）。旧的标记还在库里，同路径会按 size/mtime 对上。
func (a *app) SetRoot(p string) error {
	if p == "" {
		return errors.New("目录为空")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return err
	}
	if st, serr := os.Stat(abs); serr != nil || !st.IsDir() {
		return fmt.Errorf("不是有效目录: %s", abs)
	}
	a.store.SetRoot(abs)
	log.Printf("素材根目录已切换: %s", abs)
	if err := a.scanner.Start(abs); err != nil {
		return err
	}
	return nil
}

// SetToken 换下载口令。
//
// 不需要重启：Handler 里的口令字段换掉即可，带 ?t= 的下载页地址跟着重算。
// 已经在浏览器里打开过的页面用的是旧口令（cookie/地址里的），需要重新打开新地址。
func (a *app) SetToken(t string) error {
	t = strings.TrimSpace(t)
	if t == "" {
		return errors.New("口令不能为空")
	}
	a.netMu.Lock()
	defer a.netMu.Unlock()

	a.token = t
	a.h.setToken(t)
	a.computeURLsLocked()
	a.publishURLsLocked()
	log.Printf("下载口令已更换: %s", t)
	return nil
}

// NewToken 随机换一个新口令（本地窗口的「换一个」按钮）。
func (a *app) NewToken() (string, error) {
	t := randomToken()
	if err := a.SetToken(t); err != nil {
		return "", err
	}
	return t, nil
}

// Rebind 换监听地址（含端口），旧的连接会被掐掉。
func (a *app) Rebind(addr string) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return errors.New("地址为空")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("地址格式不对，应写成 :8080 或 0.0.0.0:8080 这样：%v", err)
	}

	a.netMu.Lock()
	defer a.netMu.Unlock()

	// 先把旧的停掉：Close 会连带关掉它自己持有的 listener
	if a.srv != nil {
		_ = a.srv.Close()
		a.srv = nil
	}
	if a.ln != nil {
		_ = a.ln.Close()
		a.ln = nil
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("无法监听 %s: %v", addr, err)
		if isAddrInUse(err) {
			return fmt.Errorf("端口已被占用，换一个（例如 :8081）")
		}
		return fmt.Errorf("监听失败: %v", err)
	}
	a.ln = ln
	a.cfg.addr = addr
	a.computeURLsLocked()
	a.publishURLsLocked()
	log.Printf("监听地址已切换: %s", addr)

	// 立刻把服务重新挂起来，不用等外部调用
	if a.srv == nil {
		a.srv = a.server()
		go func() {
			if err := a.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Printf("服务退出: %v", err)
			}
		}()
	}
	return nil
}

// status 给本地窗口用的一份快照。
type uiStatus struct {
	Root        string   `json:"root"`
	Addr        string   `json:"addr"`
	Token       string   `json:"token"`
	NoToken     bool     `json:"noToken"`
	ReviewURL   string   `json:"reviewURL"`
	DownloadURL string   `json:"downloadURL"`
	LANURLs     []string `json:"lanURLs"`
	LogPath     string   `json:"logPath"`
	FFmpeg      bool     `json:"ffmpeg"`
}

// Status 本地窗口拉状态用。
func (a *app) Status() uiStatus {
	a.urlMu.RLock()
	defer a.urlMu.RUnlock()
	lan := append([]string{}, a.lanURLs...)
	if a.lanDLURL != "" {
		lan = append(lan, a.lanDLURL)
	}
	return uiStatus{
		Root:        a.store.Root(),
		Addr:        a.cfg.addr,
		Token:       a.token,
		NoToken:     a.cfg.openDL,
		ReviewURL:   a.reviewURL,
		DownloadURL: a.downloadURL,
		LANURLs:     lan,
		LogPath:     a.logPath,
		FFmpeg:      a.videos.Available(),
	}
}
