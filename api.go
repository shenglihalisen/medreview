package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type Handler struct {
	store   *Store
	scanner *Scanner
	video   *VideoService
	images  *ImageService
	qc      *QCManager
	hub     *Hub
	dash    *Dashboard // 仪表盘增量推送器（见 dashboard.go）
	dlToken string
	noToken bool
	// 允许 ?t=口令 走 URL 鉴权。默认关闭 —— 口令在 URL 里会进浏览器历史、
	// Referer、代理日志和截图，收益（分享链接不用先登录）远小于风险。
	// 需要分享时用一次性 share token（见 handleShareToken）。
	allowQueryToken bool

	// --- 网页控制台用（无黑窗版没有命令行窗口，地址/口令/日志只能在网页里看）---
	logs        *logRing // 日志环形缓冲，给 /api/console 增量拉
	reviewURL   string   // 本机审阅页地址
	downloadURL string   // 本机下载页地址（含口令）
	lanURLs     []string // 局域网地址
	logPath     string   // 日志文件落盘路径（面板上给个「打开日志」入口）
	gui         bool     // 是不是无黑窗版（是的话才显示「停止服务」这类按钮）

	mu      sync.Mutex
	totals  Totals
	totalsT time.Time

	// 见过的批注名现在**存数据库**（见 store.RememberUser），关掉程序也还记得。
	// 这里只留一张"上次落库时间"表，用来节流：每个请求都带 X-User，
	// 不节流的话等于每个请求写一次库。
	usersMu  sync.Mutex
	userSeen map[string]int64

	// 下载口令登录的失败节流（见 handleDLLogin）。纯内存：服务重启即清零，
	// 不落盘、不跨实例共享 —— 目的是挡"顺手打个不停"，不是做持久化的审计。
	dlFailMu   sync.Mutex
	dlFailures map[string]*dlFailRecord

	// 一次性分享 token（见 handleShareToken）。同样是纯内存、有过期、有次数上限。
	shareMu sync.Mutex
	shares  map[string]*shareToken
}

// shareToken 是一次性下载授权：换取一个只能用 N 次、T 秒后失效的短 token，
// 用来替代"把主口令塞进 URL"这种做法（口令会进历史、Referer、日志、截图）。
type shareToken struct {
	token     string
	expiresAt time.Time
	maxUses   int
	used      int
}

// dlFailRecord 记录某个来源的口令失败次数与锁定截止时间。
type dlFailRecord struct {
	count    int
	lockedUntil int64 // unix 毫秒；0 = 未锁定
}

// 历史名字最多记这么多条（最近使用的排在最前）
const maxRememberedUsers = 20

// 同一个名字多久之内不重复落库（last_used 只用来排序，精度无所谓）
const userThrottleSec = 300

// 批注名/审阅人名的长度上限。
//
// 为什么必须有上限：名字来自请求头，任何人都能通过 /api/claim 塞任意字符串进来。
// 而这个名字会进 known_user 表、进认领锁、进仪表盘的「在岗人员」列表（还要原样渲染）。
// 没有上限时，一个 5000 字符的名字就能撑爆仪表盘布局、把 DB 里 known_user 塞满。
const maxUserNameRunes = 24

// sanitizeUser 校验并规范化一个批注名，返回 (规范化后的名字, 是否合法)。
//
// 规则：
//   - 去首尾空白；
//   - 按 **rune** 计数不超过 maxUserNameRunes（按字节数截会把中文名字算成 3 倍，
//     结果是"张三丰"这种正常名字在 UTF-8 下按字节算是 9，看起来还有余量但语义错了）；
//   - 不允许含控制字符（换行、制表符、ANSI 转义序列开头）。它们在日志和页面上
//     能伪造出"多出来的一行内容"，属于典型的日志/UI 注入。
//
// 非法时返回 ok=false，让调用方回 400 —— 而不是默默截断了事。默默截断会造出
// "张三" 和 "张三丰" 被当成两个人、两个认领锁互相看不见这种极难查的问题。
func sanitizeUser(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	if len([]rune(name)) > maxUserNameRunes {
		return "", false
	}
	for _, r := range name {
		// 控制字符（unicode.IsControl 覆盖 C0/C1 两段）一律拒绝。
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return name, true
}

// rememberUser 记下这次用到的批注名（落库，关掉程序也还记得）。
// 同一个名字在 userThrottleSec 内只写一次库，避免每个请求都写。
//
// 顺带做两件事：
//  1. 校验名字（见 sanitizeUser）—— 非法名字直接忽略，不入库。这是
//     known_user 被塞垃圾的入口。
//  2. 续期该人的目录认领 —— 只要还在活动，目录就不会变成僵尸认领（见 claimRenewSec）。
func (h *Handler) rememberUser(name string) {
	name = strings.TrimSpace(name)
	if name == "" || name == "匿名" || h.store == nil {
		return
	}
	if clean, ok := sanitizeUser(name); ok {
		name = clean
	} else {
		// 非法名字：不落库、不续期，但也不报错 —— 它可能只是某个客户端
		// 传来的畸形 X-User，不该把整个请求搞挂。
		return
	}
	now := time.Now().Unix()
	h.usersMu.Lock()
	last, seen := h.userSeen[name]
	throttled := seen && now-last < userThrottleSec
	if !throttled {
		if h.userSeen == nil {
			h.userSeen = map[string]int64{}
		}
		h.userSeen[name] = now
	}
	h.usersMu.Unlock()
	if !throttled {
		if err := h.store.RememberUser(name); err != nil {
			log.Printf("记住批注名失败: %v", err)
		}
	}
	// 续期不受节流限制：只要人还在动，目录就该保持锁定。
	// 这条 UPDATE 走主键 (folder_id)，成本极低，但它每次请求都执行 ——
	// 所以只在真存在认领时才写（RenewClaims 内部先查是否存在）。
	h.store.RenewClaims(name)
}

// handleUsers 返回见过的批注名（最近优先）。存库的，重启服务不会丢。
func (h *Handler) handleUsers(w http.ResponseWriter, r *http.Request) {
	users := []string{}
	if h.store != nil {
		if u, err := h.store.KnownUsers(); err == nil {
			users = u
		}
	}
	writeJSON(w, map[string]any{"users": users})
}

type Totals struct {
	Files   int `json:"files"`
	Keep    int `json:"keep"`
	Reject  int `json:"reject"`
	Pending int `json:"pending"`
}

// totalsSnapshot 取审阅总数，带 1 秒记忆。
//
// 抽成独立函数是为了让 /api/status 和仪表盘的 totals 区块**用同一份口径**。
// 以前是两处各写一遍同样的 SQL、各自缓存，改了一处忘了另一处，页面上的数字就会
// 互相矛盾（比如 KPI 说已审 12 张、侧栏说 11 张）。
func (h *Handler) totalsSnapshot() Totals {
	h.mu.Lock()
	defer h.mu.Unlock()
	if time.Since(h.totalsT) > time.Second {
		var t Totals
		row := h.store.db.QueryRow(`SELECT
			(SELECT COUNT(*) FROM file),
			(SELECT COUNT(*) FROM review WHERE decision=1),
			(SELECT COUNT(*) FROM review WHERE decision=2)`)
		if err := row.Scan(&t.Files, &t.Keep, &t.Reject); err == nil {
			t.Pending = t.Files - t.Keep - t.Reject
			h.totals = t
			h.totalsT = time.Now()
		}
	}
	return h.totals
}

// totalsSnapshotOK 和 totalsSnapshot 同一个口径，但**如实报告这次查询有没有成功**。
//
// 为什么需要它：totalsSnapshot 在查询失败时会保留上一次的缓存值并返回
// （对 /api/status 来说"稍旧"好过直接报错）。但仪表盘区块不能这么干 ——
// 它拿到的值会被当成"刚刚算出来的真相"推给页面。一旦 DB 出问题（文件被锁、
// 磁盘满、库损坏），页面会**一直显示某个历史时刻的数字，而且看起来完全正常**，
// 用户据此判断"我刚才打的勾怎么没算进去"。静默的错数字比报错危险得多。
func (h *Handler) totalsSnapshotOK() (Totals, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if time.Since(h.totalsT) <= time.Second {
		return h.totals, nil
	}
	var t Totals
	row := h.store.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM file),
		(SELECT COUNT(*) FROM review WHERE decision=1),
		(SELECT COUNT(*) FROM review WHERE decision=2)`)
	if err := row.Scan(&t.Files, &t.Keep, &t.Reject); err != nil {
		return Totals{}, err
	}
	t.Pending = t.Files - t.Keep - t.Reject
	h.totals = t
	h.totalsT = time.Now()
	return t, nil
}

// invalidateTotals 让 1 秒记忆立刻作废。任何改动 review 表的地方都要调，
// 否则最长要等 1 秒才看到新数字。
func (h *Handler) invalidateTotals() {
	h.mu.Lock()
	h.totalsT = time.Time{}
	h.mu.Unlock()
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /favicon.ico", h.handleFavicon)
	mux.HandleFunc("GET /api/events", h.hub.ServeHTTP)
	mux.HandleFunc("GET /api/status", h.handleStatus)
	mux.HandleFunc("GET /api/dashboard", h.handleDashboard)
	mux.HandleFunc("GET /api/users", h.handleUsers)
	mux.HandleFunc("POST /api/scan", h.handleScan)
	mux.HandleFunc("GET /api/browse", h.handleBrowse)
	mux.HandleFunc("GET /api/folders", h.handleFolders)
	mux.HandleFunc("GET /api/folder", h.handleFolderInfo)
	mux.HandleFunc("GET /api/files", h.handleFiles)
	mux.HandleFunc("GET /api/qc/status", h.handleQCStatus)
	mux.HandleFunc("POST /api/review", h.handleReview)
	mux.HandleFunc("POST /api/review/batch", h.handleReviewBatch)
	// 清空「本层」目录的全部标记（不含子目录）。与 /api/review/batch 分开是因为
	// 语义不同：那个是"按 fileIds 打标记"，这个是"按目录整体重置"。
	mux.HandleFunc("POST /api/review/clear-folder", h.handleReviewClearFolder)
	// 「本目录全部保留 / 不保留」：把本层目录全部文件一次性标为保留(1)/不保留(2)。
	mux.HandleFunc("POST /api/review/folder-decision", h.handleReviewFolderDecision)
	mux.HandleFunc("POST /api/claim", h.handleClaim)
	mux.HandleFunc("POST /api/claim/release", h.handleRelease)
	// 注意：图片一律走 /api/media 输出原文件，不再有缩略图接口。
	mux.HandleFunc("GET /api/media", h.handleMedia)
	// 视频在线播放：浏览器解不了的编码（HEVC 等）先转码再给
	mux.HandleFunc("GET /api/vtrans-status", h.handleVTransStatus)
	mux.HandleFunc("GET /api/vtrans", h.handleVTrans)
	mux.HandleFunc("GET /api/vtrans-warmup", h.handleVTransWarmup)
	mux.HandleFunc("GET /api/zip", h.handleZip)
	// 入口页登录下载页：提交下载口令，比对成功后种 Cookie(mr_dl)，
	// 之后 canDownload 认 Cookie，下载页才能进/能打包。口令本身仍不下发给页面。
	mux.HandleFunc("POST /api/dl-login", h.handleDLLogin)
	// 一次性分享链接：签发（需已登录）与自助换 Cookie。
	mux.HandleFunc("POST /api/dl-share", h.handleShareToken)
	mux.HandleFunc("GET /api/dl-share-adopt", h.handleShareAdopt)
	// 健康检查：给脚本化监控用（DB 可读 / 素材根在 / 转码目录可写）。
	mux.HandleFunc("GET /api/healthz", h.handleHealthz)
	mux.HandleFunc("GET /api/export", h.handleExportList)
	mux.HandleFunc("GET /api/qc-report", h.handleQCReport)
	mux.HandleFunc("POST /api/copy", h.handleCopy)
	mux.HandleFunc("GET /api/browse-dest", h.handleBrowseDest)
	// 控制台：gui 版（medreview_ui.exe）用**本地桌面窗口**（gui_walk.go 的 runGUI）显示
	// 地址 / 口令 / 实时日志 / 停止服务，完全不依赖浏览器。所以这里不再有任何网页控制台
	// 接口 —— /api/console、/api/open-log、/api/shutdown 已删除（它们等于把本地黑窗口里的
	// 信息通过网页暴露出去，且需要口令鉴权，正是要去掉的东西）。
	// 注意：Go 的 ServeMux 里 "/"-结尾的模式是前缀匹配，
	// 所以不能写成 "GET /"（那会把所有 GET 请求都吃掉）。
	// 这里只用 "/" 兜底静态资源，根路径单独重定向。
	fsrv := http.FileServer(http.FS(webFS))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/home.html", http.StatusFound)
			return
		}
		// 页面一律禁用缓存：exe 经常换代，浏览器留着上一版的 review.html
		// 却配上这一版的 app.js（或反过来），就会出现工具栏控件重复、按钮失效这类
		// 看起来像 bug 的现象 —— 刷新一次就好，但用户不知道要刷新。
		//
		// 但**静态资源**（js/css/图标）没必要跟着禁缓存：它们占了 150KB+，
		// 每次开页面都重新传一遍，在无线/手机端就是实打实的「打开很慢」。
		// 这里给它们一个短缓存 + must revalidate：既能在几秒内拿到旧资源，
		// 又保证下次一定会问服务器有没有新版本（配合 ETag，命中就是 304 空响应）。
		// HTML 仍然 no-store —— 上面说的「控件重复/按钮失效」正是它引起的。
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		switch strings.ToLower(filepath.Ext(r.URL.Path)) {
		case ".js", ".css", ".png", ".svg", ".ico", ".webmanifest":
			w.Header().Set("Cache-Control", "public, max-age=60, must-revalidate")
		}
		fsrv.ServeHTTP(w, r)
	}))
	return corsHandler(mux)
}

// corsHandler 只对「本机 / 局域网 / 无 Origin」的请求放开跨域。
//
// 以前是 Access-Control-Allow-Origin: * —— 那等于**互联网上任意网页**都能用你的浏览器
// 去读 127.0.0.1:8080，把整个素材库（含 /api/media 的原图原片）搬走，还能跨站 POST
// /api/scan 改素材目录、清空别人的标记。这是这个工具最实的一个洞，必须堵。
//
// 仍然放开本地来源：WorkBuddy 的「应用内预览面板」是另一个端口（如 :31976）渲染页面的，
// 那时页面里的 /api/* 要跨端口打回本服务，Origin 会是 http://127.0.0.1:31976 —— 放行。
// 非浏览器的直连请求（curl / 验收脚本 / 预览面板的静态抓取）不带 Origin，同样放行。
func corsHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 安全响应头：不管来源是谁都给
		//  · nosniff：/api/media 吐的是用户素材，绝不能被浏览器嗅探成 HTML 执行
		//  · no-referrer：下载页地址带 ?t=口令，跳转下载时不要把口令从 Referer 漏出去
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")

		if origin := r.Header.Get("Origin"); origin != "" {
			if localOrigin(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-User")
				w.Header().Set("Access-Control-Max-Age", "86400")
			}
			// 不认识的来源：一个 CORS 头都不给，浏览器会直接把跨站请求挡掉
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// localOrigin 判断 Origin 是不是「本机自己」：回环、localhost，或本机的局域网地址。
// 端口不参与判断（预览面板、手机端都可能是别的端口）。
func localOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "[::1]" {
		return true
	}
	for _, ip := range cachedLANIPv4s() {
		if host == ip {
			return true
		}
	}
	return false
}

// handleFavicon 只返回 204，避免浏览器自动请求 /favicon.ico 时打出 404 红字。
// 页面 <head> 里已有 data: 空图标声明，正常不会发这个请求；这里是兜底。
func (h *Handler) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// decodeHeader 还原前端 encodeURIComponent 过的请求头值。
// HTTP 头只允许 ISO-8859-1，中文名必须先编码；这里再解回 UTF-8。
func decodeHeader(s string) string {
	if s == "" {
		return s
	}
	if d, err := url.QueryUnescape(s); err == nil {
		return d
	}
	return s
}

// userOf 从请求头或 cookie 取审阅人名字。
func userOf(r *http.Request) string {
	if u := r.Header.Get("X-User"); u != "" {
		return decodeHeader(u)
	}
	if c, err := r.Cookie("mr_user"); err == nil && c.Value != "" {
		return decodeHeader(c.Value)
	}
	return "匿名"
}

// isLoopback 判断请求是否来自本机（127.0.0.1 / ::1）。
// 「枚举全盘目录」「改审阅素材根」这类操作员功能只允许本机执行 ——
// 它们都不需要口令（审阅页要用来选目录），一旦开放给局域网，
// 同一 Wi-Fi 里的任何人就能翻遍这台电脑的目录树、还能把服务指到任意目录看图。
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host == "127.0.0.1" || host == "::1"
}

// requireLoopback 局域网请求统一拒绝，给出看得懂的中文提示。
func requireLoopback(w http.ResponseWriter, r *http.Request) bool {
	if isLoopback(r) {
		return true
	}
	http.Error(w, "选择素材目录 / 重新扫描是本机操作，请在运行服务的这台电脑上执行", http.StatusForbidden)
	return false
}

// canDownload 后端侧的下载权限校验：审阅页即使手动拼接口也会被拒。
//
// 三条通路，按强度从高到低：
//  1. -open-dl：完全关闭校验（显式声明的行为，不是默认）。
//  2. HttpOnly Cookie mr_dl：正常通路。口令由 /api/dl-login 比对后种下，
//     JS 读不到，URL 里也不出现。
//  3. 一次性 share token（?s=）：给"把下载页发给同事"用。
//     它不是主口令，泄露了也只能下载有限次、且很快作废。
//
// ⚠️ 主口令走 URL（?t=）的旧通路默认**已关闭**。口令在 URL 里会进浏览器历史、
// Referer 头、代理/服务器日志、截图和肩窥，任何一处泄露都是长期有效的高权限凭据。
// 确有需要可加 -allow-query-token 打开，但那是在 knowingly 接受这个风险。
//
// 这里的 share token 只做**校验**（存在、未过期、额度未耗尽），**不核销**。
// 核销单独放在 consumeShareQuota，只由真正把数据交出去的出口调用
// （zip / export / copy，见 zip.go、copy.go）。预览、翻页这类只是"看一眼"的操作
// 不该消耗额度 —— 否则同事点开分享链接翻两下页面，10 次额度就没了。
//
// 口令可能在服务运行期间被本地窗口改过（app.SetToken），所以走加锁的 tokenOf，
// 不能直接读字段。
func (h *Handler) canDownload(r *http.Request) bool {
	if h.noToken {
		return true
	}
	tok := h.tokenOf()
	// 常量时间比较，避免通过响应时间逐字节猜口令。
	// 长度不等也走同一个分支（先比长度只是为了尽早拒绝，不构成时序泄漏：
	// 口令长度不是秘密，且主口令是定长随机生成的）。
	if c, err := r.Cookie("mr_dl"); err == nil && len(c.Value) == len(tok) &&
		subtle.ConstantTimeCompare([]byte(c.Value), []byte(tok)) == 1 {
		return true
	}
	if h.allowQueryToken {
		if t := r.URL.Query().Get("t"); t != "" && len(t) == len(tok) &&
			subtle.ConstantTimeCompare([]byte(t), []byte(tok)) == 1 {
			return true
		}
	}
	// 分享 token 的两种带法：URL 上的 ?s=（刚点开链接时），或换成 Cookie 之后的
	// mr_dl_share（页面 JS 收到 ok:true 后种下的）。后者让 URL 里不再带凭据。
	if sc, err := r.Cookie("mr_dl_share"); err == nil && h.shareTokenValid(sc.Value) {
		return true
	}
	return h.shareTokenValid(r.URL.Query().Get("s"))
}

// shareTokenValid 只校验分享 token，不消耗额度。
func (h *Handler) shareTokenValid(tok string) bool {
	if tok == "" {
		return false
	}
	now := time.Now()
	h.shareMu.Lock()
	defer h.shareMu.Unlock()
	rec, ok := h.shares[tok]
	if !ok {
		return false
	}
	if now.After(rec.expiresAt) || rec.used >= rec.maxUses {
		delete(h.shares, tok)
		return false
	}
	return true
}

// ---------- 一次性分享 token ----------

const (
	shareTokenTTL   = time.Hour
	shareTokenUses  = 10
	shareTokenBytes = 12 // 96 bit 熵，和主口令同规格
)

// handleShareAdopt 让分享链接的接收方把一次性 token 换成 HttpOnly Cookie。
//
// 分享链接里带 ?s=xxx，同一个人点开、翻页、下载都要在 URL 里一直挂着这个 token ——
// 而 token 会随 Referer 漏给页面里引用的任何外部资源。换成 Cookie 后 URL 就干净了，
// 而且额度只在真正下载时核销（见 canDownload 顶部说明）。
func (h *Handler) handleShareAdopt(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("s")
	if !h.shareTokenValid(tok) {
		writeJSON(w, map[string]any{"ok": false, "msg": "分享链接无效或已过期"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "mr_dl_share",
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(shareTokenTTL / time.Second),
	})
	writeJSON(w, map[string]any{"ok": true})
}

// handleHealthz 健康检查。返回 200 表示一切正常，503 表示数据库或素材根不可用。
// 放在独立端点而不是只塞进仪表盘，是为了让监控脚本能一把判断，不用解析页面结构。
func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	v, err := h.calcHealth()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	m, _ := v.(map[string]any)
	code := http.StatusOK
	if ok, _ := m["ok"].(bool); !ok {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(m)
}

// handleShareToken 签发一次性下载 token。
//
// 必须已通过 canDownload（也就是得先有 Cookie 或 -open-dl），否则就成了
// "任何人都能给自己签发下载凭据"。返回的 token 只在 URL 里短暂有效，
// 用满次数或过期即失效，且不能用来再签发新的。
func (h *Handler) handleShareToken(w http.ResponseWriter, r *http.Request) {
	if !h.canDownload(r) {
		http.Error(w, "请先登录下载页", http.StatusForbidden)
		return
	}
	buf := make([]byte, shareTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		http.Error(w, "无法生成随机 token", http.StatusInternalServerError)
		return
	}
	t := hex.EncodeToString(buf)
	exp := time.Now().Add(shareTokenTTL)
	h.shareMu.Lock()
	if h.shares == nil {
		h.shares = map[string]*shareToken{}
	}
	h.gcSharesLocked(time.Now())
	h.shares[t] = &shareToken{token: t, expiresAt: exp, maxUses: shareTokenUses}
	h.shareMu.Unlock()
	rel := shareTokenTTL / time.Hour
	writeJSON(w, map[string]any{
		"token": t, "expiresAt": exp.Unix(), "maxUses": shareTokenUses,
		"url": "/download.html?s=" + t,
		"note": fmt.Sprintf("此链接最多可用 %d 次、%d 小时后失效；不要转发给无关的人", shareTokenUses, rel),
	})
}

// consumeShareQuota 核销一次分享额度。**只在真正把数据交出去的出口调用**
// （/api/zip、/api/export、/api/copy）—— 见 canDownload 顶部关于"校验 vs 核销"的说明。
//
// 调用方必须已经过了 canDownload；这里只处理 share token 这一条通路
// （走 Cookie 或主口令的请求本来就不限次，不该被这里误伤）。
func (h *Handler) consumeShareQuota(r *http.Request) {
	tok := r.URL.Query().Get("s")
	if tok == "" {
		return
	}
	now := time.Now()
	h.shareMu.Lock()
	defer h.shareMu.Unlock()
	rec, ok := h.shares[tok]
	if !ok {
		return
	}
	if now.After(rec.expiresAt) {
		delete(h.shares, tok)
		return
	}
	if rec.used >= rec.maxUses {
		delete(h.shares, tok) // 次数用尽即作废，不给"下次再来"留机会
		return
	}
	rec.used++
}

// gcSharesLocked 清理过期条目。调用方必须已持有 shareMu。
func (h *Handler) gcSharesLocked(now time.Time) {
	for k, v := range h.shares {
		if now.After(v.expiresAt) {
			delete(h.shares, k)
		}
	}
}

// tokenOf / setToken 读写下载口令。
// 本地窗口可以在运行时换口令（见 app.go 的 SetToken），并发读写要有锁。
func (h *Handler) tokenOf() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dlToken
}

func (h *Handler) setToken(t string) {
	h.mu.Lock()
	h.dlToken = t
	h.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// handleDLLogin 校验下载口令：成功种 Cookie(mr_dl)，失败返回 ok:false。
// Cookie 走 canDownload 已有的校验分支，不需要改动打包下载的任何逻辑。
// Cookie 设 HttpOnly + SameSite=Lax：本机同源使用，且不给 JS 读（避免页面把口令取走）。
//
// 两道加固（针对"攻击者反复试口令 / 跨站诱导提交"）：
//  1. Origin 白名单：非本机/局域网来源的带 Origin 请求直接 403，
//     挡掉恶意网页用表单诱导浏览器 POST（CSRF）。无 Origin 的（curl、同源某些情况）放行。
//  2. 失败节流：同一来源连续错 5 次锁 30 秒，返回 Retry-After。
//     口令本身是 96bit 随机（randomToken），爆破不现实；节流是为了不让这个端点
//     变成"随便打"的探测口，也让 CPU 打满变得困难。
func (h *Handler) handleDLLogin(w http.ResponseWriter, r *http.Request) {
	if h.noToken {
		// 没启用口令：直接放行，种一个空 cookie 也无意义，返回 ok 让前端进
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	// —— 加固1：Origin 白名单 ——
	if origin := r.Header.Get("Origin"); origin != "" && !localOrigin(origin) {
		http.Error(w, "跨站请求不被允许", http.StatusForbidden)
		return
	}
	// —— 加固2：失败节流 ——
	key := loginThrottleKey(r)
	if wait := h.dlLockedFor(key); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSON(w, map[string]any{"ok": false, "msg": fmt.Sprintf("尝试过多，请 %d 秒后再试", int(wait.Seconds())+1)})
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求格式不对", http.StatusBadRequest)
		return
	}
	tok := h.tokenOf()
	// 比对：不要用 == 直接比（时序侧信道），长度不等也要走到同一分支
	if len(req.Token) != len(tok) || subtle.ConstantTimeCompare([]byte(req.Token), []byte(tok)) != 1 {
		h.dlNoteFailure(key)
		writeJSON(w, map[string]any{"ok": false, "msg": "口令不正确"})
		return
	}
	h.dlClearFailures(key)
	http.SetCookie(w, &http.Cookie{
		Name:     "mr_dl",
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, map[string]any{"ok": true})
}

// loginThrottleKey 用来源 IP 做节流键。取 RemoteAddr 的 host 部分，
// 剥掉端口（IPv6 形如 [::1]:1234）。取不到就退回一个固定键（宁可一起锁，也不放过）。
func loginThrottleKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

const (
	dlFailMax    = 5              // 连续错 5 次就锁
	dlLockWindow = 30 * time.Second // 锁 30 秒
)

// dlLockedFor 返回还需等待多久（0 = 未锁定）。
func (h *Handler) dlLockedFor(key string) time.Duration {
	h.dlFailMu.Lock()
	defer h.dlFailMu.Unlock()
	if h.dlFailures == nil {
		return 0
	}
	rec, ok := h.dlFailures[key]
	if !ok || rec.lockedUntil == 0 {
		return 0
	}
	left := time.Until(time.UnixMilli(rec.lockedUntil))
	if left <= 0 {
		// 锁到期：清掉这个键，下一次重新计数
		delete(h.dlFailures, key)
		return 0
	}
	return left
}

// dlNoteFailure 记一次失败；达到阈值就锁。锁定期内的再次失败不重复延长锁定。
func (h *Handler) dlNoteFailure(key string) {
	h.dlFailMu.Lock()
	defer h.dlFailMu.Unlock()
	if h.dlFailures == nil {
		h.dlFailures = map[string]*dlFailRecord{}
	}
	rec, ok := h.dlFailures[key]
	if !ok {
		rec = &dlFailRecord{}
		h.dlFailures[key] = rec
	}
	// 还在锁定期内：不动（不让攻击者靠持续失败无限延长锁定）
	if rec.lockedUntil > time.Now().UnixMilli() {
		return
	}
	rec.count++
	if rec.count >= dlFailMax {
		rec.lockedUntil = time.Now().Add(dlLockWindow).UnixMilli()
		rec.count = 0
	}
}

// dlClearFailures 登录成功：清空该来源的失败记录。
func (h *Handler) dlClearFailures(key string) {
	h.dlFailMu.Lock()
	defer h.dlFailMu.Unlock()
	delete(h.dlFailures, key)
}

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	t := h.totalsSnapshot()

	// 注意两件事：
	//  1. token 本身不回传给页面，否则审阅页能拿到就等于没有权限校验；
	//  2. root 只给目录名，不给绝对路径 —— 磁盘布局属于本机控制台的信息，
	//     没必要让每个打开页面的人（包括手机端）看到 D:\某某\某某 这种完整路径。
	writeJSON(w, map[string]any{
		"root":        rootLabel(h.store.Root()),
		"scan":        h.scanner.Progress(),
		"totals":      t,
		"canDownload": h.canDownload(r),
		"gui":         h.gui,
	})
}

// handleDashboard 给管理仪表盘用的**全量**快照。
//
// 现在它只是增量体系的一个兜底入口：正常情况下页面在 SSE 建连时就拿到全量了，
// 之后只收 `dash` 事件（单区块）。这个接口保留的价值有三个：
//   - 页面首屏在 SSE 尚未建好时也能渲染（不出现空白）；
//   - 断线重连后可以重新对账；
//   - 验收脚本 / 外部监控可以一次性拉全。
//
// 各区块的计算全部走 dashboard.go 的 calc*，保证「HTTP 拉的」和「SSE 推的」
// 是同一套代码算出来的，不会两套口径。
func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.dash.Full())
}

func (h *Handler) handleScan(w http.ResponseWriter, r *http.Request) {
	if !requireLoopback(w, r) {
		return
	}
	var req struct {
		Root string `json:"root"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Root == "" {
		http.Error(w, "缺少 root", http.StatusBadRequest)
		return
	}
	if err := h.scanner.Start(req.Root); err != nil {
		if errors.Is(err, errNotDir) {
			http.Error(w, "路径不是目录或不存在", http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	abs, _ := filepath.Abs(req.Root)
	// 换素材目录 = 换了另一批文件，旧转码缓存全部作废
	if old := h.store.Root(); old != "" && old != abs {
		if h.video != nil {
			if err := h.video.Purge(); err != nil {
				log.Printf("清理转码缓存失败: %v", err)
			}
		}
		if h.images != nil {
			if err := h.images.Purge(); err != nil {
				log.Printf("清理图片预览缓存失败: %v", err)
			}
		}
	}
	h.store.SetRoot(abs)
	writeJSON(w, map[string]any{"ok": true})
}

// handleBrowse 让页面在服务端侧挑选素材根目录（浏览器拿不到本机绝对路径）。
func (h *Handler) handleBrowse(w http.ResponseWriter, r *http.Request) {
	if !requireLoopback(w, r) {
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		drives := []map[string]string{}
		for _, d := range "CDEFGHIJKLMNOPQRSTUVWXYZ" {
			root := string(d) + ":\\"
			if _, err := os.Stat(root); err == nil {
				drives = append(drives, map[string]string{"name": root, "path": root})
			}
		}
		writeJSON(w, map[string]any{"path": "", "dirs": drives})
		return
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		http.Error(w, "无法读取目录: "+err.Error(), http.StatusBadRequest)
		return
	}
	dirs := []map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "$RECYCLE.BIN" || name == "System Volume Information" {
			continue
		}
		dirs = append(dirs, map[string]string{"name": name, "path": filepath.Join(p, name)})
	}
	parent := filepath.Dir(p)
	if parent == p {
		parent = ""
	}
	writeJSON(w, map[string]any{"path": p, "parent": parent, "dirs": dirs})
}

func (h *Handler) handleFolders(w http.ResponseWriter, r *http.Request) {
	parentID, _ := strconv.ParseInt(r.URL.Query().Get("parent"), 10, 64)
	list, err := h.store.SubFolders(parentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"folders": list})
}

func (h *Handler) handleFolderInfo(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("folder"), 10, 64)
	if id <= 0 {
		id = 1
	}
	n, err := h.store.FolderByID(id)
	if err != nil {
		http.Error(w, "目录不存在", http.StatusNotFound)
		return
	}
	// 本层已标记数 / 其中本人标的数：给「清空本目录标记」的确认弹窗做预览。
	// 只作用本层 → 用 folder_id 直查、不递归；markedMine 依赖 X-User。
	marked, markedMine, _ := h.store.FolderMarkCounts(id, userOf(r))
	files, _ := h.store.FolderFileCount(id)
	writeJSON(w, map[string]any{"folder": n, "marked": marked, "markedMine": markedMine, "files": files})
}

// handleQCStatus 返回某目录「本层」质量自动检测的汇总计数（重复/损坏/空镜/镜头脏污）。
// 前端在工具栏/侧栏角标展示；与 /api/files 里的逐文件 qc 互补：这里给汇总，那里给明细。
func (h *Handler) handleQCStatus(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("folder"), 10, 64)
	if id <= 0 {
		id = 1
	}
	st, err := h.store.QCStats(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 直接回结构体：字段由 QCStat 的 json tag 决定，加检测器时不用再改这里
	// （之前手写成 5 个 key，加了模糊/曝光/噪点后忘记同步，接口就悄悄少三个字段）。
	writeJSON(w, st)
}

func (h *Handler) handleFiles(w http.ResponseWriter, r *http.Request) {
	folderID, _ := strconv.ParseInt(r.URL.Query().Get("folder"), 10, 64)
	if folderID <= 0 {
		folderID = 1
	}
	afterID, _ := strconv.ParseInt(r.URL.Query().Get("afterId"), 10, 64)
	afterName := r.URL.Query().Get("afterName")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 300 {
		limit = 60
	}
	filter := r.URL.Query().Get("filter")

	items, err := h.store.FilePage(folderID, afterName, afterID, limit, filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 「含父目录」：把上一层的直属文件接在本层后面一起显示（只追加，不改任何标记/打包语义）。
	// 只在第一页（还没翻页）时追加，否则翻页会把同一批父目录文件重复塞进来。
	withParent := r.URL.Query().Get("withParent") == "1"
	parentID := int64(0)
	if withParent && afterID == 0 {
		if pid, perr := h.store.ParentIDOf(folderID); perr == nil && pid > 0 {
			parentID = pid
			up, uerr := h.store.FilePage(pid, "", 0, 200, filter)
			if uerr == nil {
				for i := range up {
					up[i].FromParent = true
				}
				items = append(items, up...)
			}
		}
	}

	// 进入目录 / 翻页时，后台预热这一层的图片预览（缓存命中的直接跳过）。
	// goroutine 让响应先回去，预热不阻塞文件列表。
	if h.images != nil {
		if parentID > 0 {
			go h.images.WarmFolders(folderID, parentID)
		} else {
			go h.images.WarmFolder(folderID)
		}
	}
	// 同一目录的质量自动检测也在后台触发（已算过的按 size/mtime 跳过，几乎零成本）。
	if h.qc != nil {
		go h.qc.WarmFolder(folderID)
	}
	writeJSON(w, map[string]any{"files": items})
}

func (h *Handler) handleReview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FileID   int64 `json:"fileId"`
		Decision int   `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	h.applyDecision(w, r, []int64{req.FileID}, req.Decision)
}

func (h *Handler) handleReviewBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FileIDs  []int64 `json:"fileIds"`
		Decision int     `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	h.applyDecision(w, r, req.FileIDs, req.Decision)
}

func (h *Handler) applyDecision(w http.ResponseWriter, r *http.Request, ids []int64, decision int) {
	if decision < 0 || decision > 2 {
		http.Error(w, "decision 只能是 0/1/2", http.StatusBadRequest)
		return
	}
	user := userOf(r)
	h.rememberUser(user)
	// 认领锁：只有认领者本人能改这个目录里的文件。
	//
	// 结果分四类计数，返回体全部给出 —— 以前只返回 changed，
	// 前端把 changed==0 一律当成"被他人认领"，但 changed==0 还有另一个
	// 完全正常的含义：这张图本来就已经是目标状态（SetDecision 返回 ok=false）。
	// 那是个高频操作（重复点 ✓、批量按钮圈住的几张已全标过），
	// 误报成"被他人认领"让人莫名其妙 —— 见 check_keep_scope.py 第 6 组的回归断言。
	changed, blocked, same, missing := 0, 0, 0, 0
	for _, id := range ids {
		f, err := h.store.FileByID(id)
		if err != nil {
			missing++
			continue
		}
		if !h.canEdit(f, user) {
			blocked++
			continue
		}
		ok, err := h.store.SetDecision(id, decision, user)
		if err != nil {
			log.Printf("写审阅状态失败 id=%d: %v", id, err)
			missing++
			continue
		}
		if ok {
			changed++
			// folderId 必须是数字 id：前端拿它和 S.folderId（数字）比，
			// 塞 RelPath 的话 String() 比较永远不等，实时同步就白广播了。
			h.hub.Broadcast("review", map[string]any{
				"fileId":   id,
				"decision": decision,
				"reviewer": user,
				"folderId": f.FolderID,
			})
		} else {
			same++ // 目标状态与现状相同，无需写 —— 这不是被锁，绝不能报"被他人认领"
		}
	}
	h.invalidateTotals()
	h.dash.Mark(SecTotals)
	// changed 保留：老调用方只看它。blocked/same/missing 给前端区分"为什么没变"。
	// 三个批量按钮的 ids 全部来自当前目录（本层），认领锁按"文件直接所属目录"判，
	// 所以 blocked 要么 0 要么等于总数，前端按计数分支就够，不需要明细列表。
	writeJSON(w, map[string]any{
		"changed": changed, "blocked": blocked, "same": same, "missing": missing,
	})
}

// handleReviewClearFolder 清空「本层」目录里全部文件的标记。
//
// 范围严格等于 GET /api/files?folder=N —— 只本层，不递归子目录。
// 认领锁只需要查一次：只作用于本层时，同一个 folder_id 的认领结论是统一的
// （要么整体被别人认领，要么整体放行），不存在"目录内一半能改一半不能改"，
// 所以不必像 applyDecision 那样逐文件 canEdit，也不会出现"清一半"。
//
// 不可逆：review 行被删掉后没有任何地方能还原。前端的确认弹窗会写明本层
// 有多少个已被标记、其中几个是别人标的，并要求勾选后才能提交。
func (h *Handler) handleReviewClearFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FolderID int64 `json:"folderId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FolderID <= 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if ok, err := h.store.FolderExists(req.FolderID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if !ok {
		http.Error(w, "目录不存在", http.StatusNotFound)
		return
	}

	user := userOf(r)
	owner, err := h.store.ClaimOf(req.FolderID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 认领锁：语义完全对齐 canEdit —— 认领人本人放行、无人认领放行、别人 403。
	// 注意：-token / 下载权限在这里不生效（canEdit 从来不认它），保持一致，
	// 否则"只读的下载页"就能改掉别人正在审的目录，那是行为倒退。
	if owner != "" && owner != user {
		http.Error(w, fmt.Sprintf("该目录已被 %s 认领，只能查看", owner), http.StatusForbidden)
		return
	}
	h.rememberUser(user)

	marked, mine, _ := h.store.FolderMarkCounts(req.FolderID, user) // 删之前取，仅作预览
	changed, err := h.store.ClearFolderReview(req.FolderID)         // 单条原子 DELETE
	if err != nil {
		log.Printf("清空目录标记失败 folder=%d: %v", req.FolderID, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.invalidateTotals() // 与 applyDecision 一致，让 /api/status 立刻重算
	h.dash.Mark(SecTotals)

	// 聚合广播：一条事件代表"整个目录被清空"，绝不逐文件广播。
	// 这条事件是幂等的终态描述，重复收到无副作用；即使被丢掉，
	// 客户端下次 reload/resync 会从服务端重读对齐 —— 丢事件不丢状态。
	h.hub.Broadcast("folder-clear", map[string]any{
		"folderId": req.FolderID, // 真实数字 id（别学上面 review 事件那样塞 RelPath）
		"changed":  changed,
		"user":     user,
	})
	writeJSON(w, map[string]any{"changed": changed, "marked": marked, "mine": mine})
}

// handleReviewFolderDecision 把「本层」目录的全部文件一次性标为 保留(1)/不保留(2)。
// 与 clear-folder 同款把关：目录存在 + 认领锁（认领人本人 / 无人认领才放行）；
// 本层语义与清空本目录一致 —— 不含子目录。
func (h *Handler) handleReviewFolderDecision(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FolderID int64 `json:"folderId"`
		Decision int   `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.FolderID <= 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Decision != 1 && req.Decision != 2 {
		http.Error(w, "decision 只能是 1(保留) 或 2(不保留)", http.StatusBadRequest)
		return
	}
	if ok, err := h.store.FolderExists(req.FolderID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if !ok {
		http.Error(w, "目录不存在", http.StatusNotFound)
		return
	}

	user := userOf(r)
	owner, err := h.store.ClaimOf(req.FolderID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if owner != "" && owner != user {
		http.Error(w, fmt.Sprintf("该目录已被 %s 认领，只能查看", owner), http.StatusForbidden)
		return
	}
	h.rememberUser(user)

	changed, err := h.store.SetFolderReview(req.FolderID, req.Decision, user)
	if err != nil {
		log.Printf("整目录标记失败 folder=%d decision=%d: %v", req.FolderID, req.Decision, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	h.invalidateTotals() // 与 applyDecision 一致，让 /api/status 立刻重算
	h.dash.Mark(SecTotals)

	// 聚合广播：一条事件代表"整个目录被标为 X"，绝不逐文件广播（几十上百条会刷爆 SSE）。
	h.hub.Broadcast("folder-decision", map[string]any{
		"folderId": req.FolderID,
		"decision": req.Decision,
		"changed":  changed,
		"user":     user,
	})
	writeJSON(w, map[string]any{"changed": changed})
}

// canEdit 认领锁校验：目录被别人认领后，其他人只读。
func (h *Handler) canEdit(f *FileItem, user string) bool {
	var owner string
	err := h.store.db.QueryRow(`SELECT c.user_name FROM claim c
		JOIN file f ON f.folder_id = c.folder_id WHERE f.id = ?`, f.ID).Scan(&owner)
	if err != nil || owner == "" || owner == user {
		return true
	}
	return false
}

// claimRenewSec 认领续期窗口：超过这个时间没有任何动作，claim 就算僵尸（见 staleClaimMs）。
//
// 以前 claim 一旦写入就永远有效，人临时走开目录就一直被锁着，只能手动释放。
// 现在每次带 X-User 的请求都会顺手续期（见 rememberUser → RenewClaims），
// 目录会随人的实际活动保持/失去锁定，不用人工干预。
const claimRenewSec = 30 * 60

func (h *Handler) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FolderID int64  `json:"folderId"`
		User     string `json:"user"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.User == "" {
		req.User = userOf(r)
	}
	name, ok := sanitizeUser(req.User)
	if !ok {
		http.Error(w, "名字不合法：最多 24 个字符，且不能只含空格或控制字符", http.StatusBadRequest)
		return
	}
	ok2, err := h.store.ClaimFolder(req.FolderID, name)
	h.rememberUser(name)
	// 认领一直是"点了没反应"的头号嫌疑，这里留一行可追溯的日志：
	// rawXUser 是前端原始请求头（编码过），req.User 是解码后的名字。
	log.Printf("[claim] folder=%d user=%q rawXUser=%q ok=%v err=%v", req.FolderID, name, r.Header.Get("X-User"), ok2, err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok2 {
		// 已被他人认领：明确告诉前端（前端提示"已被 X 认领"），
		// 而不是静默成功 —— 静默会让两个人都以为自己拿到了这个目录。
		owner, _ := h.store.ClaimOf(req.FolderID)
		writeJSON(w, map[string]any{"ok": false, "owner": owner, "msg": "该目录已被 " + owner + " 认领"})
		return
	}
	h.hub.Broadcast("claim", map[string]any{"folderId": req.FolderID, "user": name})
	h.dash.Mark(SecFolders, SecWorkers)
	writeJSON(w, map[string]any{"ok": true})
}

func (h *Handler) handleRelease(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FolderID int64 `json:"folderId"`
		Force    bool  `json:"force"` // true = 强制释放别人的僵尸认领
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	user := userOf(r)
	// 强制释放是给仪表盘用的：只允许摘掉**僵尸**认领（超过 staleClaimMs 无动作），
	// 否则任何人都能抢走一个正在被审的目录 —— 那等于绕过了整个认领锁。
	if req.Force {
		ok, owner, err := h.store.ForceReleaseStale(req.FolderID, staleClaimMs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			writeJSON(w, map[string]any{"ok": false, "msg": "该目录的认领还" + "在活动期内，不能强制释放"})
			return
		}
		log.Printf("[claim] force-release folder=%d by=%q previousOwner=%q", req.FolderID, user, owner)
		h.hub.Broadcast("claim", map[string]any{"folderId": req.FolderID, "user": ""})
		h.dash.Mark(SecFolders, SecWorkers)
		writeJSON(w, map[string]any{"ok": true, "released": owner})
		return
	}
	err := h.store.ReleaseFolder(req.FolderID, user)
	h.rememberUser(user)
	log.Printf("[claim] release folder=%d user=%q rawXUser=%q err=%v", req.FolderID, user, r.Header.Get("X-User"), err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.hub.Broadcast("claim", map[string]any{"folderId": req.FolderID, "user": ""})
	h.dash.Mark(SecFolders, SecWorkers)
	writeJSON(w, map[string]any{"ok": true})
}

// handleMedia 输出原文件。交给 http.ServeFile 处理 Range，
// 这样 <video> 拖动进度条会走 206 Partial Content，不会整段重下。
func (h *Handler) handleMedia(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if id <= 0 {
		http.Error(w, "缺少 id", http.StatusBadRequest)
		return
	}
	f, err := h.store.FileByID(id)
	if err != nil {
		http.Error(w, "文件不存在", http.StatusNotFound)
		return
	}
	p := h.store.absPath(f.RelPath)
	if _, err := os.Stat(p); err != nil {
		http.Error(w, "源文件已不在磁盘上", http.StatusNotFound)
		return
	}
	// 图片预览：普通预览请求（不带 dl / orig）给低清转码版 —— 原图动辄好几 MB，
	// 打开一个目录拉几十 MB 太慢。orig=1 是灯箱「看原图」用的，给原文件。
	// Preview 失败时内部回退原文件（orig=true），落到下面的 ServeFile，页面永远不会挂。
	if f.Kind == kindImage && h.images != nil &&
		r.URL.Query().Get("dl") != "1" && r.URL.Query().Get("orig") != "1" {
		if pp, orig, perr := h.images.Preview(r.Context(), f); perr == nil && !orig {
			http.ServeFile(w, r, pp)
			return
		}
	}
	// dl=1 才是"下载"。这里额外要求文件被明确标记为「保留」——
	// 否则手拼 URL 就能把「不保留」和「从没标过」的文件拿走，绕过打包口径。
	// 注意：不带 dl 的预览（上面那段 os.Stat 之后直接 ServeFile）绝不能加这道闸，
	// 否则审阅页就看不到「不保留」的图了，审阅流程直接废掉。
	if r.URL.Query().Get("dl") == "1" {
		if !h.canDownload(r) {
			http.Error(w, "当前页面无下载权限", http.StatusForbidden)
			return
		}
		// 单文件下载也是出口，分享额度在这里核销。
		h.consumeShareQuota(r)
		if f.Decision != decisionKeep {
			http.Error(w, "该文件没有标记为「保留」，不能下载", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename="medreview.bin"; filename*=UTF-8''%s`, urlEscape(f.Name)))
	}
	http.ServeFile(w, r, p)
}

func urlEscape(s string) string {
	const upper = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upper[c>>4])
		b.WriteByte(upper[c&15])
	}
	return b.String()
}

// webFS 由 main.go 通过 go:embed 注入
var webFS fs.FS
