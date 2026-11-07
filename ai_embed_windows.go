//go:build windows

package main

// 内嵌 AI 推理资源：qcai.exe（cgo 构建的独立推理器）+ ONNX 模型 + onnxruntime.dll。
//
// 为什么内嵌：medreview 的便携包是"拷到哪都能用"。AI 功能如果依赖外部文件，
// 少一个 dll 就整条检测链静默失效。和 ffmpeg 一样内嵌，首次运行释放到
// %LocalAppData%\medreview\tools\qcml\，之后按大小比对复用。
//
// 与 ffmpeg 的差别：这里失败**不致命**（AI 默认关闭），返回空串由调用方降级。

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
)

//go:embed tools/qcml/qcai.exe tools/qcml/qc_quality.onnx tools/onnxruntime/onnxruntime.dll
var aiEmbed embed.FS

const aiDirName = "qcml"

// aiEmbedded 内嵌文件名 → embed 内的路径
var aiEmbedded = []struct{ name, path string }{
	{"qcai.exe", "tools/qcml/qcai.exe"},
	{"qc_quality.onnx", "tools/qcml/qc_quality.onnx"},
	{"onnxruntime.dll", "tools/onnxruntime/onnxruntime.dll"},
}

var (
	aiOnce sync.Once
	aiExe  string
	aiMdl  string
	aiDll  string
)

// embeddedAIFiles 确保内嵌资源已释放，返回 (qcai.exe, model, dll) 路径。
// 任一环节失败返回三个空串——AI 检测器随之禁用，其余 QC 不受影响。
func embeddedAIFiles() (string, string, string) {
	aiOnce.Do(func() {
		base, err := os.UserCacheDir()
		if err != nil {
			log.Printf("AI: 取缓存目录失败（%v），AI 检测不可用", err)
			return
		}
		dir := filepath.Join(base, "medreview", "tools", aiDirName)
		if need, total := aiNeedExtract(dir); need {
			log.Printf("首次运行：释放 AI 推理资源（共 %d MB）到 %s", total>>20, dir)
			if err := aiExtract(dir); err != nil {
				log.Printf("AI: 释放失败（%v），AI 检测不可用", err)
				return
			}
			log.Printf("AI 推理资源释放完成")
		}
		aiExe = filepath.Join(dir, "qcai.exe")
		aiMdl = filepath.Join(dir, "qc_quality.onnx")
		aiDll = filepath.Join(dir, "onnxruntime.dll")
	})
	return aiExe, aiMdl, aiDll
}

func aiNeedExtract(dir string) (bool, int64) {
	var total int64
	need := false
	for _, f := range aiEmbedded {
		st, err := fs.Stat(aiEmbed, f.path)
		if err != nil {
			return true, 0
		}
		total += st.Size()
		cur, err := os.Stat(filepath.Join(dir, f.name))
		if err != nil || cur.Size() != st.Size() {
			need = true
		}
	}
	return need, total
}

func aiExtract(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range aiEmbedded {
		src, err := aiEmbed.Open(f.path)
		if err != nil {
			return err
		}
		tmp := filepath.Join(dir, f.name+".part")
		dst := filepath.Join(dir, f.name)
		if err := copyToFile(src, tmp); err != nil {
			src.Close()
			os.Remove(tmp)
			return fmt.Errorf("写 %s: %w", tmp, err)
		}
		src.Close()
		os.Remove(dst)
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("就位 %s: %w", dst, err)
		}
	}
	return nil
}
