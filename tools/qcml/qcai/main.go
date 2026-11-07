//go:build windows && cgo

// qcai 是 medreview 的 AI 推理小工具（独立进程，cgo 构建）。
//
// 为什么要拆出来：onnxruntime 的 Go 绑定必须 cgo，而 medreview 主程序坚持
// CGO_ENABLED=0（便携单文件）。把它隔离成独立 exe，主程序 exec 调用，
// 既保住主程序的构建性质，也把 AI 崩溃的风险关在子进程里。
//
// 用法（主程序自动调用，一般不需要手跑）：
//
//	qcai -selftest -dll onnxruntime.dll -model qc_quality.onnx
//	qcai -dll onnxruntime.dll -model qc_quality.onnx -img photo.jpg
//
// 输出：stdout 一行 JSON {"scores":{"blur":..,"noise":..,"dirt":..},"patches":9}
// 出错走 stderr + 非零退出码（主程序据此留痕跳过，不冒充"测过没问题"）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"bufio"
	"math"
	"os"
	"strings"

	ort "github.com/yalue/onnxruntime_go"
)

const inputSize = 224

// clsOrder 模型输出列序（与 qc-ml 导出一致）。
// 注意是 **4 列**：训练时含 exposure，交付只取 blur/noise/dirt，
// 但预分配输出 tensor 必须按模型真实形状来，否则 ORT 报维度不符。
var clsOrder = []string{"blur", "noise", "exposure", "dirt"}

type scores struct {
	Blur  float64 `json:"blur"`
	Noise float64 `json:"noise"`
	Dirt  float64 `json:"dirt"`
}

type reply struct {
	Scores  scores `json:"scores"`
	Patches int    `json:"patches"`
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func main() {
	var dll, model, img string
	var selftest, stdinMode bool
	flag.StringVar(&dll, "dll", "", "onnxruntime 动态库路径")
	flag.StringVar(&model, "model", "", "ONNX 模型路径")
	flag.StringVar(&img, "img", "", "待检测图片路径")
	flag.BoolVar(&selftest, "selftest", false, "自检：加载模型并跑一张合成图")
	flag.BoolVar(&stdinMode, "stdin", false, "常驻模式：每行读一个图片路径，每行输出一个 JSON（省掉重复加载 dll）")
	flag.Parse()

	if dll == "" || model == "" {
		fail("必须提供 -dll 与 -model")
	}
	ort.SetSharedLibraryPath(dll)
	if err := ort.InitializeEnvironment(); err != nil {
		fail("初始化 onnxruntime 失败（dll=%s）: %v", dll, err)
	}
	defer ort.DestroyEnvironment()

	opts, err := ort.NewSessionOptions()
	if err != nil {
		fail("创建会话选项失败: %v", err)
	}
	defer opts.Destroy()
	sess, err := ort.NewDynamicAdvancedSession(
		model, []string{"input"}, []string{"logits"}, opts)
	if err != nil {
		fail("加载模型失败（%s）: %v", model, err)
	}
	defer sess.Destroy()

	if selftest {
		// 纯灰图：只验证 dll + 模型 + 一次前向能跑通，不校验分数语义
		gray := image.NewGray(image.Rect(0, 0, inputSize, inputSize))
		for i := range gray.Pix {
			gray.Pix[i] = 128
		}
		_, _, err := score(sess, gray)
		if err != nil {
			fail("自检前向失败: %v", err)
		}
		fmt.Println(`{"ok":true}`)
		return
	}
	if stdinMode {
		// 常驻模式：主程序 qc/ai.go 用它批量调用。dll 只加载一次，单张约 50~100ms。
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			path := strings.TrimSpace(sc.Text())
			if path == "" {
				continue
			}
			line, err := one(sess, path)
			if err != nil {
				out, _ := json.Marshal(map[string]string{"error": err.Error()})
				fmt.Println(string(out))
				continue
			}
			fmt.Println(line)
		}
		return
	}
	if img == "" {
		fail("必须提供 -img（或 -selftest / -stdin）")
	}
	line, err := one(sess, img)
	if err != nil {
		fail("%v", err)
	}
	fmt.Println(line)
}

// one 处理单张图片并返回一行 JSON。
func one(sess *ort.DynamicAdvancedSession, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("打开图片失败: %w", err)
	}
	defer f.Close()
	im, _, err := image.Decode(f)
	if err != nil {
		return "", fmt.Errorf("解码失败（CR3/HEIC 等格式不支持）: %w", err)
	}
	sc, n, err := score(sess, im)
	if err != nil {
		return "", fmt.Errorf("推理失败: %w", err)
	}
	out, _ := json.Marshal(reply{Scores: sc, Patches: n})
	return string(out), nil
}

// score 3×3 裁 224 → NCHW float32 RGB/255 → 前向 → sigmoid → 按块平均（mean 口径）。
// 预处理必须与训练一致：不做 ImageNet mean/std 归一化。
func score(sess *ort.DynamicAdvancedSession, im image.Image) (scores, int, error) {
	crops := gridCrops(im, inputSize)
	n := len(crops)
	data := make([]float32, 0, n*3*inputSize*inputSize)
	for _, c := range crops {
		data = append(data, c...)
	}
	in, err := ort.NewTensor(ort.NewShape(int64(n), 3, inputSize, inputSize), data)
	if err != nil {
		return scores{}, 0, err
	}
	defer in.Destroy()
	// DynamicAdvancedSession.Run 要求预分配输出 tensor
	out, err := ort.NewEmptyTensor[float32](ort.NewShape(int64(n), int64(len(clsOrder))))
	if err != nil {
		return scores{}, 0, err
	}
	defer out.Destroy()
	if err := sess.Run([]ort.Value{in}, []ort.Value{out}); err != nil {
		return scores{}, 0, err
	}
	flat := out.GetData()
	if len(flat) != n*len(clsOrder) {
		return scores{}, 0, fmt.Errorf("输出长度 %d != %d", len(flat), n*len(clsOrder))
	}
	rows := make([][]float32, n)
	for i := range rows {
		rows[i] = flat[i*len(clsOrder) : (i+1)*len(clsOrder)]
	}
	var s scores
	for _, r := range rows {
		// 取 blur(0) / noise(1) / dirt(3)；exposure(2) 不接管（AUC 0.55，见 qc-ml 结论）
		s.Blur += sigmoid(float64(r[0]))
		s.Noise += sigmoid(float64(r[1]))
		s.Dirt += sigmoid(float64(r[3]))
	}
	s.Blur /= float64(n)
	s.Noise /= float64(n)
	s.Dirt /= float64(n)
	return s, n, nil
}

// gridCrops 3×3 网格裁块；小图最近邻放大到能裁 9 块（与 eval_real 口径一致）。
func gridCrops(im image.Image, size int) []([]float32) {
	b := im.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < size || h < size {
		f := float64(size) / float64(min(w, h))
		nw, nh := int(float64(w)*f)+1, int(float64(h)*f)+1
		canvas := image.NewRGBA(image.Rect(0, 0, nw, nh))
		for y := 0; y < nh; y++ {
			sy := b.Min.Y + y*h/nh
			for x := 0; x < nw; x++ {
				canvas.Set(x, y, im.At(b.Min.X+x*w/nw, sy))
			}
		}
		im, b, w, h = canvas, canvas.Bounds(), nw, nh
	}
	rgb := make([]float32, w*h*3)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, _ := im.At(b.Min.X+x, b.Min.Y+y).RGBA()
			o := (y*w + x) * 3
			rgb[o], rgb[o+1], rgb[o+2] = float32(r>>8)/255, float32(g>>8)/255, float32(bl>>8)/255
		}
	}
	crops := make([]([]float32), 0, 9)
	for gy := 0; gy < 3; gy++ {
		for gx := 0; gx < 3; gx++ {
			y0 := gy * (h - size) / 2
			x0 := gx * (w - size) / 2
			chw := make([]float32, 3*size*size)
			for y := 0; y < size; y++ {
				for x := 0; x < size; x++ {
					si := (y0+y)*w + (x0 + x)
					di := y*size + x
					chw[di] = rgb[si*3]
					chw[size*size+di] = rgb[si*3+1]
					chw[2*size*size+di] = rgb[si*3+2]
				}
			}
			crops = append(crops, chw)
		}
	}
	return crops
}

func sigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }
