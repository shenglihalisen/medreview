package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// VideoService 把浏览器放不了的视频（主要是 HEVC/H.265）转成 H.264 预览并缓存，
// 让审阅页在浏览器里能直接播放。
//
// 背景：iPhone/安卓录屏常见 H.265（ffprobe 里叫 hevc，MP4 里 fourcc 是 hvc1/hev1），
// Chrome/Edge 在 Windows 上基本无法解码，<video> 会直接报"无法播放"。
//
// 转码时机：**启动时 / 每次扫描完成后立刻预热**（WarmUp），而不是等用户点开某个视频才转。
// 预热分两阶段：
//  1. 预检 —— 并发跑 ffprobe 只读文件头判断编码。**不是所有视频都要转**：
//     本来就是 H.264/VP8/VP9/AV1 的直接跳过，已有转码产物的也跳过。
//  2. 转码 —— 只把"确实播不了"的排进队列，按并发数跑。
//
// 保留懒转码（Status 里按需触发）作为兜底：预热还没轮到的文件被点开时也不会卡住。
//
// ffmpeg/ffprobe 查找顺序：
//  1. 可执行文件同级的 tools\ffmpeg\（内置，推荐）
//  2. 当前工作目录的 tools\ffmpeg\（开发时 go run）
//  3. PATH
//
// 找不到 ffmpeg 时不报错，只是标记为 unavailable，由前端提示用户。
type VideoService struct {
	store   *Store
	dir     string
	ffmpeg  string
	ffprobe string
	profile transcodeProfile

	mu     sync.Mutex
	probes map[string]probeInfo
	jobs   map[string]*videoJob
	warm   WarmupState
}

// transcodeProfile 是转码规格。它**参与缓存键**：改了分辨率/CRF/预设就等于换了产物，
// 老缓存必须自动失效 —— 否则改了参数还会继续把老规格的视频喂给浏览器。
type transcodeProfile struct {
	shortSide int    // 720p：把"较小边"封顶到这个值（横屏就是高，竖屏就是宽）；0 = 不缩放
	crf       int    // H.264 质量参数，越大越糊越小
	preset    string // libx264 预设，越快文件越大
}

func (p transcodeProfile) tag() string {
	return fmt.Sprintf("h%d-crf%d-%s", p.shortSide, p.crf, p.preset)
}

func (p transcodeProfile) describe() string {
	dim := "保持原分辨率"
	if p.shortSide > 0 {
		dim = fmt.Sprintf("%dp（较小边≤%d）", p.shortSide, p.shortSide)
	}
	return fmt.Sprintf("%s, H.264 %s crf%d", dim, p.preset, p.crf)
}

// targetSize 按 720p 算输出尺寸：把较小的一边封顶到 shortSide，另一边等比缩放。
// 横屏 2772x1280 → 1558x720；竖屏 1080x1920 → 720x1280。
// 本来就没超过上限的不放大（返回 need=false）。
func (p transcodeProfile) targetSize(srcW, srcH int) (w, h int, need bool) {
	if srcW <= 0 || srcH <= 0 || p.shortSide <= 0 {
		return srcW, srcH, false
	}
	short := srcW
	if srcH < srcW {
		short = srcH
	}
	if short <= p.shortSide {
		return srcW, srcH, false
	}
	ratio := float64(p.shortSide) / float64(short)
	w = int(math.Round(float64(srcW) * ratio))
	h = int(math.Round(float64(srcH) * ratio))
	// H.264 + yuv420p 要求宽高都是偶数
	if w%2 != 0 {
		w--
	}
	if h%2 != 0 {
		h--
	}
	if w < 2 {
		w = 2
	}
	if h < 2 {
		h = 2
	}
	return w, h, true
}

type probeInfo struct {
	ok    bool
	codec string // h264 / hevc / vp9 ...
	w, h  int    // 源分辨率，用来算 720p 的目标尺寸
	dur   float64
	err   string
}

type videoJob struct {
	state string // queued（已排队，等 worker）/ running / done / failed
	pct   int
	msg   string
}

// vstatus 是给前端的状态：ready 才能把 src 指到 /api/vtrans。
type vstatus struct {
	State  string `json:"state"`  // ready / transcoding / unavailable / error
	Pct    int    `json:"pct"`    // 0-100，转码进度
	Codec  string `json:"codec"`  // 源文件编码
	Source string `json:"source"` // original / transcoded
	Msg    string `json:"msg"`
}

func NewVideoService(store *Store, cacheDir string, shortSide int) (*VideoService, error) {
	if err := os.MkdirAll(filepath.Join(cacheDir, "vtrans"), 0o755); err != nil {
		return nil, err
	}
	v := &VideoService{
		store:   store,
		dir:     cacheDir,
		probes:  map[string]probeInfo{},
		jobs:    map[string]*videoJob{},
		profile: transcodeProfile{shortSide: shortSide, crf: 26, preset: "veryfast"},
	}
	// 注意：这里**不碰磁盘上的缓存**。清理统一交给 PrepareCache，
	// 由 main 在端口占住之后调用 —— 否则"注定起不来的第二个实例"也会先把
	// 正在跑的那个实例的缓存删掉。
	v.ffmpeg = findTool("ffmpeg")
	v.ffprobe = findTool("ffprobe")
	if v.ffmpeg != "" && v.ffprobe != "" {
		log.Printf("视频转码: 就绪 (%s)", v.profile.describe())
		log.Printf("          ffmpeg: %s", v.ffmpeg)
	} else {
		log.Printf("视频转码: 未找到 ffmpeg/ffprobe —— HEVC 等编码的视频将无法在浏览器里播放")
	}
	return v, nil
}

func findTool(name string) string {
	exe := name
	if runtime.GOOS == "windows" {
		exe = name + ".exe"
	}
	var dirs []string
	if self, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(self))
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	for _, d := range dirs {
		for _, sub := range [][]string{{"tools", "ffmpeg"}, {"tools", "ffmpeg", "bin"}, {"tools"}} {
			p := filepath.Join(append([]string{d}, append(sub, exe)...)...)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	// 内置 ffmpeg：exe 被拷到没有 tools 目录的地方（比如桌面）双击时，
	// 从 %LocalAppData%\medreview\tools\ffmpeg 里取（首次运行自动释放，见 ffmpeg_embed.go）。
	if d := embeddedToolsDir(); d != "" {
		p := filepath.Join(d, exe)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// Available 报告能否转码（没装 ffmpeg 时前端会给出明确提示）。
func (v *VideoService) Available() bool { return v.ffmpeg != "" && v.ffprobe != "" }

// key 用 "相对路径 + 大小 + 修改时间 + 转码规格" 做缓存键。和缩略图同理：
// 只按 id 缓存会在换素材目录后张冠李戴；不带规格则改了 -vres 还会命中老产物。
func (v *VideoService) key(f *FileItem) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%s|%d|%d|%s", f.RelPath, f.Size, f.MTime, v.profile.tag())))
	return hex.EncodeToString(sum[:])
}

func (v *VideoService) outPath(f *FileItem) string {
	k := v.key(f)
	return filepath.Join(v.dir, "vtrans", k[:2], k+".mp4")
}

// profileName 记录当前缓存是用哪套规格转的，放在 vtrans/ 根下。
const profileName = ".profile"

func (v *VideoService) profileFile() string {
	return filepath.Join(v.dir, "vtrans", profileName)
}

// PrepareCache 处理"上一轮留下的转码产物"，清理动作集中在这里。
//
// ⚠️ **必须在端口占住之后调用**。放在 NewVideoService 里的话，用户双击两次
// start.bat 时，那个注定因端口被占而退出的实例，会先把正在运行的实例的
// 刚转好的缓存删掉 —— 于是页面上所有视频莫名其妙开始重转。
//
// keep=false（默认）：删掉上一轮的全部产物，每次启动都是干净的；
// keep=true（-vkeep）：产物留着复用，只清 .part 残渣；
// 无论 keep 是什么，转码规格变了（-vres 改过）就一定清，因为老产物已经作废。
func (v *VideoService) PrepareCache(keep bool) error {
	tag := v.profile.tag()
	f := v.profileFile()

	old := ""
	if b, err := os.ReadFile(f); err == nil {
		old = strings.TrimSpace(string(b))
	} else if !os.IsNotExist(err) {
		return err
	}
	specChanged := old != tag
	dropProducts := !keep || specChanged

	d := filepath.Join(v.dir, "vtrans")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}

	var products, parts, locked int
	var freedProd, freedParts int64
	// 删单文件：成功了返回大小；被别的进程占着（上一次的 ffmpeg 还没退干净）
	// 就跳过并计数 —— 绝不能因为清不掉一个文件就让服务起不来。
	del := func(p string) (int64, bool) {
		sz := int64(0)
		if fi, err := os.Stat(p); err == nil {
			sz = fi.Size()
		}
		if err := os.Remove(p); err != nil {
			locked++
			return 0, false
		}
		return sz, true
	}

	err := filepath.WalkDir(d, func(p string, e os.DirEntry, werr error) error {
		if werr != nil || e.IsDir() {
			return nil
		}
		if e.Name() == profileName {
			return nil
		}
		switch {
		case strings.HasSuffix(e.Name(), ".part"):
			// 半成品一定是废的（进程被杀时 defer 跑不到），无论 keep 都清
			if sz, ok := del(p); ok {
				parts++
				freedParts += sz
			}
		case dropProducts:
			if sz, ok := del(p); ok {
				products++
				freedProd += sz
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 产物删光了，把 vtrans/ 下那些空的 xx/ 分片目录也收掉
	if ents, err := os.ReadDir(d); err == nil {
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			sub := filepath.Join(d, e.Name())
			if rest, err := os.ReadDir(sub); err == nil && len(rest) == 0 {
				_ = os.Remove(sub)
			}
		}
	}

	if specChanged {
		log.Printf("转码规格已变（%s → %s），旧产物全部作废", old, tag)
	}
	switch {
	case products > 0:
		log.Printf("清理上次转码产物: %d 个, %.1f MB", products, float64(freedProd)/1048576)
	case keep && !specChanged:
		log.Printf("沿用上次转码产物（-vkeep）：命中缓存，秒开")
	}
	if parts > 0 {
		log.Printf("清理转码临时文件(.part): %d 个, %.1f MB", parts, float64(freedParts)/1048576)
	}
	if locked > 0 {
		log.Printf("有 %d 个文件被占用未能删除（上一次的 ffmpeg 可能还没退出），下次启动会再试", locked)
	}

	v.mu.Lock()
	v.probes = map[string]probeInfo{}
	v.jobs = map[string]*videoJob{}
	v.mu.Unlock()

	return os.WriteFile(f, []byte(tag+"\n"), 0o644)
}

// Purge 清空转码产物（换素材目录、或转码规格变了时调用）。
// 保留 .profile：否则下次启动会误判成"规格变了"，把刚转好的又删一遍。
func (v *VideoService) Purge() error {
	d := filepath.Join(v.dir, "vtrans")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	ents, err := os.ReadDir(d)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.Name() == profileName {
			continue
		}
		if err := os.RemoveAll(filepath.Join(d, e.Name())); err != nil {
			return err
		}
	}
	v.mu.Lock()
	v.probes = map[string]probeInfo{}
	v.jobs = map[string]*videoJob{}
	v.mu.Unlock()
	return nil
}

// probe 用 ffprobe 取首个视频流的编码和总时长；结果按缓存键记忆。
func (v *VideoService) probe(f *FileItem) probeInfo {
	k := v.key(f)
	v.mu.Lock()
	if pi, ok := v.probes[k]; ok && pi.ok {
		v.mu.Unlock()
		return pi
	}
	v.mu.Unlock()

	pi := probeInfo{}
	path := v.store.absPath(f.RelPath)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, v.ffprobe,
		"-v", "error",
		"-show_entries", "stream=codec_type,codec_name,width,height",
		"-show_entries", "format=duration",
		"-of", "json",
		path)
	out, err := cmd.Output()
	if err != nil {
		pi.err = "无法识别视频编码（ffprobe 探测失败）"
		log.Printf("ffprobe 失败 (%s): %v", f.Name, err)
		return pi
	}
	var po struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &po); err != nil {
		pi.err = "视频信息解析失败"
		return pi
	}
	for _, s := range po.Streams {
		if s.CodecType == "video" {
			pi.codec = strings.ToLower(s.CodecName)
			pi.w, pi.h = s.Width, s.Height
			break
		}
	}
	if pi.codec == "" {
		pi.err = "文件里没有视频流"
		return pi
	}
	if d, err := strconv.ParseFloat(po.Format.Duration, 64); err == nil {
		pi.dur = d
	}
	pi.ok = true
	v.mu.Lock()
	v.probes[k] = pi
	v.mu.Unlock()
	return pi
}

// isBrowserPlayable 判断编码能不能在浏览器里直接播。
func isBrowserPlayable(codec string) bool {
	switch codec {
	case "h264", "avc1", "vp8", "vp9", "av1", "theora":
		return true
	}
	return false
}

// Status 返回该视频当前的可播放状态；必要时会顺手启动转码任务。
func (v *VideoService) Status(f *FileItem) vstatus {
	if !v.Available() {
		return vstatus{State: "unavailable", Msg: "服务器未找到 ffmpeg，无法转码；请安装 ffmpeg 后重试"}
	}
	pi := v.probe(f)
	if !pi.ok {
		return vstatus{State: "error", Msg: pi.err}
	}
	if isBrowserPlayable(pi.codec) {
		return vstatus{State: "ready", Pct: 100, Codec: pi.codec, Source: "original"}
	}
	out := v.outPath(f)
	if st, err := os.Stat(out); err == nil && st.Size() > 0 {
		return vstatus{State: "ready", Pct: 100, Codec: pi.codec, Source: "transcoded"}
	}
	k := v.key(f)
	v.mu.Lock()
	j := v.jobs[k]
	if j != nil && j.state == "done" {
		// 任务记着已完成、但产物已经不在了（多半是清了 cache 目录）→ 当作没转过，重新排队
		delete(v.jobs, k)
		j = nil
	}
	var state, msg string
	var pct int
	if j != nil {
		state, pct, msg = j.state, j.pct, j.msg
	}
	v.mu.Unlock()

	switch state {
	case "failed":
		return vstatus{State: "error", Codec: pi.codec, Msg: msg}
	case "queued", "running":
		// queued 是预热排好的队，worker 还没轮到；对前端一样是"转码中"
		return vstatus{State: "transcoding", Pct: pct, Codec: pi.codec}
	}
	v.startTranscode(f, pi)
	return vstatus{State: "transcoding", Pct: 0, Codec: pi.codec}
}

// ensureJob 取得（或新建）某文件的转码任务，新建时状态是"排队中"。
func (v *VideoService) ensureJob(f *FileItem) *videoJob {
	k := v.key(f)
	v.mu.Lock()
	defer v.mu.Unlock()
	if j, ok := v.jobs[k]; ok {
		return j
	}
	j := &videoJob{state: "queued"}
	v.jobs[k] = j
	return j
}

// setJob 改任务状态（不存在则忽略）。
func (v *VideoService) setJob(f *FileItem, state string, pct int, msg string) {
	k := v.key(f)
	v.mu.Lock()
	if j, ok := v.jobs[k]; ok {
		j.state = state
		j.pct = pct
		j.msg = msg
	}
	v.mu.Unlock()
}

// claimJob 把一个"排队中"的任务认领成本 worker 在跑。
// 已经被别人跑起来了（或已经跑完）则返回 false —— 防止预热和懒转码同时写同一个输出文件。
func (v *VideoService) claimJob(f *FileItem) (*videoJob, bool) {
	k := v.key(f)
	v.mu.Lock()
	defer v.mu.Unlock()
	j, ok := v.jobs[k]
	if !ok {
		j = &videoJob{state: "running"}
		v.jobs[k] = j
		return j, true
	}
	if j.state != "queued" {
		return j, false
	}
	j.state = "running"
	j.pct = 0
	return j, true
}

// releaseQueued 清掉"排了队但一直没跑"的任务（上一轮预热被取消时留下的僵尸）。
func (v *VideoService) releaseQueued() {
	v.mu.Lock()
	for k, j := range v.jobs {
		if j.state == "queued" {
			delete(v.jobs, k)
		}
	}
	v.mu.Unlock()
}

func (v *VideoService) startTranscode(f *FileItem, pi probeInfo) {
	k := v.key(f)
	v.mu.Lock()
	if _, exists := v.jobs[k]; exists {
		v.mu.Unlock()
		return
	}
	j := &videoJob{state: "running"}
	v.jobs[k] = j
	v.mu.Unlock()

	log.Printf("视频转码开始: %s (编码 %s, %dx%d, %.1fs)", f.Name, pi.codec, pi.w, pi.h, pi.dur)
	go func() {
		// 懒转码没有外部 ctx（用户点开才触发，不该被预热那轮的取消连坐）
		dims, err := v.transcode(context.Background(), f, pi, j)
		v.mu.Lock()
		if err != nil {
			j.state = "failed"
			j.msg = err.Error()
			v.mu.Unlock()
			log.Printf("视频转码失败 %s: %v", f.Name, err)
			return
		}
		j.state = "done"
		j.pct = 100
		v.mu.Unlock()
		log.Printf("视频转码完成: %s → %s", f.Name, dims)
	}()
}

// ---------- 预热：启动 / 每次扫描完成后预检 + 预转码 ----------

// WarmupState 是一次预热的进度快照，控制台打印和 /api/vtrans-warmup 都用它。
type WarmupState struct {
	Active     bool   `json:"active"`
	Total      int    `json:"total"`    // 库里视频总数
	Probed     int    `json:"probed"`   // 已探测编码数
	NeedConv   int    `json:"needConv"` // 预检判定"浏览器播不了、要转码"的数量
	Skipped    int    `json:"skipped"`  // 编码本来就浏览器可播 → 直接跳过，不转
	Cached     int    `json:"cached"`   // 以前转过、命中缓存 → 不重复转
	Done       int    `json:"done"`     // 本轮转码成功数
	Failed     int    `json:"failed"`   // 探测失败或转码失败数
	Current    string `json:"current"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt"`
	ElapsedMs  int64  `json:"elapsedMs"`
}

func (v *VideoService) Warmup() WarmupState {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.warm
}

func (v *VideoService) warmSet(fn func(*WarmupState)) {
	v.mu.Lock()
	fn(&v.warm)
	v.mu.Unlock()
}

// WarmUp 预检库里所有视频，只把"确实播不了"的排进转码队列。
//
// 这就是"再放一个检测，不一定都要转码"：先 ffprobe 逐个读文件头判断编码，
// H.264/VP8/VP9/AV1 直接跳过，已有转码产物的也跳过，剩下的才排队转。
// 两阶段：阶段一并发探测（很快），阶段二按 conc 并发转码（慢）。
// 阻塞直到全部结束；ctx 取消时尽快返回（换素材目录时会取消上一轮）。
func (v *VideoService) WarmUp(ctx context.Context, conc int) {
	if conc < 1 {
		conc = 1
	}
	// 上一轮可能被取消（换素材目录），留下"排队中"的僵尸任务会让前端一直显示
	// "转码中 0%"，所以开新轮之前先清掉没跑起来的那些。
	v.releaseQueued()

	files, err := v.store.AllVideos()
	if err != nil {
		log.Printf("视频预检: 读取视频列表失败: %v", err)
		return
	}
	started := time.Now()
	v.warmSet(func(w *WarmupState) {
		*w = WarmupState{Active: true, Total: len(files), StartedAt: started.Unix()}
	})
	// 先把每个视频都登记成"排队中"：这样预检还没轮到时用户点开，前端看到的也是
	// "转码中"，不会走懒转码那条路去跟预热抢同一个输出文件。
	for i := range files {
		v.ensureJob(&files[i])
	}

	if len(files) == 0 {
		log.Println("视频预检: 素材里没有视频，无需转码")
		v.warmDone(started)
		return
	}
	if !v.Available() {
		log.Printf("视频预检: %d 个视频，但没找到 ffmpeg —— 跳过转码（HEVC 等在浏览器里将无法播放）", len(files))
		v.warmSet(func(w *WarmupState) { w.Failed = len(files) })
		v.warmDone(started)
		return
	}

	log.Printf("视频预检: 共 %d 个视频，开始逐个探测编码", len(files))

	// —— 阶段一：并发 ffprobe 预检 ——
	type todo struct {
		f  FileItem
		pi probeInfo
	}
	var (
		qmu    sync.Mutex
		toConv []todo
		skip   int
		cached int
		broken int
	)
	var wg sync.WaitGroup
	sem := make(chan struct{}, conc)
	for i := range files {
		if ctx.Err() != nil {
			break
		}
		f := files[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			pi := v.probe(&f)
			v.warmSet(func(w *WarmupState) { w.Probed++; w.Current = f.Name })
			switch {
			case !pi.ok:
				v.setJob(&f, "failed", 0, pi.err)
				qmu.Lock()
				broken++
				qmu.Unlock()
			case isBrowserPlayable(pi.codec):
				// 编码本来就浏览器可播 —— 这就是"不一定都要转码"，直接跳过
				v.setJob(&f, "done", 100, "")
				qmu.Lock()
				skip++
				qmu.Unlock()
			default:
				if st, err := os.Stat(v.outPath(&f)); err == nil && st.Size() > 0 {
					v.setJob(&f, "done", 100, "")
					qmu.Lock()
					cached++
					qmu.Unlock()
					return
				}
				qmu.Lock()
				toConv = append(toConv, todo{f, pi})
				qmu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	v.warmSet(func(w *WarmupState) {
		w.NeedConv = len(toConv)
		w.Skipped = skip
		w.Cached = cached
		w.Failed = broken
		w.Current = ""
	})
	log.Printf("视频预检完成: 需转码 %d，已有缓存 %d，编码可直接播 %d，探测失败 %d",
		len(toConv), cached, skip, broken)

	if len(toConv) == 0 {
		v.warmDone(started)
		return
	}

	// —— 阶段二：排队转码 ——
	workers := conc
	if workers > len(toConv) {
		workers = len(toConv)
	}
	log.Printf("视频转码: 开始（并发 %d）", workers)

	queue := make(chan todo)
	var wg2 sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			for it := range queue {
				if ctx.Err() != nil {
					return
				}
				v.warmSet(func(w *WarmupState) { w.Current = it.f.Name })
				j, mine := v.claimJob(&it.f)
				if !mine {
					// 已经有别的路径在转这个文件了（比如用户手快先点开了），不重复跑
					if _, err := os.Stat(v.outPath(&it.f)); err == nil {
						v.warmSet(func(w *WarmupState) { w.Done++ })
					}
					continue
				}
				t0 := time.Now()
				dims, err := v.transcode(ctx, &it.f, it.pi, j)
				if err != nil {
					v.setJob(&it.f, "failed", 0, err.Error())
					log.Printf("  转码失败: %s: %v", it.f.Name, err)
					v.warmSet(func(w *WarmupState) { w.Failed++ })
					continue
				}
				var mb float64
				if st, serr := os.Stat(v.outPath(&it.f)); serr == nil {
					mb = float64(st.Size()) / 1048576
				}
				v.setJob(&it.f, "done", 100, "")
				log.Printf("  转码完成: %s %dx%d → %s（%s, %.1f MB）",
					it.f.Name, it.pi.w, it.pi.h, dims, time.Since(t0).Round(time.Second), mb)
				v.warmSet(func(w *WarmupState) { w.Done++ })
			}
		}()
	}
	go func() {
		defer close(queue)
		for _, it := range toConv {
			select {
			case queue <- it:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg2.Wait()
	v.warmDone(started)
}

func (v *VideoService) warmDone(started time.Time) {
	el := time.Since(started)
	v.warmSet(func(w *WarmupState) {
		w.Active = false
		w.Current = ""
		w.FinishedAt = time.Now().Unix()
		w.ElapsedMs = el.Milliseconds()
	})
	st := v.Warmup()
	log.Printf("视频就绪: 共 %d 个 —— 需转码 %d（完成 %d / 命中缓存 %d / 失败 %d），可直接播 %d；耗时 %s",
		st.Total, st.NeedConv, st.Done, st.Cached, st.Failed, st.Skipped, el.Round(time.Second))
}

// transcode 用 ffmpeg 转成 H.264 + faststart，先写 .part 再改名，避免半成品被当成成品。
// 返回产物分辨率描述（例如 "1558x720"），给控制台日志用。
//
// ctx 取消时会连 ffmpeg 一起杀掉（CommandContext）：换素材目录会取消上一轮预热，
// 以前用 exec.Command 是杀不掉的 —— 旧 ffmpeg 还在后台跑，继续占满 CPU，
// 还会把上一批素材的产物写到新目录的缓存里，且文件被占用导致下次启动清不掉。
func (v *VideoService) transcode(ctx context.Context, f *FileItem, pi probeInfo, j *videoJob) (string, error) {
	src := v.store.absPath(f.RelPath)
	out := v.outPath(f)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	tmp := out + ".part"
	defer os.Remove(tmp)

	// 目标尺寸按 720p 算：较小的一边封顶 720，另一边等比。源本来就小就不缩放、不放大。
	tw, th, needScale := v.profile.targetSize(pi.w, pi.h)
	dims := fmt.Sprintf("%dx%d", tw, th)
	if !needScale {
		dims += "(原分辨率)"
	}

	// veryfast + crf26 兼顾速度与体积；+faststart 让 moov 前置，浏览器才能边下边播。
	args := []string{
		"-hide_banner", "-y",
		"-i", src,
	}
	if needScale {
		args = append(args, "-vf", fmt.Sprintf("scale=%d:%d", tw, th))
	}
	args = append(args,
		"-c:v", "libx264", "-preset", v.profile.preset, "-crf", strconv.Itoa(v.profile.crf),
		"-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k",
		"-movflags", "+faststart",
		"-progress", "pipe:1", "-nostats",
		// 必须显式指定封装格式：临时文件名是 xxx.mp4.part，
		// ffmpeg 靠扩展名猜格式，".part" 会报 "Invalid argument"
		"-f", "mp4",
		tmp,
	)
	cmd := exec.CommandContext(ctx, v.ffmpeg, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("启动 ffmpeg 失败: %v", err)
	}

	total := pi.dur * 1e6 // 微秒
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "out_time_us=") && !strings.HasPrefix(line, "out_time_ms=") {
			continue
		}
		// 注意：ffmpeg 的 out_time_ms 其实也是微秒（历史遗留）
		raw := strings.TrimSpace(strings.SplitN(line, "=", 2)[1])
		if raw == "N/A" {
			continue
		}
		us, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || total <= 0 {
			continue
		}
		p := int(float64(us) / total * 100)
		if p < 0 {
			p = 0
		}
		if p > 99 {
			p = 99
		}
		v.mu.Lock()
		j.pct = p
		v.mu.Unlock()
	}
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("ffmpeg 转码失败: %s", tailLine(stderr.String()))
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", err
	}
	return dims, nil
}

func tailLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(无错误输出)"
	}
	lines := strings.Split(s, "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(last) > 200 {
		last = last[:200]
	}
	return last
}

// handleVTransWarmup 给页面/排查用：预热进度（还在不在转、总共几个、转好几个）。
func (h *Handler) handleVTransWarmup(w http.ResponseWriter, r *http.Request) {
	if h.video == nil {
		writeJSON(w, WarmupState{})
		return
	}
	writeJSON(w, h.video.Warmup())
}

// handleVTransStatus 给前端查询：能直接播 / 正在转码(百分比) / 无法播放。
func (h *Handler) handleVTransStatus(w http.ResponseWriter, r *http.Request) {
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
	if f.Kind != kindVideo {
		writeJSON(w, vstatus{State: "ready", Pct: 100, Source: "original"})
		return
	}
	writeJSON(w, h.video.Status(f))
}

// handleVTrans 输出"浏览器能播的版本"：本来就是 H.264 等直接给原文件；
// HEVC 等则给转码结果（走 http.ServeFile，支持 Range，可拖动进度条）。
func (h *Handler) handleVTrans(w http.ResponseWriter, r *http.Request) {
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
	if f.Kind != kindVideo {
		http.ServeFile(w, r, h.store.absPath(f.RelPath))
		return
	}
	st := h.video.Status(f)
	if st.State != "ready" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(st)
		return
	}
	if st.Source == "original" {
		http.ServeFile(w, r, h.store.absPath(f.RelPath))
		return
	}
	p := h.video.outPath(f)
	if _, err := os.Stat(p); err != nil {
		http.Error(w, "转码结果不存在", http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, p)
}
