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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
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

	mu  sync.Mutex // 单进程串行；QC 并发时排队
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
func NewAIDetector(exe, model, dll string) (*AIDetector, error) {
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
	return &AIDetector{exe: exe, model: model, dll: dll}, nil
}

// ensure 懒启动常驻进程；已有进程直接返回。
func (a *AIDetector) ensure() error {
	if a.cmd != nil {
		return nil
	}
	cmd := exec.Command(a.exe, "-dll", a.dll, "-model", a.model, "-stdin")
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr // 协议走 stdout；stderr 直接透传，qcai 崩了能在主日志里看到 traceback
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 qcai: %w", err)
	}
	a.cmd, a.in, a.out = cmd, in, bufio.NewReader(out)
	return nil
}

// kill 终止常驻进程；下次调用会重新拉起。
func (a *AIDetector) kill() {
	if a.cmd == nil {
		return
	}
	if a.in != nil {
		a.in.Close()
	}
	if a.cmd.Process != nil {
		a.cmd.Process.Kill()
	}
	a.cmd.Wait()
	a.cmd, a.in, a.out = nil, nil, nil
}

// ScoreFile 对一张图片跑推理，返回 mean 口径的三类分数与参与平均的块数。
func (a *AIDetector) ScoreFile(path string) (AIScores, int, error) {
	if a == nil {
		return AIScores{}, 0, fmt.Errorf("AI 检测器不可用")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.ensure(); err != nil {
		return AIScores{}, 0, err
	}
	if _, err := fmt.Fprintln(a.in, path); err != nil {
		a.kill()
		return AIScores{}, 0, fmt.Errorf("写入 qcai: %w", err)
	}
	// 读是阻塞的，用 goroutine + 超时兜底：卡死时杀进程，不让 QC 整体拖住。
	type readResult struct {
		line string
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, err := a.out.ReadString('\n')
		ch <- readResult{line, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			a.kill()
			return AIScores{}, 0, fmt.Errorf("读取 qcai 输出: %w", res.err)
		}
		var rep aiReply
		if err := json.Unmarshal([]byte(strings.TrimSpace(res.line)), &rep); err != nil {
			return AIScores{}, 0, fmt.Errorf("解析 qcai 输出: %w（原文 %q）", err, res.line)
		}
		if rep.Error != "" {
			return AIScores{}, 0, fmt.Errorf("qcai: %s", rep.Error)
		}
		return rep.Scores, rep.Patches, nil
	case <-time.After(60 * time.Second):
		a.kill()
		return AIScores{}, 0, fmt.Errorf("qcai 超时（60s）")
	}
}

// Close 关闭常驻进程（进程退出前调用）。
func (a *AIDetector) Close() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.kill()
}
