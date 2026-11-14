package main

// ai_worker.go AI 推理 worker（由主程序以 `-qcai-worker` 启动自己）。
//
// 2026-11-14 起改用 purego 系的纯 Go ONNX 绑定（无 cgo），所以推理可以直接
// 编进主程序，全平台 CGO_ENABLED=0 单文件。但**仍然跑在独立子进程里**：
//   1. ONNX Runtime 是原生库，崩了会带崩整个进程，隔离开不牵连主程序；
//   2. 主程序可以开多个 worker 组成进程池，吃满 QC 的并发。
// 子进程就是 `os.Executable()` 加一个参数，不再需要单独的 qcai.exe。
//
// 协议（stdin/stdout 各一行 JSON）与之前的 qcai 完全一致，主程序侧无需感知。

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"strings"

	ort "github.com/getcharzp/onnxruntime_purego"
)

const aiInputSize = 224

// aiClsOrder 模型输出列序。4 列：训练时含 exposure，交付只取 blur/noise/dirt。
var aiClsOrder = []string{"blur", "noise", "exposure", "dirt"}

type aiScores struct {
	Blur  float64 `json:"blur"`
	Noise float64 `json:"noise"`
	Dirt  float64 `json:"dirt"`
}

type aiReply struct {
	Scores  aiScores `json:"scores"`
	Patches int      `json:"patches"`
}

// runAIWorker 供 main 在检测到 -qcai-worker 时直接调用，返回进程退出码。
func runAIWorker(args []string) int {
	fs := flag.NewFlagSet("qcai-worker", flag.ContinueOnError)
	lib := fs.String("lib", "", "ONNX Runtime 动态库路径")
	legacy := fs.String("dll", "", "兼容旧参数名")
	model := fs.String("model", "", "ONNX 模型路径")
	img := fs.String("img", "", "单张图片（调试用）")
	selftest := fs.Bool("selftest", false, "加载模型并跑一次合成图")
	stdinMode := fs.Bool("stdin", false, "常驻模式：每行一个图片路径，每行一个 JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *lib == "" {
		*lib = *legacy
	}
	if *lib == "" || *model == "" {
		fmt.Fprintln(os.Stderr, "必须提供 -lib 与 -model")
		return 2
	}

	eng, err := ort.NewEngine(*lib)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载 ONNX Runtime 失败（%s）: %v\n", *lib, err)
		return 1
	}
	defer eng.Close()
	sess, err := eng.NewSession(*model, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载模型失败（%s）: %v\n", *model, err)
		return 1
	}
	defer sess.Close()

	if *selftest {
		gray := image.NewGray(image.Rect(0, 0, aiInputSize, aiInputSize))
		for i := range gray.Pix {
			gray.Pix[i] = 128
		}
		if _, _, err := aiScore(sess, gray); err != nil {
			fmt.Fprintln(os.Stderr, "自检前向失败:", err)
			return 1
		}
		fmt.Println(`{"ok":true}`)
		return 0
	}
	if *stdinMode {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			path := strings.TrimSpace(sc.Text())
			if path == "" {
				continue
			}
			line, err := aiOne(sess, path)
			if err != nil {
				out, _ := json.Marshal(map[string]string{"error": err.Error()})
				fmt.Println(string(out))
				continue
			}
			fmt.Println(line)
		}
		return 0
	}
	if *img == "" {
		fmt.Fprintln(os.Stderr, "必须提供 -img（或 -selftest / -stdin）")
		return 2
	}
	line, err := aiOne(sess, *img)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(line)
	return 0
}

func aiOne(sess *ort.Session, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("打开图片失败: %w", err)
	}
	defer f.Close()
	im, _, err := image.Decode(f)
	if err != nil {
		return "", fmt.Errorf("解码失败（CR3/HEIC 等格式不支持）: %w", err)
	}
	sc, n, err := aiScore(sess, im)
	if err != nil {
		return "", fmt.Errorf("推理失败: %w", err)
	}
	out, _ := json.Marshal(aiReply{Scores: sc, Patches: n})
	return string(out), nil
}

// aiScore 3×3 裁 224 → NCHW float32 RGB/255 → 前向 → sigmoid → 按块平均。
// 预处理必须与训练一致：不做 ImageNet mean/std 归一化。
func aiScore(sess *ort.Session, im image.Image) (aiScores, int, error) {
	crops := aiGridCrops(im, aiInputSize)
	n := len(crops)
	data := make([]float32, 0, n*3*aiInputSize*aiInputSize)
	for _, c := range crops {
		data = append(data, c...)
	}
	in, err := ort.NewTensor([]int64{int64(n), 3, aiInputSize, aiInputSize}, data)
	if err != nil {
		return aiScores{}, 0, err
	}
	defer in.Close()
	outs, err := sess.Run(map[string]*ort.Value{"input": in})
	if err != nil {
		return aiScores{}, 0, err
	}
	ov, ok := outs["logits"]
	if !ok {
		return aiScores{}, 0, fmt.Errorf("模型缺少 logits 输出")
	}
	defer ov.Close()
	flat, err := ort.GetTensorData[float32](ov)
	if err != nil {
		return aiScores{}, 0, err
	}
	cols := len(aiClsOrder)
	if len(flat) != n*cols {
		return aiScores{}, 0, fmt.Errorf("输出长度 %d != %d", len(flat), n*cols)
	}
	var s aiScores
	for i := 0; i < n; i++ {
		r := flat[i*cols : (i+1)*cols]
		// 取 blur(0) / noise(1) / dirt(3)；exposure(2) 不接管（AUC≈0.55）
		s.Blur += aiSigmoid(float64(r[0]))
		s.Noise += aiSigmoid(float64(r[1]))
		s.Dirt += aiSigmoid(float64(r[3]))
	}
	s.Blur /= float64(n)
	s.Noise /= float64(n)
	s.Dirt /= float64(n)
	return s, n, nil
}

// aiGridCrops 3×3 网格裁块；小图最近邻放大到能裁 9 块（与训练/评测口径一致）。
//
// 2026-11-14 性能修正：旧实现先把整张图逐像素铺平成 w*h*3 的 float32 平面，
// 4K 图就是 16M 次 im.At() + 192MB 分配，实测 1.2 秒/张；而 9 个 224 块只用到
// 45 万像素（2.8%）。现在直接在 9 个块里取像素，省掉全图铺平。
func aiGridCrops(im image.Image, size int) []([]float32) {
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
	crops := make([]([]float32), 0, 9)
	plane := size * size
	for gy := 0; gy < 3; gy++ {
		for gx := 0; gx < 3; gx++ {
			y0 := b.Min.Y + gy*(h-size)/2
			x0 := b.Min.X + gx*(w-size)/2
			chw := make([]float32, 3*plane)
			for y := 0; y < size; y++ {
				for x := 0; x < size; x++ {
					r, g, bl, _ := im.At(x0+x, y0+y).RGBA()
					di := y*size + x
					chw[di] = float32(r>>8) / 255
					chw[plane+di] = float32(g>>8) / 255
					chw[2*plane+di] = float32(bl>>8) / 255
				}
			}
			crops = append(crops, chw)
		}
	}
	return crops
}

func aiSigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }
