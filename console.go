package main

import (
	"os/exec"
	"strings"
	"sync"
)

// ---------------------------------------------------------------- 日志缓冲
//
// 为什么要有它：gui 版（medreview_ui.exe）双击启动后没有黑窗口，
// 地址、下载口令、扫描/转码进度就没人看得见 —— 于是这些东西在**本地桌面窗口**
// （gui_walk.go 的 runGUI）里看。窗口每 500ms 调一次 Since 拉增量，
// 所以这里按自增序号缓存最近若干行。console 版也顺带用它给日志落盘。

type logLine struct {
	Seq  int64  `json:"seq"`
	Text string `json:"text"`
}

type logRing struct {
	mu    sync.Mutex
	lines []logLine
	max   int
	seq   int64
}

func NewLogRing(max int) *logRing {
	if max <= 0 {
		max = 500
	}
	return &logRing{max: max}
}

// Write 实现 io.Writer，好直接塞进 log.SetOutput 的 MultiWriter。
// 一条 log.Printf 就是一次 Write（末尾带 \n），按行拆开存。
func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ln := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		r.seq++
		r.lines = append(r.lines, logLine{Seq: r.seq, Text: ln})
	}
	if len(r.lines) > r.max {
		r.lines = append(r.lines[:0], r.lines[len(r.lines)-r.max:]...)
	}
	return len(p), nil
}

// Since 返回序号大于 since 的行。since<=0 表示"全给我"。
func (r *logRing) Since(since int64) ([]logLine, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []logLine
	for _, l := range r.lines {
		if l.Seq > since {
			out = append(out, l)
		}
	}
	if out == nil {
		out = []logLine{}
	}
	return out, r.seq
}

func (r *logRing) LastSeq() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// ---------------------------------------------------------------- 本地窗口辅助
//
// 网页控制台接口（/api/console、/api/shutdown、/api/open-log）已删除：
// gui 版的控制台是**本地桌面窗口**（gui_walk.go 的 runGUI），
// 直接从进程内读 logRing、调 os.Exit / ShellExecute，不经过 HTTP、不需要口令鉴权，
// 也等于不把本地黑窗口里的信息（含下载口令）通过网页暴露出去。
// 这里只保留「把日志文件用资源管理器打开」的底层能力，给 fatalExit 弹日志用。

// openPathWithShell 用 ShellExecute 打开一个本地文件（无黑窗版出错时把日志弹给用户看）。
// 沿用 main.go 里 openBrowser 的那套候选顺序，并且**不能优先用 explorer** ——
// 它会把路径里的特殊字符当通配符解析。
func openPathWithShell(path string) {
	candidates := [][]string{
		{"cmd", "/c", "start", "", path},
		{"rundll32", "url.dll,FileProtocolHandler", path},
	}
	for _, c := range candidates {
		cmd := exec.Command(c[0], c[1:]...)
		hideWindow(cmd)
		if err := cmd.Start(); err == nil {
			go func() { _ = cmd.Wait() }() // 收尸，不阻塞
			return
		}
	}
	// 都打不开就算了 —— 原因已经写进日志和 medreview.log 里了
}
