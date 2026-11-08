package qc

// ai.go 第 8 个检测器：小模型（MobileNetV3-Small ONNX）「AI 怀疑度」。
//
// 定位与边界（2026-11-07 定稿）：
//   - **独立 bit FlagAISuspect，不接管现有规则**。模型与规则的判定语义不同
//     （模型 = 整图综合分数，规则 = 编译期校准的物理指标），直接替换会破坏
//     用户校准过的阈值与既有工作流。先并存，真实缺陷集验证后再谈接管。
//   - **默认关闭**：需要 -qc-ai 显式开启。推理器缺失/执行失败自动跳过，
//     检测流程照常，绝不因为 AI 起不来而拖垮整个 QC。
//   - **只对图片生效**（第一版）。视频走 runVideo 投票，AI 单帧分数语义不同。
//   - 预处理必须和训练严格一致：RGB、除 255、**不做 ImageNet mean/std 归一化**
//     （训练时就是这么喂的），3×3 网格裁 224，每类取 9 块平均（部署口径，
//     eval_real 实测真实照片误报 0~1.3%）。
//
// 为什么是子进程而不是 cgo：
//   medreview 全线 CGO_ENABLED=0 纯静态（便携单文件的前提）。onnxruntime 的 Go
//   绑定必须 cgo，把主程序染上 cgo 等于放弃这个前提。所以推理放在独立小工具
//   qcai.exe（cgo 构建，随包内嵌释放），主程序只做 exec + JSON 解析：
//   主程序零 cgo、AI 崩溃不牵连主进程、换模型不动主程序。
//
// 模型与阈值来源：私有仓库 qc-ml（cnn-20261107-173740，闸门 4/4 通过）。

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AIInputSize 模型输入边长（必须与训练导出一致）。
const AIInputSize = 224

// AIScores 三类「AI 怀疑度」（0~1，sigmoid 后的概率）。
// 列序与模型输出一致：blur / noise / dirt（exposure 列模型里有，但本项目不接管）。
type AIScores struct {
	Blur  float64 `json:"blur"`
	Noise float64 `json:"noise"`
	Dirt  float64 `json:"dirt"`
}

// AIOut 挂在 QCResult 上的检测结果（detail JSON 可见，排查用）。
type AIOut struct {
	Scores  AIScores `json:"scores"`
	Patches int      `json:"patches"` // 实际推理的块数（图太小会少于 9）
}

// AIDetector 外部推理器（qcai.exe）的封装。nil = 不可用，调用方判空。
//
// qcai 以 **常驻进程 + stdin 管道** 方式运行：dll 只加载一次（约 1s），
// 之后每张图只付解码 + 推理的时间。每张图重启进程会让 1000 张素材的 QC
// 多花十几分钟，所以这里维护一个进程，出错时自动重启。
type AIDetector struct {
	exe   string
	model string
	dll   string

	// pool 是 qcai 进程池。QC 的 runAll 本来就是 3 路并发（sem 容量 3），
	// 单进程 + 一把大锁会把并发压回串行 —— 池化后吞吐直接 ×3。
	// 每个进程各自加载一份 dll/模型（约 120MB 常驻），池大小与 QC 并发数一致。
	pool chan *aiProc

	mu     sync.Mutex
	closed bool

	// modelHash 模型文件指纹：换模型必须让旧 QC 结果失效，否则新模型的分母配旧
	// 模型的分数，语义是错的。由 VersionForAI 混进 qc_version。
	modelHash string
}

// aiProc 一个常驻 qcai 进程。
type aiProc struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

// aiReply qcai 的 stdout 契约（单行 JSON）。
type aiReply struct {
	Scores  AIScores `json:"scores"`
	Patches int      `json:"patches"`
	Error   string   `json:"error"`
}

// NewAIDetector 校验三个文件存在且 qcai 能跑起来。失败返回 error，调用方降级。
func NewAIDetector(exe, model, dll string, workers int) (*AIDetector, error) {
	if exe == "" || model == "" || dll == "" {
		return nil, fmt.Errorf("AI 推理器路径为空（exe=%q model=%q dll=%q）", exe, model, dll)
	}
	for _, p := range []string{exe, model, dll} {
		st, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("缺少 %s: %w", p, err)
		}
		if st.Size() == 0 {
			return nil, fmt.Errorf("%s 是空文件", p)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "-selftest", "-dll", dll, "-model", model).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("qcai 自检失败: %v: %s", err, strings.TrimSpace(string(out)))
	}
	hash := ""
	if h, herr := fileSHA256(model); herr == nil {
		hash = h
	}
	if workers < 1 {
		workers = 1
	}
	a := &AIDetector{exe: exe, model: model, dll: dll, modelHash: hash,
		pool: make(chan *aiProc, workers)}
	for i := 0; i < workers; i++ {
		p, perr := a.start()
		if perr != nil {
			a.Close()
			return nil, fmt.Errorf("启动第 %d 个推理进程: %w", i+1, perr)
		}
		a.pool <- p
	}
	return a, nil
}

// ModelHash 模型文件前 16 位 sha256（空 = 没算出来，不影响检测）。
func (a *AIDetector) ModelHash() string {
	if a == nil {
		return ""
	}
	return a.modelHash
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// start 拉起一个 qcai 常驻进程。
func (a *AIDetector) start() (*aiProc, error) {
	cmd := exec.Command(a.exe, "-dll", a.dll, "-model", a.model, "-stdin")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr // 协议走 stdout；stderr 直接透传，qcai 崩了能在主日志里看到 traceback
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 qcai: %w", err)
	}
	return &aiProc{cmd: cmd, in: in, out: bufio.NewReader(out)}, nil
}

// kill 终止单个进程。
func (p *aiProc) kill() {
	if p == nil {
		return
	}
	if p.in != nil {
		p.in.Close()
	}
	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Kill()
		p.cmd.Wait()
	}
}

// retire 处死坏进程并补一个新鲜的，保持池容量不缩水。
func (a *AIDetector) retire(p *aiProc) {
	p.kill()
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return
	}
	if np, err := a.start(); err == nil {
		select {
		case a.pool <- np:
		default:
			np.kill() // 池满（并发收尾竞态）：多出来的直接关掉
		}
	}
}

// judgeVideoAI 视频：抽代表帧跑 AI，与图片同口径（mean）。
//
// 抽帧**保持原分辨率**：缩放会把"糊"变清楚，与训练时的 224 原图像素口径对不上，
// 判断会系统性地偏松。每 100 帧取一帧（25fps ≈ 4 秒），最多 5 帧。
func (s *QCService) judgeVideoAI(ctx context.Context, src string, r *QCResult) {
	if s.ai == nil || s.ffmpeg == "" {
		return
	}
	th := s.CurrentThresholds()
	dir, err := os.MkdirTemp("", "qcai-vid-")
	if err != nil {
		log.Printf("QC: %s AI 抽帧目录创建失败: %v", src, err)
		return
	}
	defer os.RemoveAll(dir)
	pat := filepath.Join(dir, "f%03d.jpg")
	cmd := exec.CommandContext(ctx, s.ffmpeg,
		"-v", "error", "-i", src,
		"-vf", "select=not(mod(n\\,100))",
		"-frames:v", "5", "-q:v", "3", pat)
	if out, cerr := cmd.CombinedOutput(); cerr != nil {
		log.Printf("QC: %s AI 抽帧失败: %v: %s", src, cerr, strings.TrimSpace(string(out)))
		return
	}
	files, _ := filepath.Glob(filepath.Join(dir, "f*.jpg"))
	if len(files) == 0 {
		log.Printf("QC: %s AI 抽帧为空（视频太短？），跳过 AI", src)
		return
	}
	var sc AIScores
	n := 0
	for _, f := range files {
		if got, _, e := s.ai.ScoreFile(f); e == nil {
			sc.Blur += got.Blur
			sc.Noise += got.Noise
			sc.Dirt += got.Dirt
			n++
		}
	}
	if n == 0 {
		return
	}
	sc.Blur /= float64(n)
	sc.Noise /= float64(n)
	sc.Dirt /= float64(n)
	r.AI = &AIOut{Scores: sc, Patches: n}
	if sc.Blur > th.AIBlur || sc.Noise > th.AINoise || sc.Dirt > th.AIDirt {
		r.Flags |= FlagAISuspect
	}
}

// ScoreFile 对一张图片跑推理，返回 mean 口径的三类分数与参与平均的块数。
func (a *AIDetector) ScoreFile(path string) (AIScores, int, error) {
	if a == nil {
		return AIScores{}, 0, fmt.Errorf("AI 检测器不可用")
	}
	// 从池里借一个进程（池空则等待，最多 30s）
	var p *aiProc
	select {
	case p = <-a.pool:
	case <-time.After(30 * time.Second):
		return AIScores{}, 0, fmt.Errorf("等待 AI 推理进程超时（池已满 30s）")
	}
	bad := true
	defer func() {
		if bad {
			a.retire(p)
		} else {
			a.pool <- p
		}
	}()

	if _, err := fmt.Fprintln(p.in, path); err != nil {
		return AIScores{}, 0, fmt.Errorf("写入 qcai: %w", err)
	}
	// 读是阻塞的，用 goroutine + 超时兜底：卡死时杀进程，不让 QC 整体拖住。
	type readResult struct {
		line string
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, err := p.out.ReadString('\n')
		ch <- readResult{line, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			return AIScores{}, 0, fmt.Errorf("读取 qcai 输出: %w", res.err)
		}
		var rep aiReply
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.line)), &rep); err != nil {
			return AIScores{}, 0, fmt.Errorf("解析 qcai 输出: %w（原文 %q）", err, res.line)
		}
		if rep.Error != "" {
			// 单文件失败（CR3/HEIC 解不了）不代表进程坏了，进程还能继续用
			bad = false
			return AIScores{}, 0, fmt.Errorf("qcai: %s", rep.Error)
		}
		bad = false
		return rep.Scores, rep.Patches, nil
	case <-time.After(60 * time.Second):
		return AIScores{}, 0, fmt.Errorf("qcai 超时（60s）")
	}
}

// Close 关闭池里所有进程（进程退出前调用）。
func (a *AIDetector) Close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	for {
		select {
		case p := <-a.pool:
			p.kill()
		default:
			return
		}
	}
}
