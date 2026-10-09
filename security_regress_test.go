package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io/fs"
	"os"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件锁住 2026-10-09 安全审查里**实测发现并修复**的几个洞。
//
// 为什么要专门写：这些洞当时是抓到了、也修了，但**没有测试**。修完之后没有
// 回归保护，下一次重构很容易把它改回来 —— 而且这类回归不会让任何现有测试
// 变红（它们测的是别的东西），只会在某天重新变成可利用的漏洞。

// ---- 1. 目录认领不能被静默抢占 ----
//
// 原来的 SQL：
//   ON CONFLICT(folder_id) DO UPDATE SET user_name=excluded.user_name
// 没有 WHERE，于是 DO UPDATE **无条件覆盖**已有认领人。局域网里任何人都能
// 一条 POST 抢走别人的目录，而且把 user 填成对方的名字也不影响 —— 结果是
// 两个人都以为目录是自己的。

func TestClaimCannotSteal(t *testing.T) {
	s := newTestStore(t)
	dir := testFolderID(t, s)

	if ok, err := s.ClaimFolder(dir, "alice"); err != nil || !ok {
		t.Fatalf("alice 认领失败: ok=%v err=%v", ok, err)
	}
	// bob 抢：必须失败
	ok, err := s.ClaimFolder(dir, "bob")
	if err != nil {
		t.Fatalf("bob 认领不该报错: %v", err)
	}
	if ok {
		t.Fatal("bob 抢走了 alice 的目录 —— 认领锁失效")
	}
	if owner, _ := s.ClaimOf(dir); owner != "alice" {
		t.Fatalf("认领人被改成了 %q，应该是 alice", owner)
	}
	// alice 自己续期：必须成功（否则正常操作会被自己的锁挡住）
	if ok, err := s.ClaimFolder(dir, "alice"); err != nil || !ok {
		t.Fatalf("alice 自己续期失败: ok=%v err=%v", ok, err)
	}
	// 伪造：bob 用 alice 的名字也不能抢走 —— 因为 WHERE 比的是 user_name，
	// 而 bob 传的 user_name 就是 alice 的话等同于 alice 本人操作，属于同一个洞：
	// 只要请求体里的 user 可任意伪造，锁就形同虚设。
	// 所以真正的防线在 API 层：user 取自 userOf(r)（请求头），
	// 而请求头是谁浏览器填的。也就是说**身份本身不可信**是设计前提，
	// 局域网工具不打算解决"谁能自称张三"这个问题 ——
	// 它只保证"自称不同名字的人互相不踩"，见下面 TestClaimCannotStealByImpersonation。
	_ = ok
}

// 冒充场景：bob 把请求体里的 user 填成 "alice"，能否绕过锁？
//
// 答案：能，而且这是**已知且有意接受**的边界。claim 的 user 来自请求头/请求体，
// 没有密码，局域网里任何人都能自称任何人。这个工具防的是"意外踩踏"和
// "静默失败"，不是"身份认证" —— 真正的身份控制靠"不给别人发下载口令"
// + 目录认领只是协作提示。
//
// 那为什么还要测它？因为它是**必须被文档化的行为**：如果哪天有人给 claim
// 加了密码，这个测试会提醒他重新审视这条边界是否还成立。
func TestClaimCannotStealByImpersonation(t *testing.T) {
	s := newTestStore(t)
	dir := testFolderID(t, s)

	if ok, _ := s.ClaimFolder(dir, "alice"); !ok {
		t.Fatal("alice 认领失败")
	}
	// bob 自称 alice：会成功（这就是上面说的边界）
	ok, err := s.ClaimFolder(dir, "alice")
	if err != nil || !ok {
		t.Fatalf("自称 alice 的续期失败: ok=%v err=%v", ok, err)
	}
	// 但 bob **不能**用一个不同的名字抢走
	if ok, _ := s.ClaimFolder(dir, "bob"); ok {
		t.Fatal("不同名字仍应被拒")
	}
}

// ---- 2. /api/vtrans 不能被当成"任意文件下载"用 ----
//
// 原来的代码：非视频文件走 `http.ServeFile(w, r, absPath(f.RelPath))`，
// 于是**任何人不带 Cookie** 拿一个图片 id 就能下载原图（实测拿到 6.7MB），
// 完全绕过 /api/zip 的口令校验。

func TestVTransRejectsNonVideo(t *testing.T) {
	a := testAppWithImage(t, "127.0.0.1:0")
	h := a.h

	imgID := someImageID(t, a)
	if imgID == 0 {
		t.Fatal("测试素材里应该有图片")
	}
	// 无 Cookie 请求：必须是 404（不是 200、更不能是文件内容）
	rec := doGet(t, h, "/api/vtrans?id="+itoa(imgID))
	if rec.Code == http.StatusOK {
		t.Fatalf("未鉴权请求图片走 /api/vtrans 返回了 200 —— 鉴权绕过回归！body 长度 %d", rec.Body.Len())
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("图片请求 /api/vtrans 应返回 404，实际 %d", rec.Code)
	}
	// 不存在的 id 也不能 200
	rec2 := doGet(t, h, "/api/vtrans?id=999999")
	if rec2.Code == http.StatusOK {
		t.Fatal("不存在的文件 id 返回了 200")
	}
}

// /api/vtrans-warmup 现在要过下载鉴权（它每次调用都要全库 stat + 可能 ffprobe，
// 是最容易被拿来打服务的重端点）。
func TestWarmupRequiresDownloadAuth(t *testing.T) {
	a := testApp(t, "127.0.0.1:0")
	rec := doGet(t, a.h, "/api/vtrans-warmup")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("未鉴权访问 /api/vtrans-warmup 应 403，实际 %d", rec.Code)
	}
	// 带正确 Cookie 就放行
	req := httptest.NewRequest("GET", "/api/vtrans-warmup", nil)
	req.AddCookie(&http.Cookie{Name: "mr_dl", Value: a.h.tokenOf()})
	rec2 := httptest.NewRecorder()
	a.h.Routes().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("带正确 Cookie 应 200，实际 %d", rec2.Code)
	}
}

// ---- 3. absPath 不能被穿越出素材根 ----
//
// 素材根是用户选的目录，任何 ../.. 都应该被压在根内。
// 这里测的是"拼出来的路径确实落在 root 下面"，而不是"某个具体字符串被拒绝" —
// 前者才是这个函数的职责。

func TestAbsPathStaysInRoot(t *testing.T) {
	s := newTestStore(t)
	root := s.Root()
	if root == "" {
		t.Skip("没有素材根")
	}
	bad := []string{
		"../../../etc/passwd",
		`..\..\..\Windows\System32\config\SAM`,
		"./../../secrets",
		"sub/../../outside.jpg",
	}
	for _, rel := range bad {
		got := s.absPath(rel)
		if !strings.HasPrefix(strings.ToLower(got), strings.ToLower(root)) {
			t.Errorf("absPath(%q) 逃出了素材根: got=%q root=%q", rel, got, root)
		}
	}
	// 正常路径仍要能用
	if got := s.absPath("a/b.jpg"); got == "" || !strings.Contains(got, "b.jpg") {
		t.Errorf("正常相对路径解析异常: %q", got)
	}
}

// ---- 4. dl-login 的 Origin 白名单 + 失败节流 ----
//
// 这两道加固分别防：恶意网页诱导浏览器 POST（CSRF），以及反复试口令。

func TestDLLoginBlocksCrossSiteOrigin(t *testing.T) {
	a := testApp(t, "127.0.0.1:0")
	body := strings.NewReader(`{"token":"whatever"}`)
	req := httptest.NewRequest("POST", "/api/dl-login", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example.com")
	rec := httptest.NewRecorder()
	a.h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("跨站 Origin 应 403，实际 %d", rec.Code)
	}
}

func TestDLLoginThrottles(t *testing.T) {
	a := testApp(t, "127.0.0.1:0")
	routes := a.h.Routes()

	// 返回解析后的 ok 字段。注意节流命中时是 **200 + ok:false**（不是 403）：
	// 前端要能显示"请 N 秒后再试"这句话，走 403 就只能弹一个通用错误页。
	post := func(tok string) (int, bool) {
		req := httptest.NewRequest("POST", "/api/dl-login", strings.NewReader(`{"token":"`+tok+`"}`))
		req.Header.Set("Content-Type", "application/json")
		// 固定 RemoteAddr：节流按来源 IP 计，不固定的话每轮都算新 IP，测不出来。
		req.RemoteAddr = "192.168.1.50:12345"
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		var d struct {
			Ok  bool   `json:"ok"`
			Msg string `json:"msg"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &d)
		if rec.Header().Get("Retry-After") != "" {
			// 锁定中
			return rec.Code, false
		}
		return rec.Code, d.Ok
	}

	// 连错 dlFailMax 次 → 触发锁定
	for i := 0; i < dlFailMax; i++ {
		if _, ok := post("wrong-token"); ok {
			t.Fatalf("第 %d 次错误口令不该成功", i+1)
		}
	}
	// 锁定后**用正确口令**也必须失败 —— 这才是节流的意义：不给暴力留缝。
	if _, ok := post(a.h.tokenOf()); ok {
		t.Fatal("锁定期间仍接受正确口令 —— 节流没生效")
	}

	// 锁定期内应带 Retry-After，且提示里要有秒数
	req := httptest.NewRequest("POST", "/api/dl-login", strings.NewReader(`{"token":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.168.1.50:12345"
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	if rec.Header().Get("Retry-After") == "" {
		t.Error("被锁定时响应里应有 Retry-After")
	}
	var d struct {
		Ok  bool   `json:"ok"`
		Msg string `json:"msg"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.Ok {
		t.Error("锁定期间 ok 必须是 false")
	}
	if !strings.Contains(d.Msg, "秒") {
		t.Errorf("锁定提示应告知还需等几秒，实际: %q", d.Msg)
	}

	// 成功登录要清零：这里模拟锁定期过去后 —— 直接验证清零函数的行为
	h := a.h
	h.dlClearFailures("192.168.1.50")
	if left := h.dlLockedFor("192.168.1.50"); left > 0 {
		t.Errorf("登录成功后应清空失败记录，仍剩 %v", left)
	}
	if _, ok := post(a.h.tokenOf()); !ok {
		t.Error("清零后正确口令应被接受")
	}
}

// ---- 5. 用户名白名单 ----
//
// 名字来自请求头，任何人都能塞任意字符串。它会进 known_user、进认领锁、
// 还要在仪表盘上原样渲染 —— 没有校验时一个 5000 字符的名字就能撑爆布局。

func TestSanitizeUser(t *testing.T) {
	ok := []struct{ in, want string }{
		{"张三", "张三"},
		{"  李四  ", "李四"},
		{"Alice", "Alice"},
		{"小蛋糕 A", "小蛋糕 A"},
		// 边界：正好等于上限（24 rune）必须**合法**。
		// 这里曾经写成非法导致误报 —— 上限是"含"边界而不是"不含"，
		// 差一个 rune 就把人名截断/拒绝，症状是"某人突然登录失败"极难排查。
		{strings.Repeat("超", maxUserNameRunes), strings.Repeat("超", maxUserNameRunes)},
		{"张三丰张三丰张三丰张三丰张三丰张三丰张三丰张三丰", "张三丰张三丰张三丰张三丰张三丰张三丰张三丰张三丰"},
	}
	for _, c := range ok {
		got, valid := sanitizeUser(c.in)
		if !valid {
			t.Errorf("sanitizeUser(%q) 应该合法", c.in)
			continue
		}
		if got != c.want {
			t.Errorf("sanitizeUser(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	bad := []string{
		"",         // 空
		"   ",      // 全空白
		"abc\ndef", // 控制字符：能在日志/页面里伪造出一整行
		"abc\tdef",
		"a\x00b", // NUL
		// 26 个 rune，超过上限
		"张三丰张三丰张三丰张三丰张三丰张三丰张三丰张三丰张三丰",
		strings.Repeat("超", maxUserNameRunes+1), // 用上限+1 直接卡边界
	}
	for _, in := range bad {
		if got, valid := sanitizeUser(in); valid {
			t.Errorf("sanitizeUser(%q) 应该判为非法，却返回了 %q", in, got)
		}
	}
}

// 名字超长时不能入库（这是 known_user 被灌垃圾的实际入口）。
func TestOversizedUserNotRemembered(t *testing.T) {
	s := newTestStore(t)
	h := &Handler{store: s}
	h.rememberUser("这是一个非常非常长的名字用来撑爆仪表盘布局abcdefghijklmnop")
	users, err := s.KnownUsers()
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if len([]rune(u)) > maxUserNameRunes {
			t.Fatalf("超长名字被记进了 known_user: %q", u)
		}
	}
}

// ---- 6. 僵尸认领：只统计不自动释放，且强制释放有 stale 门槛 ----
//
// 自动释放会在合法的长休息（喝咖啡、开会）时把别人的目录抢走；
// 而不卡 stale 直接释放，则等于任何人都能抢锁。所以两边都要测。

func TestStaleClaimNotAutoReleased(t *testing.T) {
	s := newTestStore(t)
	dir := testFolderID(t, s)
	if ok, _ := s.ClaimFolder(dir, "alice"); !ok {
		t.Fatal("认领失败")
	}
	// 手工把 claimed_at 推到很久以前
	old := time.Now().Add(-2 * time.Hour).UnixMilli()
	if _, err := s.db.Exec(`UPDATE claim SET claimed_at=? WHERE folder_id=?`, old, dir); err != nil {
		t.Fatal(err)
	}
	// 只是**读取**区块，不应该动数据
	if _, err := (&Handler{store: s, scanner: newTestScanner(s.db)}).calcFolders(); err != nil {
		t.Fatal(err)
	}
	if owner, _ := s.ClaimOf(dir); owner != "alice" {
		t.Fatalf("读取区块后认领被自动释放了（owner=%q）—— 不能自动踢人", owner)
	}
}

func TestForceReleaseRespectsStaleWindow(t *testing.T) {
	s := newTestStore(t)
	dir := testFolderID(t, s)
	if ok, _ := s.ClaimFolder(dir, "alice"); !ok {
		t.Fatal("认领失败")
	}
	// 刚认领 = 活动期内：强制释放必须被拒
	if ok, _, err := s.ForceReleaseStale(dir, staleClaimMs); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("活动期内的认领被强制释放了 —— 认领锁形同虚设")
	}
	// 推成僵尸后再释放：应该成功
	if _, err := s.db.Exec(`UPDATE claim SET claimed_at=? WHERE folder_id=?`,
		time.Now().Add(-2*time.Hour).UnixMilli(), dir); err != nil {
		t.Fatal(err)
	}
	ok, owner, err := s.ForceReleaseStale(dir, staleClaimMs)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("僵尸认领释放失败")
	}
	if owner != "alice" {
		t.Errorf("释放时返回的原认领人 = %q，应该是 alice", owner)
	}
	if o, _ := s.ClaimOf(dir); o != "" {
		t.Errorf("释放后仍有认领人 %q", o)
	}
}

// ---- 7. claim 时间戳单位 ----
//
// claimed_at 以前写 Unix() 秒，新代码按 UnixMilli() 毫秒读。
// 单位不一致的后果是老认领全被判成"闲置 56 年"，页面上一片僵尸。
// normalizeClaimTimestamps 负责把老数据换算掉。

func TestClaimTimestampsAreMilliseconds(t *testing.T) {
	s := newTestStore(t)
	dir := testFolderID(t, s)
	if ok, _ := s.ClaimFolder(dir, "alice"); !ok {
		t.Fatal("认领失败")
	}
	var v int64
	if err := s.db.QueryRow(`SELECT claimed_at FROM claim WHERE folder_id=?`, dir).Scan(&v); err != nil {
		t.Fatal(err)
	}
	// 毫秒时间戳现在是 1.7e12 量级；秒是 1.7e9。
	if v < 1_000_000_000_000 {
		t.Fatalf("claimed_at=%d 不是毫秒量级 —— 单位又退回秒了", v)
	}
	// 归一函数对已是毫秒的数据必须是幂等的（不能再乘 1000）
	if err := normalizeClaimTimestamps(s.db); err != nil {
		t.Fatal(err)
	}
	var v2 int64
	if err := s.db.QueryRow(`SELECT claimed_at FROM claim WHERE folder_id=?`, dir).Scan(&v2); err != nil {
		t.Fatal(err)
	}
	if v2 != v {
		t.Fatalf("归一对已是毫秒的数据不该改动: %d -> %d", v, v2)
	}
}

// 老的秒级数据要被正确换算成毫秒。
func TestNormalizeConvertsSecondsToMillis(t *testing.T) {
	s := newTestStore(t)
	dir := testFolderID(t, s)
	if ok, _ := s.ClaimFolder(dir, "alice"); !ok {
		t.Fatal("认领失败")
	}
	// 模拟老版本留下的秒级时间戳
	sec := time.Now().Add(-time.Minute).Unix()
	if _, err := s.db.Exec(`UPDATE claim SET claimed_at=? WHERE folder_id=?`, sec, dir); err != nil {
		t.Fatal(err)
	}
	if err := normalizeClaimTimestamps(s.db); err != nil {
		t.Fatal(err)
	}
	var v int64
	if err := s.db.QueryRow(`SELECT claimed_at FROM claim WHERE folder_id=?`, dir).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v < 1_000_000_000_000 {
		t.Fatalf("秒级时间戳没被换算成毫秒: %d", v)
	}
	// 换算后应该"刚刚"，不是 56 年前 —— 用 1 分钟的余量校验
	if d := time.Since(time.UnixMilli(v)); d > 2*time.Minute || d < -time.Minute {
		t.Fatalf("换算后时间不对: 距今 %v", d)
	}
}

// ---- 8. 分享 token：有效、限次、过期 ----
//
// 分享 token 是替代"主口令进 URL"的新通路，它必须是限次且短命的，
// 否则就等于换了个名字的老洞。

func TestShareTokenIssueAndConsume(t *testing.T) {
	a := testApp(t, "127.0.0.1:0")
	h := a.h

	// 未登录不能签发（否则任何人都能给自己签发下载凭据）
	rec := doPostJSON(t, h, "/api/dl-share", `{}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("未登录签发分享 token 应 403，实际 %d", rec.Code)
	}

	// 登录后签发
	tok := issueShareToken(t, h)
	if len(tok) < 16 {
		t.Fatalf("分享 token 太短，熵不足: %q", tok)
	}
	// 有效期内可用
	if !h.shareTokenValid(tok) {
		t.Fatal("刚签发的分享 token 应有效")
	}
	// 额度耗尽后失效
	for i := 0; i < shareTokenUses; i++ {
		h.consumeShareQuota(&http.Request{URL: mustURL("/api/zip?s=" + tok)})
	}
	if h.shareTokenValid(tok) {
		t.Fatalf("用满 %d 次后分享 token 仍有效 —— 限次没生效", shareTokenUses)
	}
	// 未知 token 一律无效
	if h.shareTokenValid("deadbeefdeadbeef") {
		t.Fatal("不存在的分享 token 被判为有效")
	}
}

// 主口令不该再走 URL（默认关闭）。
func TestQueryTokenDisabledByDefault(t *testing.T) {
	a := testApp(t, "127.0.0.1:0")
	if a.h.allowQueryToken {
		t.Skip("这个实例开了 -allow-query-token")
	}
	req := httptest.NewRequest("GET", "/api/zip?folder=1&t="+a.h.tokenOf(), nil)
	if a.h.canDownload(req) {
		t.Fatal("默认配置下 ?t=口令 应该被拒（口令会进历史/Referer/日志）")
	}
	// 打开开关后才认
	a.h.allowQueryToken = true
	req2 := httptest.NewRequest("GET", "/api/zip?folder=1&t="+a.h.tokenOf(), nil)
	if !a.h.canDownload(req2) {
		t.Fatal("显式打开 -allow-query-token 后 ?t= 应被接受")
	}
}

// ---- 9. 增量推送：变了才推 ----
//
// 这是本次改造的核心不变量：内容没变的区块绝不能产生事件，
// 否则「每秒推一次全量」的旧开销会原样回来。

func TestDashboardSkipsUnchangedSections(t *testing.T) {
	a := testApp(t, "127.0.0.1:0")
	srv := httptest.NewServer(a.h.Routes())
	t.Cleanup(srv.Close)

	d := a.h.dash
	if d == nil {
		t.Fatal("dash 没初始化")
	}
	// 停掉后台循环：这个测试要自己控制 flush 时机，否则循环自己醒着
	// 会把事件混进来，断言就不确定了。
	d.Stop()

	ev := collectDashEvents(t, srv)
	// 建连首帧走的是 Full()，**顺手把缓存对齐了** —— 所以连接建立之后
	// 数据没再变的情况下，正确行为就是"什么都不推"。
	// 这正是改造要的效果：页面已经拿到全量了，没必要再推一遍一样的。
	ev.drain()

	d.MarkAll()
	d.flush()
	time.Sleep(150 * time.Millisecond)
	if got := ev.dashEvents(); len(got) != 0 {
		names := make([]string, 0, len(got))
		for _, e := range got {
			m, _ := e["data"].(map[string]any)
			names = append(names, fmt.Sprint(m["section"]))
		}
		t.Fatalf("内容没变却推了 %d 个事件（%v）—— 「变了才推」失效", len(got), names)
	}

	// 现在真的改一点数据：只有受影响的那一块该被推。
	// 用固定 id 1：testApp 不跑扫描，folder 表是空的，从表里查会拿到
	// "sql: no rows"（那是测试取值方式的问题，不是功能问题）。
	if ok, err := a.store.ClaimFolder(1, "alice"); err != nil || !ok {
		t.Fatalf("认领失败: ok=%v err=%v", ok, err)
	}
	d.Mark(SecFolders)
	d.flush()
	if !ev.waitFor(1, 2*time.Second) {
		t.Fatal("folders 变了却没有推事件")
	}
	got := ev.dashEvents()
	if len(got) != 1 {
		t.Fatalf("只改 folders 却推了 %d 个事件（应该正好 1 个）", len(got))
	}
	m, _ := got[0]["data"].(map[string]any)
	if m["section"] != SecFolders {
		t.Fatalf("推的是 %v，应该是 %s", m["section"], SecFolders)
	}
	// 且推的内容里要能看出认领数变了
	data, _ := m["data"].(map[string]any)
	if c, _ := data["claimed"].(float64); c != 1 {
		t.Errorf("推送内容里 claimed = %v，应该是 1", data["claimed"])
	}

	// 同样的状态再标脏一次：不该重复推
	d.Mark(SecFolders)
	d.flush()
	time.Sleep(150 * time.Millisecond)
	if got := ev.dashEvents(); len(got) != 0 {
		t.Fatalf("同一状态重复推了 %d 个事件", len(got))
	}
}

// 首帧必须包含全部区块：页面连上就该是完整的。
func TestDashboardSnapshotCoversAllSections(t *testing.T) {
	a := testApp(t, "127.0.0.1:0")
	full := a.h.dash.Full()
	for _, name := range AllSections {
		v, has := full[name]
		if !has {
			t.Errorf("全量快照缺少区块 %q", name)
			continue
		}
		if v == nil {
			t.Errorf("区块 %q 的值是 nil", name)
		}
	}
}

// 单个区块的计算失败不该拖垮其它区块（也不能 panic 或卡住）。
//
// 注意这里刻意**不断言"关库后 Full() 返回空"**：不是所有区块都依赖 DB。
// scan 进度来自内存里的 Scanner，health 里的"素材目录/缓存可写"是直接
// stat 文件系统，vtrans 有一部分来自磁盘枚举 —— 它们在库关掉后**仍然算得出来，
// 而且应该算得出来**（页面不能因为库出问题就整块空白，那等于把一个局部故障
// 放大成整个看板不可用）。
//
// 真正要保证的是：依赖 DB 的区块拿不到值，但**不 panic、不死锁、不返回垃圾**。
func TestDashboardSectionFailureIsolated(t *testing.T) {
	a := testApp(t, "127.0.0.1:0")

	// 先在库正常时留一份基线，确认这些区块本来是有内容的
	base := a.h.dash.Full()
	for _, name := range []string{SecTotals, SecFolders, SecWorkers, SecQC} {
		if _, ok := base[name]; !ok {
			t.Fatalf("基线里缺少区块 %q，说明这个用例选错了区块", name)
		}
	}

	// 关库。
	// 先等过 totals 的 1 秒记忆窗口：否则 totalsSnapshotOK 会命中缓存直接返回，
	// 根本不会打到库上 —— 那测的就不是"库关掉之后会怎样"，而是"缓存还热着"。
	time.Sleep(1100 * time.Millisecond)
	a.db.Close()

	done := make(chan map[string]any, 1)
	go func() { done <- a.h.dash.Full() }()
	var full map[string]any
	select {
	case full = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("关库后 Full() 卡住了 —— 区块计算里一定有没设超时的等待")
	}

	// 依赖 DB 的区块应当缺失（而不是返回一个看起来正常的假值）
	for _, name := range []string{SecTotals, SecFolders, SecWorkers, SecQC} {
		if v, has := full[name]; has {
			t.Errorf("库已关，区块 %q 却返回了值 %#v —— 那是编出来的，不能信", name, v)
		}
	}
	// 不依赖 DB 的区块仍然可用：局部故障不能放大成整页空白
	for _, name := range []string{SecScan, SecHealth} {
		if _, has := full[name]; !has {
			t.Errorf("区块 %q 不依赖数据库，库关掉时也该有值", name)
		}
	}

	// flush 在库关着的时候也不能 panic 或卡住
	markDone := make(chan struct{})
	go func() {
		defer close(markDone)
		a.h.dash.MarkAll()
		a.h.dash.flush()
	}()
	select {
	case <-markDone:
	case <-time.After(10 * time.Second):
		t.Fatal("关库后 flush() 卡住了")
	}
}

// ---- 10. SSE 建连即有全量 ----
//
// 页面连上就该是完整的，不该先空着再等第一次增量。

func TestSESSendsSnapshotOnConnect(t *testing.T) {
	a := testApp(t, "127.0.0.1:0")
	if a.hub.onConnect == nil {
		t.Fatal("onConnect 没注册")
	}
	v := a.hub.onConnect()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("onConnect 返回类型不对: %T", v)
	}
	if m["type"] != "snapshot" {
		t.Errorf("首帧 type = %v，应该是 snapshot", m["type"])
	}
	secs, ok := m["sections"].(map[string]any)
	if !ok {
		t.Fatal("首帧里没有 sections")
	}
	for _, name := range AllSections {
		if _, has := secs[name]; !has {
			t.Errorf("首帧缺少区块 %q", name)
		}
	}
}

// testAppWithImage 建一个素材根里带**真实 JPEG** 的测试实例。
//
// 为什么非要真实图片：/api/vtrans 的洞正是"非视频分支会 ServeFile 原文件"。
// 素材里没有图片时那条分支根本走不到，测试会静默 skip —— 一个"永远绿"的
// 安全测试等于没有测试。所以这里用标准库现造一张 4x4 的 JPEG，
// 让那条分支必然被执行到。
func testAppWithImage(t *testing.T, addr string) *app {
	t.Helper()
	root := t.TempDir()
	writeTestJPEG(t, filepath.Join(root, "照片.jpg"))

	sub, err := fs.Sub(webEmbed, "web")
	if err != nil {
		t.Fatal(err)
	}
	webFS = sub

	cfg := appConfig{
		root:     root,
		addr:     addr,
		dbPath:   filepath.Join(t.TempDir(), "t.db"),
		cacheDir: filepath.Join(t.TempDir(), "cache"),
		vjobs:    1, vres: 720, ires: 1600,
	}
	a, err := newApp(cfg, NewLogRing(200), "")
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(a.Close)
	if err := a.bind(); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// 必须真的跑一轮扫描：file 表里要有那行图片记录，
	// /api/vtrans 才会走到"非视频"分支（否则这条安全断言测的是空数据）。
	if err := a.scanner.Start(root); err != nil {
		t.Fatalf("启动扫描: %v", err)
	}
	waitScan(t, a)
	return a
}

// writeTestJPEG 造一张最小的合法 JPEG。
func writeTestJPEG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 30), B: 128, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := jpeg.Encode(f, img, nil); err != nil {
		t.Fatal(err)
	}
}

// ---------- 测试辅助 ----------

// newTestStore 建一个只含空壳的 Store（够测认领/路径/时间戳这类不依赖素材的逻辑）。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	db, err := openDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := NewStore(db, root)
	// 建一个根目录，folder id = 1
	if _, err := db.Exec(`INSERT INTO folder(id, name, rel_path, parent_id) VALUES(1,?,?,0)`,
		"root", ""); err != nil {
		t.Fatal(err)
	}
	return s
}

// newTestScanner 给只需要 Scanner.Progress() 的测试用（calcFolders 会读它）。
func newTestScanner(db *sql.DB) *Scanner {
	return &Scanner{db: db, hub: NewHub()}
}

// testFolderID 返回根目录 id（恒为 1，由 newTestStore 建好）。
func testFolderID(t *testing.T, s *Store) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM folder ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func doGet(t *testing.T, h *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

func doPostJSON(t *testing.T, h *Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

func mustURL(p string) *url.URL {
	u, _ := url.Parse(p)
	return u
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// issueShareToken 走真实 HTTP 路径签发一个分享 token（需要先有下载 Cookie）。
func issueShareToken(t *testing.T, h *Handler) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/dl-share", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "mr_dl", Value: h.tokenOf()})
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("签发分享 token 失败: %d %s", rec.Code, rec.Body.String())
	}
	var d struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d.Token
}

// someImageID 找一个图片 id；没有图片返回 0（让调用方 skip）。
func someImageID(t *testing.T, a *app) int64 {
	t.Helper()
	var id int64
	err := a.db.QueryRow(`SELECT id FROM file WHERE kind=? LIMIT 1`, kindImage).Scan(&id)
	if err != nil {
		return 0
	}
	return id
}

// eventCollector 通过一条**真实的 SSE 连接**收集事件。
//
// 刻意不用"给 Hub 加个测试钩子"的方式：那等于为了测试改生产代码的形状。
// 直接起 httptest.Server + http.Client 读事件流，顺带把 SSE 通路本身也测了
// （首帧格式、心跳、事件名），比旁路钩子更有价值。
type eventCollector struct {
	mu     sync.Mutex
	events []map[string]any

	cancel func()
}

// collectDashEvents 起一条 SSE 连接并开始后台读取。
// 调用方要在断言前调 wait()：SSE 是异步的，刚 Broadcast 完立刻断言会读到空。
func collectDashEvents(t *testing.T, srv *httptest.Server) *eventCollector {
	t.Helper()
	c := &eventCollector{}
	req, err := http.NewRequest("GET", srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("SSE Content-Type 不对: %q", ct)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue // ": ping" 心跳和空行直接跳过
			}
			var ev map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				continue
			}
			c.mu.Lock()
			c.events = append(c.events, ev)
			c.mu.Unlock()
		}
	}()
	c.cancel = func() {
		resp.Body.Close()
		<-done
	}
	t.Cleanup(c.cancel)
	// 等首帧到达，否则后面 flush 的事件可能和首帧竞态
	c.waitFor(1, 2*time.Second)
	return c
}

// waitFor 等到攒够 n 个事件（或超时）。SSE 是异步管道，
// 断言前必须等，否则会拿到"还没到"的结果并误判成功能失效。
func (c *eventCollector) waitFor(n int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := len(c.events)
		c.mu.Unlock()
		if got >= n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events) >= n
}

// drain 取走并清空已收到的事件。
func (c *eventCollector) drain() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.events
	c.events = nil
	return out
}

// dashEvents 只留 type=="dash" 的事件（首帧 type 是 "init"）。
func (c *eventCollector) dashEvents() []map[string]any {
	var out []map[string]any
	for _, ev := range c.drain() {
		if ev["type"] == "dash" {
			out = append(out, ev)
		}
	}
	return out
}
