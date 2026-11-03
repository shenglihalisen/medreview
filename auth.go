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
	Enabled   bool
	Users     map[string]string // 用户名 → bcrypt 哈希
	SessionKey []byte
	AuditPath string
	Secure    bool // 只走 HTTPS 时置 true（Cookie 加 Secure）
}

// authState 运行期状态：登录失败计数 + 审计文件句柄。
type authState struct {
	mu      sync.Mutex
	fails   map[string]*failRec
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
func loadAuthConfig(single, file, keyHex, audit string, secure bool) (authConfig, error) {
	cfg := authConfig{Users: map[string]string{}, AuditPath: audit, Secure: secure}
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return cfg, fmt.Errorf("读取 -auth-file 失败: %w", err)
		}
		var doc struct {
			Users map[string]string `json:"users"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			return cfg, fmt.Errorf("解析 -auth-file 失败: %w", err)
		}
		for u, hash := range doc.Users {
			if !strings.HasPrefix(hash, "$2") {
				return cfg, fmt.Errorf("用户 %s 的密码不是 bcrypt 哈希（用 htpasswd -bnBC 10 \"\" pass 生成）", u)
			}
			cfg.Users[u] = hash
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
