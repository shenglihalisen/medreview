package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 会话签名必须能防篡改：改一个字符就失效。
func TestSessionSignVerify(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	exp := time.Now().Add(time.Hour)
	fp := pwFingerprint("$2a$10$abcdefghijklmnopqrstuv")
	tok := signSession(key, "alice", exp, fp)
	if got, gotFP := verifySession(key, tok); got != "alice" || gotFP != fp {
		t.Fatalf("正常 token 应返回 alice/%s，实际 %q/%q", fp, got, gotFP)
	}
	// 篡改载荷
	if got, _ := verifySession(key, "x"+tok); got != "" {
		t.Fatalf("篡改后应失效，实际 %q", got)
	}
	// 换密钥
	if got, _ := verifySession([]byte("ffffffffffffffffffffffffffffffff"), tok); got != "" {
		t.Fatalf("换密钥后应失效，实际 %q", got)
	}
	// 过期
	old := signSession(key, "alice", time.Now().Add(-time.Minute), fp)
	if got, _ := verifySession(key, old); got != "" {
		t.Fatalf("过期 token 应失效，实际 %q", got)
	}
	// 旧格式（没有密码指纹的两段载荷）一律不认
	legacy := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("alice|%d", exp.Unix())))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(fmt.Sprintf("alice|%d", exp.Unix())))
	legacy += "." + hex.EncodeToString(mac.Sum(nil))
	if got, _ := verifySession(key, legacy); got != "" {
		t.Fatalf("旧格式 token 应失效，实际 %q", got)
	}
}

// 改密码后旧会话必须失效：同一张 Cookie 不能再登进去。
func TestSessionInvalidatedOnPasswordChange(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	oldHash := "$2a$10$oldoldoldoldoldoldoldold"
	h := &Handler{
		authCfg: authConfig{Enabled: true, SessionKey: key, Users: map[string]string{"alice": oldHash}},
		auth:    newAuthState(authConfig{}),
	}
	tok := signSession(key, "alice", time.Now().Add(time.Hour), pwFingerprint(oldHash))
	req := httptest.NewRequest("GET", "/api/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	if got := h.sessionUser(req); got != "alice" {
		t.Fatalf("密码未变时应保持登录，实际 %q", got)
	}
	// 模拟改密码
	h.authCfg.Users["alice"] = "$2a$10$newnewnewnewnewnewnewnew"
	if got := h.sessionUser(req); got != "" {
		t.Fatalf("改密码后旧会话应失效，实际 %q", got)
	}
	// 模拟删账号
	delete(h.authCfg.Users, "alice")
	if got := h.sessionUser(req); got != "" {
		t.Fatalf("删账号后旧会话应失效，实际 %q", got)
	}
}

// 登录失败超过阈值后应被限流，窗口过期后恢复。
func TestLoginRateLimit(t *testing.T) {
	st := newAuthState(authConfig{})
	ip := "10.0.0.9"
	for i := 0; i < loginMaxFail; i++ {
		if !st.allowLogin(ip) {
			t.Fatalf("第 %d 次之前不该被限流", i+1)
		}
		st.noteLogin(ip, false)
	}
	if st.allowLogin(ip) {
		t.Fatal("达到上限后应被限流")
	}
	// 手动把窗口拨到过去，模拟 15 分钟之后
	st.mu.Lock()
	st.fails[ip].first = time.Now().Add(-loginWindow - time.Second)
	st.mu.Unlock()
	if !st.allowLogin(ip) {
		t.Fatal("窗口过期后应恢复")
	}
	// 成功登录清零
	st.noteLogin(ip, true)
	if !st.allowLogin(ip) {
		t.Fatal("登录成功后计数应清零")
	}
}

// 未登录请求 API 应 401，页面应 302 到登录页。
func TestAuthWrapGate(t *testing.T) {
	h := &Handler{authCfg: authConfig{Enabled: true, SessionKey: []byte("0123456789abcdef0123456789abcdef"), Users: map[string]string{}}, auth: newAuthState(authConfig{})}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	wrapped := h.authWrap(next)

	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录 API 应 401，实际 %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest("GET", "/review.html", nil))
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "/login.html") {
		t.Fatalf("未登录页面应 302 到登录页，实际 %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// 登录页本身必须放行
	rec = httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest("GET", "/login.html", nil))
	if rec.Code != 200 {
		t.Fatalf("登录页应放行，实际 %d", rec.Code)
	}
}

// 已登录但跨站 POST 必须被拒（CSRF 兜底）。
func TestAuthWrapCSRF(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	hash := "$2a$10$abcdefghijklmnopqrstuv"
	h := &Handler{authCfg: authConfig{Enabled: true, SessionKey: key, Users: map[string]string{"alice": hash}}, auth: newAuthState(authConfig{})}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	wrapped := h.authWrap(next)

	req := httptest.NewRequest("POST", "/api/review", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: signSession(key, "alice", time.Now().Add(time.Hour), pwFingerprint(hash))})
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨站 POST 应 403，实际 %d", rec.Code)
	}
}
