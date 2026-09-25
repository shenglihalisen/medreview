package main

import (
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
)

type Handler struct {
	store   *Store
	scanner *Scanner
	video   *VideoService
	images  *ImageService
	hub     *Hub
	dlToken string
	noToken bool

	// --- 网页控制台用（无黑窗版没有命令行窗口，地址/口令/日志只能在网页里看）---
	logs       *logRing  // 日志环形缓冲，给 /api/console 增量拉
	reviewURL  string    // 本机审阅页地址
	downloadURL string   // 本机下载页地址（含口令）
	lanURLs    []string  // 局域网地址
	logPath    string    // 日志文件落盘路径（面板上给个「打开日志」入口）
	gui        bool      // 是不是无黑窗版（是的话才显示「停止服务」这类按钮）

	mu      sync.Mutex
	totals  Totals
	totalsT time.Time

	// 见过的批注名，给前端「最近用过…」下拉用。
	// 只放在进程内存里 —— 程序一关就没了，正好符合"刷新还在、关掉程序失效"的预期，
	// 也不占数据库、不需要清理。
	usersMu sync.Mutex
	users   []string
}

// 历史名字最多记这么多条（最近使用的排在最前）
const maxRememberedUsers = 20

// rememberUser 记下这次用到的批注名。空名字和"匿名"不入库。
func (h *Handler) rememberUser(name string) {
	name = strings.TrimSpace(name)
	if name == "" || name == "匿名" {
		return
	}
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	out := make([]string, 0, len(h.users)+1)
	out = append(out, name)
	for _, u := range h.users {
		if u != name {
			out = append(out, u)
		}
	}
	if len(out) > maxRememberedUsers {
		out = out[:maxRememberedUsers]
	}
	h.users = out
}

func (h *Handler) knownUsers() []string {
	h.usersMu.Lock()
	defer h.usersMu.Unlock()
	out := make([]string, len(h.users))
	copy(out, h.users)
	return out
}

// handleUsers 返回本进程见过的批注名（最近优先）。
// 注意是进程内存：关掉服务就清空，刷新页面/换设备仍能看到。
func (h *Handler) handleUsers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"users": h.knownUsers()})
}

type Totals struct {
	Files   int `json:"files"`
	Keep    int `json:"keep"`
	Reject  int `json:"reject"`
	Pending int `json:"pending"`
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /favicon.ico", h.handleFavicon)
	mux.HandleFunc("GET /api/events", h.hub.ServeHTTP)
	mux.HandleFunc("GET /api/status", h.handleStatus)
	mux.HandleFunc("GET /api/users", h.handleUsers)
	mux.HandleFunc("POST /api/scan", h.handleScan)
	mux.HandleFunc("GET /api/browse", h.handleBrowse)
	mux.HandleFunc("GET /api/folders", h.handleFolders)
	mux.HandleFunc("GET /api/folder", h.handleFolderInfo)
	mux.HandleFunc("GET /api/files", h.handleFiles)
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
	mux.HandleFunc("GET /api/export", h.handleExportList)
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
			http.Redirect(w, r, "/review.html", http.StatusFound)
			return
		}
		// 页面和脚本一律禁用缓存：exe 经常换代，浏览器留着上一版的 review.html
		// 却配上这一版的 app.js（或反过来），就会出现工具栏控件重复、按钮失效这类
		// 看起来像 bug 的现象 —— 刷新一次就好，但用户不知道要刷新。
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
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
// 口令可能在服务运行期间被**本地窗口**改过（app.SetToken），
// 所以这里不能直接读字段，要走加锁的 tokenOf。
func (h *Handler) canDownload(r *http.Request) bool {
	if h.noToken {
		return true
	}
	tok := h.tokenOf()
	if t := r.URL.Query().Get("t"); t != "" && t == tok {
		return true
	}
	if c, err := r.Cookie("mr_dl"); err == nil && c.Value == tok {
		return true
	}
	return false
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

func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	var t Totals
	h.mu.Lock()
	if time.Since(h.totalsT) > time.Second {
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
	t = h.totals
	h.mu.Unlock()

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
	h.mu.Lock()
	h.totalsT = time.Time{}
	h.mu.Unlock()
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

	h.mu.Lock()
	h.totalsT = time.Time{} // 与 applyDecision 一致，让 /api/status 立刻重算
	h.mu.Unlock()

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

	h.mu.Lock()
	h.totalsT = time.Time{} // 与 applyDecision 一致，让 /api/status 立刻重算
	h.mu.Unlock()

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
	err := h.store.ClaimFolder(req.FolderID, req.User)
	h.rememberUser(req.User)
	// 认领一直是"点了没反应"的头号嫌疑，这里留一行可追溯的日志：
	// rawXUser 是前端原始请求头（编码过），req.User 是解码后的名字。
	log.Printf("[claim] folder=%d user=%q rawXUser=%q err=%v", req.FolderID, req.User, r.Header.Get("X-User"), err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.hub.Broadcast("claim", map[string]any{"folderId": req.FolderID, "user": req.User})
	writeJSON(w, map[string]any{"ok": true})
}

func (h *Handler) handleRelease(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FolderID int64 `json:"folderId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	user := userOf(r)
	err := h.store.ReleaseFolder(req.FolderID, user)
	h.rememberUser(user)
	log.Printf("[claim] release folder=%d user=%q rawXUser=%q err=%v", req.FolderID, user, r.Header.Get("X-User"), err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.hub.Broadcast("claim", map[string]any{"folderId": req.FolderID, "user": ""})
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
