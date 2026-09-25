package main

import (
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 这三个测试盯的是**本地窗口要在运行时改配置**那几条路径：
// 换素材根目录 / 换下载口令 / 换监听地址。
// 它们以前只能靠重启程序实现，现在是进程内操作 —— 一旦有人改坏了顺序
// （比如忘了重算带 ?t= 的地址、或者重绑端口后忘了重新 Serve），
// 界面上会显示一个"已经生效但实际还是旧的"地址，很难靠肉眼发现。

func testRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "素材", "照片")
	if err := os.MkdirAll(filepath.Join(root, "子目录"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func testApp(t *testing.T, addr string) *app {
	t.Helper()
	// main() 里做的事情：把内嵌网页挂给静态路由（外部 tooling 直接 newApp 时它是空的）
	sub, err := fs.Sub(webEmbed, "web")
	if err != nil {
		t.Fatal(err)
	}
	webFS = sub

	tmp := t.TempDir()
	cfg := appConfig{
		root:     testRoot(t),
		addr:     addr,
		dbPath:   filepath.Join(tmp, "t.db"),
		cacheDir: filepath.Join(tmp, "cache"),
		vjobs:    1,
		vres:     720,
		ires:     1600,
	}
	a, err := newApp(cfg, NewLogRing(200), "")
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	t.Cleanup(a.Close)
	if err := a.bind(); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return a
}

func portOf(t *testing.T, a *app) int {
	t.Helper()
	a.netMu.Lock()
	defer a.netMu.Unlock()
	ta, ok := a.ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("拿不到监听端口")
	}
	return ta.Port
}

func TestSetRoot(t *testing.T) {
	a := testApp(t, "127.0.0.1:0")
	old := a.store.Root()
	newRoot := testRoot(t)

	if err := a.SetRoot(newRoot); err != nil {
		t.Fatalf("SetRoot: %v", err)
	}
	if got := a.store.Root(); got != newRoot {
		t.Fatalf("换根目录没生效: got=%q want=%q", got, newRoot)
	}
	if old == newRoot {
		t.Fatal("测试写错了：新旧根目录一样")
	}
	// Scanner 在后台 goroutine 里干活，meta 里的新路径要等它跑完才有
	waitScan(t, a)
	if got := metaGet(a.db, "root_path"); got != newRoot {
		t.Fatalf("新根目录没写进 meta: got=%q want=%q", got, newRoot)
	}
	// 空路径要报错，不能悄悄把根目录清掉
	if err := a.SetRoot(""); err == nil {
		t.Fatal("SetRoot(\"\") 应该报错")
	}
}

func TestSetToken(t *testing.T) {
	a := testApp(t, "127.0.0.1:0")
	before := a.Status()
	if !strings.Contains(before.DownloadURL, "?t="+before.Token) {
		t.Fatalf("下载页地址里没带口令: %s", before.DownloadURL)
	}

	if err := a.SetToken("woaini123"); err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	after := a.Status()
	if after.Token != "woaini123" {
		t.Fatalf("口令没换成: %s", after.Token)
	}
	if !strings.Contains(after.DownloadURL, "?t=woaini123") {
		t.Fatalf("下载页地址没跟着更新: %s", after.DownloadURL)
	}
	if after.ReviewURL != before.ReviewURL {
		t.Fatalf("换口令不该动审阅页地址: %s → %s", before.ReviewURL, after.ReviewURL)
	}
	if a.h.tokenOf() != "woaini123" {
		t.Fatal("Handler 里的口令没同步")
	}

	nt, err := a.NewToken()
	if err != nil || len(nt) != 24 {
		t.Fatalf("NewToken: %v %q", err, nt)
	}
	if err := a.SetToken("  "); err == nil {
		t.Fatal("SetToken(空白) 应该报错")
	}
}

func TestRebind(t *testing.T) {
	a := testApp(t, "127.0.0.1:0")
	p1 := portOf(t, a)
	a.serveBackground()
	if _, err := get(fmt.Sprintf("http://127.0.0.1:%d/review.html", p1)); err != nil {
		t.Fatalf("原端口服务不通: %v", err)
	}

	// 换到一个 explicitly 指定的新端口
	l2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p2 := l2.Addr().(*net.TCPAddr).Port
	_ = l2.Close()

	if err := a.Rebind(fmt.Sprintf("127.0.0.1:%d", p2)); err != nil {
		t.Fatalf("Rebind: %v", err)
	}
	if got := portOf(t, a); got != p2 {
		t.Fatalf("没绑到新端口: got=%d want=%d", got, p2)
	}
	// 新端口要能马上服务（改完地址不用重启程序）
	if _, err := get(fmt.Sprintf("http://127.0.0.1:%d/review.html", p2)); err != nil {
		t.Fatalf("新端口服务不通: %v", err)
	}
	// 地址字符串要跟着换
	if got := a.Status().ReviewURL; !strings.Contains(got, fmt.Sprintf(":%d/", p2)) {
		t.Fatalf("ReviewURL 没跟着换: %s", got)
	}

	// 格式不对的地址要报错，而不是把服务弄挂
	if err := a.Rebind("随便写"); err == nil {
		t.Fatal("Rebind(乱写) 应该报错")
	}
	if _, err := get(fmt.Sprintf("http://127.0.0.1:%d/review.html", p2)); err != nil {
		t.Fatalf("Rebind 失败后老服务也挂了: %v", err)
	}
}

// waitScan 等后台扫描跑完（/api/status 的 running=false + endedAt>0 那一套判断）。
// 注意不能只看 running：启动前的初始状态也是 false，得看有没有跑完的时间戳。
func waitScan(t *testing.T, a *app) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		p := a.scanner.Progress()
		if !p.Running && p.EndedAt > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("扫描超时未完成")
}

// get 取一个 URL 并返回 body。
func get(url string) (string, error) {
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(b), nil
}
