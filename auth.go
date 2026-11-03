package main

// 公网模式的内置认证（2026-11-03）
//
// 背景：medreview 的审阅接口本来就没有鉴权（能看图才能标记），这在局域网里
// 是产品特性，挂到公网就是灾难。这个文件提供一个"够用且不复杂"的认证层：
//
//   - 账号：启动参数 -auth user:pass（单账号），或 -auth-file users.json（多账号，bcrypt）
//   - 会话：HMAC-SHA256 签名的 Cookie（HttpOnly / SameSite=Lax / 可选 Secure）
//   - 防护：登录失败按 IP 限流；写操作校验 Origin（CSRF 兜底）
//   - 审计：JSONL 追加到 -audit-log（时间/IP/用户/动作/结果）
//
// 刻意不做的事：不引入 OAuth / 用户管理界面 / 密码找回。要这些请上反向代理
// （Authelia、OAuth2-Proxy），本层只保证"直接暴露端口也不至于裸奔"。

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "mr_sess"
	sessionTTL    = 12 * time.Hour
	loginMaxFail  = 5                // 15 分钟窗口内最多失败次数
	loginWindow   = 15 * time.Minute //
)

// authConfig 认证配置。Enabled=false 时整个认证层不生效（默认行为不变）。
type authConfig struct {
	Enabled    bool
	Users      map[string]string // 用户名 → bcrypt 哈希
	Admins     map[string]bool   // 超管名单（可管理其他账号）
	SessionKey []byte
	AuditPath  string
	Secure     bool   // 只走 HTTPS 时置 true（Cookie 加 Secure）
	UsersPath  string // -auth-file 的路径；非空才支持在网页里改密码（要写回）
}

// authState 运行期状态：登录失败计数 + 审计文件句柄。
type authState struct {
	mu       sync.Mutex
	usersMu  sync.Mutex // 保护 authCfg.Users 的读写（网页改密码会写）
	fails    map[string]*failRec
	auditMu sync.Mutex
	audit   *os.File
}

type failRec struct {
	n     int
	first time.Time
}

// newAuthState 初始化认证状态，并打开审计文件（失败不致命，只打日志）。
func newAuthState(cfg authConfig) *authState {
	st := &authState{fails: map[string]*failRec{}}
	if cfg.AuditPath != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.AuditPath), 0o755); err == nil {
			if f, err := os.OpenFile(cfg.AuditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
				st.audit = f
			} else {
				log.Printf("审计日志打不开（%s）：%v", cfg.AuditPath, err)
			}
		}
	}
	return st
}

// logAudit 追加一条 JSONL 审计记录（失败静默：审计不能影响主流程）。
func (st *authState) logAudit(r *http.Request, user, action, result string) {
	if st.audit == nil {
		return
	}
	rec := map[string]any{
		"ts":     time.Now().Format(time.RFC3339),
		"ip":     clientIP(r),
		"user":   user,
		"action": action,
		"result": result,
		"ua":     r.UserAgent(),
	}
	b, _ := json.Marshal(rec)
	st.auditMu.Lock()
	st.audit.Write(append(b, '\n'))
	st.auditMu.Unlock()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- 会话签名 ----------

// signSession 生成 "base64(payload).hex(hmac)"。
func signSession(key []byte, user string, exp time.Time) string {
	payload := fmt.Sprintf("%s|%d", user, exp.Unix())
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + hex.EncodeToString(mac.Sum(nil))
}

// verifySession 校验签名与有效期，返回用户名；失败返回空串。
func verifySession(key []byte, raw string) string {
	parts := strings.SplitN(raw, ".", 2)
	if len(parts) != 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	want := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[1])) != 1 {
		return ""
	}
	f := strings.SplitN(string(payload), "|", 2)
	if len(f) != 2 {
		return ""
	}
	var exp int64
	if _, err := fmt.Sscanf(f[1], "%d", &exp); err != nil || time.Now().Unix() > exp {
		return ""
	}
	return f[0]
}

// ---------- 限流 ----------

// allowLogin 判断这个 IP 还能不能再试。窗口过期自动清零。
func (st *authState) allowLogin(ip string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	rec, ok := st.fails[ip]
	if !ok || time.Since(rec.first) > loginWindow {
		return true
	}
	return rec.n < loginMaxFail
}

// noteLogin 记录一次登录结果。
func (st *authState) noteLogin(ip string, ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if ok {
		delete(st.fails, ip)
		return
	}
	rec, exists := st.fails[ip]
	if !exists || time.Since(rec.first) > loginWindow {
		st.fails[ip] = &failRec{n: 1, first: time.Now()}
		return
	}
	rec.n++
}

// ---------- 请求侧 ----------

// sessionUser 从请求里取当前登录用户（未登录返回空串）。
func (h *Handler) sessionUser(r *http.Request) string {
	if h.auth == nil || !h.authCfg.Enabled {
		return ""
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return verifySession(h.authCfg.SessionKey, c.Value)
}

// authEnabled 认证是否生效。
func (h *Handler) authEnabled() bool { return h.authCfg.Enabled }

// authExempt 免认证路径：登录页/登录接口/健康检查/静态图标样式。
// 只放行"登录前必须能看到"的东西，多一个都不给。
func authExempt(p string) bool {
	switch p {
	case "/login.html", "/api/login", "/api/health", "/favicon.ico",
		"/style.css", "/manifest.webmanifest",
		"/icon-192.png", "/icon-512.png", "/icon-maskable-512.png":
		return true
	}
	return false
}

// authWrap 认证中间件：未登录的 API 返回 401，页面重定向到登录页。
// 写操作额外校验 Origin（SameSite=Lax 之外的 CSRF 兜底）。
func (h *Handler) authWrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.authCfg.Enabled || authExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		user := h.sessionUser(r)
		if user == "" {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"未登录","login":"/login.html","api":1}`))
			} else {
				http.Redirect(w, r, "/login.html?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			}
			return
		}
		// CSRF：浏览器发起的写操作必须来自本站（Origin 缺失 = 非浏览器客户端，放行）
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					http.Error(w, "跨站请求被拒绝", http.StatusForbidden)
					h.auth.logAudit(r, user, r.Method+" "+r.URL.Path, "csrf-blocked")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// handleLogin POST /api/login {user,password}
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !h.authCfg.Enabled {
		http.Error(w, "认证未启用", http.StatusNotFound)
		return
	}
	ip := clientIP(r)
	if !h.auth.allowLogin(ip) {
		h.auth.logAudit(r, "", "login", "rate-limited")
		writeJSONStatus(w, http.StatusTooManyRequests, map[string]any{"error": "尝试过于频繁，请稍后再试"})
		return
	}
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	hash, ok := h.authCfg.Users[req.User]
	pass := []byte(req.Password)
	if !ok || bcrypt.CompareHashAndPassword([]byte(hash), pass) != nil {
		h.auth.noteLogin(ip, false)
		h.auth.logAudit(r, req.User, "login", "fail")
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "用户名或密码错误"})
		return
	}
	h.auth.noteLogin(ip, true)
	exp := time.Now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    signSession(h.authCfg.SessionKey, req.User, exp),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   h.authCfg.Secure,
		Expires:  exp,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	h.auth.logAudit(r, req.User, "login", "ok")
	h.mu.Lock()
	h.loginUser = req.User
	h.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "user": req.User})
}

// handleLogout POST /api/logout
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	user := h.sessionUser(r)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	if user != "" {
		h.auth.logAudit(r, user, "logout", "ok")
	}
	writeJSON(w, map[string]any{"ok": true})
}

// securityHeaders 公网模式额外发的安全响应头（HSTS 只在 TLS 下加）。
func (h *Handler) securityHeaders(w http.ResponseWriter, r *http.Request) {
	if !h.authCfg.Enabled {
		return
	}
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
	if r.TLS != nil || h.authCfg.Secure {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
}

// loadAuthConfig 解析启动参数里的账号配置。
//
//	-auth user:pass          单账号（启动时 bcrypt 哈希）
//	-auth-file users.json    多账号：{"users":{"alice":"$2a$..."}}
//	-session-key <hex>       Cookie 签名密钥；不填则每次启动随机（重启需重新登录）
func loadAuthConfig(single, file, keyHex, audit, adminsFlag string, secure bool) (authConfig, error) {
	cfg := authConfig{Users: map[string]string{}, Admins: map[string]bool{}, AuditPath: audit, Secure: secure, UsersPath: file}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return cfg, fmt.Errorf("读取 -auth-file 失败: %w", err)
		}
		var doc struct {
			Users  map[string]string `json:"users"`
			Admins []string          `json:"admins"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			return cfg, fmt.Errorf("解析 -auth-file 失败: %w", err)
		}
		for u, hash := range doc.Users {
			if !strings.HasPrefix(hash, "$2") {
				return cfg, fmt.Errorf("用户 %s 的密码不是 bcrypt 哈希（用 -hash-pass 生成）", u)
			}
			cfg.Users[u] = hash
		}
		for _, u := range doc.Admins {
			if _, ok := cfg.Users[u]; ok {
				cfg.Admins[u] = true
			}
		}
	}
	if single != "" {
		parts := strings.SplitN(single, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return cfg, fmt.Errorf("-auth 格式应为 user:password")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(parts[1]), 10)
		if err != nil {
			return cfg, err
		}
		cfg.Users[parts[0]] = string(hash)
	}
	// -admin name1,name2：把这些账号标成超管（建议配合 -auth-file 使用）
	for _, n := range strings.Split(adminsFlag, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, ok := cfg.Users[n]; !ok {
			return cfg, fmt.Errorf("-admin 指定的用户 %q 不存在", n)
		}
		cfg.Admins[n] = true
	}

	if len(cfg.Users) == 0 {
		return cfg, nil
	}
	cfg.Enabled = true
	if keyHex != "" {
		k, err := hex.DecodeString(keyHex)
		if err != nil || len(k) < 16 {
			return cfg, fmt.Errorf("-session-key 需为 >=32 位十六进制（16 字节）")
		}
		cfg.SessionKey = k
	} else {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			return cfg, err
		}
		cfg.SessionKey = k
		log.Println("提示：未指定 -session-key，本次启动的会话密钥是随机的，重启后需要重新登录。")
	}
	return cfg, nil
}

// ---------- 网页里的账号管理（2026-11-03）----------
// 只做"改自己的密码"：没有管理员概念，也没有增删用户的界面。
// 改完写回 -auth-file 指向的 users.json（原子替换），并记审计。

// authUserName 当前登录用户名（与 sessionUser 相同，语义化别名）。
func (h *Handler) authUserName(r *http.Request) string { return h.sessionUser(r) }

// handleAuthMe GET /api/auth/me → {user, users:[名字], canChange:bool}
func (h *Handler) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	if !h.authCfg.Enabled {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "未启用认证"})
		return
	}
	me := h.authUserName(r)
	h.auth.usersMu.Lock()
	names := make([]string, 0, len(h.authCfg.Users))
	for n := range h.authCfg.Users {
		names = append(names, n)
	}
	h.auth.usersMu.Unlock()
	sort.Strings(names)
	writeJSON(w, map[string]any{
		"user":      me,
		"users":     names,
		"canChange": h.authCfg.UsersPath != "",
		"admin":     h.isAdmin(me),
		"manage":    h.authCfg.UsersPath != "",
	})
}

// handleChangePassword POST /api/auth/password {oldPassword,newPassword}
func (h *Handler) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if !h.authCfg.Enabled {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "未启用认证"})
		return
	}
	me := h.authUserName(r)
	if me == "" {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return
	}
	if h.authCfg.UsersPath == "" {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": "当前用 -auth 单账号启动，改密码需要改用 -auth-file users.json 后重启",
		})
		return
	}
	var req struct {
		Old string `json:"oldPassword"`
		New string `json:"newPassword"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	if len([]rune(req.New)) < 8 {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "新密码至少 8 位"})
		return
	}

	h.auth.usersMu.Lock()
	hash := h.authCfg.Users[me]
	h.auth.usersMu.Unlock()
	if hash == "" || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Old)) != nil {
		h.auth.logAudit(r, me, "password-change", "fail(old)")
		writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "当前密码不正确"})
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.New), 10)
	if err != nil {
		http.Error(w, "生成失败", http.StatusInternalServerError)
		return
	}

	h.auth.usersMu.Lock()
	h.authCfg.Users[me] = string(newHash)
	snapshot := make(map[string]string, len(h.authCfg.Users))
	for n, v := range h.authCfg.Users {
		snapshot[n] = v
	}
	h.auth.usersMu.Unlock()

	if err := persistUsers(h.authCfg.UsersPath, snapshot, h.snapshotAdmins()); err != nil {
		h.auth.logAudit(r, me, "password-change", "fail(write)")
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": "写入 users.json 失败: " + err.Error()})
		return
	}
	h.auth.logAudit(r, me, "password-change", "ok")
	writeJSON(w, map[string]any{"ok": true})
}

// persistUsers 原子写回 users.json（先写 .tmp 再 rename，避免半截文件）。
// 权限固定 0600：里面虽然是哈希，也属于凭据。
func persistUsers(path string, users map[string]string, admins map[string]bool) error {
	var list []string
	for n := range admins {
		list = append(list, n)
	}
	sort.Strings(list)
	doc := map[string]any{"users": users, "admins": list}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		// Windows 上 chmod 基本无效，忽略
	}
	return os.Rename(tmp, path)
}

// ---------- 超管（2026-11-03）----------
// 规则（刻意保守）：
//   · 超管只能由 -admin 启动参数或 users.json 的 admins 指定，**没有"第一个注册者自动成超管"**
//   · 超管可增删用户、重置任意用户密码、升降权
//   · 不能删除自己；不能把最后一个超管降级或删除（避免把自己锁死）
//   · 所有动作记审计

// isAdmin 当前用户是否超管（每次请求现查，撤销立即生效）。
func (h *Handler) isAdmin(name string) bool {
	if !h.authCfg.Enabled || name == "" {
		return false
	}
	h.auth.usersMu.Lock()
	defer h.auth.usersMu.Unlock()
	return h.authCfg.Admins[name]
}

// snapshotAdmins 拷贝一份超管名单（持久化用）。
func (h *Handler) snapshotAdmins() map[string]bool {
	h.auth.usersMu.Lock()
	defer h.auth.usersMu.Unlock()
	out := make(map[string]bool, len(h.authCfg.Admins))
	for n, v := range h.authCfg.Admins {
		if v {
			out[n] = true
		}
	}
	return out
}

// requireAdmin 校验登录 + 超管身份；返回操作者名字。
func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	me := h.sessionUser(r)
	if me == "" {
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
		return "", false
	}
	if !h.isAdmin(me) {
		h.auth.logAudit(r, me, "admin", "denied")
		writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "需要超管权限"})
		return "", false
	}
	return me, true
}

// validUserName 用户名白名单：1..32 个字符，不含控制字符与冒号（避免和 user:pass 混淆）。
func validUserName(n string) bool {
	n = strings.TrimSpace(n)
	if n == "" || len([]rune(n)) > 32 {
		return false
	}
	for _, c := range n {
		if c < 0x20 || c == ':' || c == '/' || c == '\\' {
			return false
		}
	}
	return true
}

// GET /api/admin/users → [{name, admin, self}]
func (h *Handler) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	me := h.sessionUser(r)
	h.auth.usersMu.Lock()
	out := make([]map[string]any, 0, len(h.authCfg.Users))
	for n := range h.authCfg.Users {
		out = append(out, map[string]any{"name": n, "admin": h.authCfg.Admins[n], "self": n == me})
	}
	h.auth.usersMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	writeJSON(w, map[string]any{"users": out, "me": me})
}

// POST /api/admin/users {name,password,admin}
func (h *Handler) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	op, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}
	if h.authCfg.UsersPath == "" {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "需要 -auth-file 模式才能管理用户"})
		return
	}
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		Admin    bool   `json:"admin"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !validUserName(req.Name) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "用户名需 1-32 个字符，不能含冒号或斜杠"})
		return
	}
	if len([]rune(req.Password)) < 8 {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "密码至少 8 位"})
		return
	}
	h.auth.usersMu.Lock()
	if _, exists := h.authCfg.Users[req.Name]; exists {
		h.auth.usersMu.Unlock()
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "用户已存在"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), 10)
	if err != nil {
		h.auth.usersMu.Unlock()
		http.Error(w, "生成失败", http.StatusInternalServerError)
		return
	}
	h.authCfg.Users[req.Name] = string(hash)
	if req.Admin {
		h.authCfg.Admins[req.Name] = true
	}
	users := make(map[string]string, len(h.authCfg.Users))
	for n, v := range h.authCfg.Users {
		users[n] = v
	}
	admins := make(map[string]bool, len(h.authCfg.Admins))
	for n, v := range h.authCfg.Admins {
		admins[n] = v
	}
	h.auth.usersMu.Unlock()

	if err := persistUsers(h.authCfg.UsersPath, users, admins); err != nil {
		h.auth.logAudit(r, op, "admin-create "+req.Name, "fail(write)")
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": "写入 users.json 失败"})
		return
	}
	h.auth.logAudit(r, op, "admin-create "+req.Name, "ok")
	writeJSON(w, map[string]any{"ok": true})
}

// POST /api/admin/users/password {name,password} —— 超管重置任意用户密码
func (h *Handler) handleAdminResetPassword(w http.ResponseWriter, r *http.Request) {
	op, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}
	if h.authCfg.UsersPath == "" {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "需要 -auth-file 模式才能管理用户"})
		return
	}
	var req struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	if len([]rune(req.Password)) < 8 {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "密码至少 8 位"})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), 10)
	if err != nil {
		http.Error(w, "生成失败", http.StatusInternalServerError)
		return
	}
	h.auth.usersMu.Lock()
	if _, exists := h.authCfg.Users[req.Name]; !exists {
		h.auth.usersMu.Unlock()
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "用户不存在"})
		return
	}
	h.authCfg.Users[req.Name] = string(hash)
	users := make(map[string]string, len(h.authCfg.Users))
	for n, v := range h.authCfg.Users {
		users[n] = v
	}
	admins := make(map[string]bool, len(h.authCfg.Admins))
	for n, v := range h.authCfg.Admins {
		admins[n] = v
	}
	h.auth.usersMu.Unlock()
	if err := persistUsers(h.authCfg.UsersPath, users, admins); err != nil {
		h.auth.logAudit(r, op, "admin-reset "+req.Name, "fail(write)")
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": "写入 users.json 失败"})
		return
	}
	h.auth.logAudit(r, op, "admin-reset "+req.Name, "ok")
	writeJSON(w, map[string]any{"ok": true})
}

// POST /api/admin/users/delete {name}
func (h *Handler) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	op, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}
	if h.authCfg.UsersPath == "" {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "需要 -auth-file 模式才能管理用户"})
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	if req.Name == op {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "不能删除自己"})
		return
	}
	h.auth.usersMu.Lock()
	if _, exists := h.authCfg.Users[req.Name]; !exists {
		h.auth.usersMu.Unlock()
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "用户不存在"})
		return
	}
	// 最后一名超管不能删
	if h.authCfg.Admins[req.Name] {
		n := 0
		for _, v := range h.authCfg.Admins {
			if v {
				n++
			}
		}
		if n <= 1 {
			h.auth.usersMu.Unlock()
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "这是最后一个超管，不能删除"})
			return
		}
	}
	delete(h.authCfg.Users, req.Name)
	delete(h.authCfg.Admins, req.Name)
	users := make(map[string]string, len(h.authCfg.Users))
	for n, v := range h.authCfg.Users {
		users[n] = v
	}
	admins := make(map[string]bool, len(h.authCfg.Admins))
	for n, v := range h.authCfg.Admins {
		admins[n] = v
	}
	h.auth.usersMu.Unlock()
	if err := persistUsers(h.authCfg.UsersPath, users, admins); err != nil {
		h.auth.logAudit(r, op, "admin-delete "+req.Name, "fail(write)")
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": "写入 users.json 失败"})
		return
	}
	h.auth.logAudit(r, op, "admin-delete "+req.Name, "ok")
	writeJSON(w, map[string]any{"ok": true})
}

// POST /api/admin/users/role {name,admin}
func (h *Handler) handleAdminSetRole(w http.ResponseWriter, r *http.Request) {
	op, ok := h.requireAdmin(w, r)
	if !ok {
		return
	}
	if h.authCfg.UsersPath == "" {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "需要 -auth-file 模式才能管理用户"})
		return
	}
	var req struct {
		Name  string `json:"name"`
		Admin bool   `json:"admin"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	h.auth.usersMu.Lock()
	if _, exists := h.authCfg.Users[req.Name]; !exists {
		h.auth.usersMu.Unlock()
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "用户不存在"})
		return
	}
	if !req.Admin && h.authCfg.Admins[req.Name] {
		n := 0
		for _, v := range h.authCfg.Admins {
			if v {
				n++
			}
		}
		if n <= 1 {
			h.auth.usersMu.Unlock()
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "这是最后一个超管，不能降级"})
			return
		}
	}
	if req.Admin {
		h.authCfg.Admins[req.Name] = true
	} else {
		delete(h.authCfg.Admins, req.Name)
	}
	users := make(map[string]string, len(h.authCfg.Users))
	for n, v := range h.authCfg.Users {
		users[n] = v
	}
	admins := make(map[string]bool, len(h.authCfg.Admins))
	for n, v := range h.authCfg.Admins {
		admins[n] = v
	}
	h.auth.usersMu.Unlock()
	if err := persistUsers(h.authCfg.UsersPath, users, admins); err != nil {
		h.auth.logAudit(r, op, "admin-role "+req.Name, "fail(write)")
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": "写入 users.json 失败"})
		return
	}
	action := "admin-demote "
	if req.Admin {
		action = "admin-promote "
	}
	h.auth.logAudit(r, op, action+req.Name, "ok")
	writeJSON(w, map[string]any{"ok": true})
}
