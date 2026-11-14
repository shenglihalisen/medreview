package main

import (
	"embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// 内嵌 AI 资源（模型 + 当前平台的 ONNX Runtime 动态库）。
//
// 2026-11-14：改用纯 Go ONNX 绑定后，不再需要单独的 qcai.exe；主程序把自己
// 以 -qcai-worker 再启动一次即可。这里只负责把「模型」和「本平台的动态库」
// 释放到本地缓存，让 worker 能按路径加载。
//
// 释放策略与 ffmpeg 一致：目标 %LocalAppData%（Windows）/ ~/.cache（Unix）下的
// medreview/tools/qcml2/，按文件大小比对复用，.part + rename 原子落盘。
// 模型全平台共用一份，动态库由 ai_embed_lib_*.go 按 GOOS/GOARCH 提供。

//go:embed tools/qcml/qc_quality.onnx
var aiModelFS embed.FS

const aiModelEmbedPath = "tools/qcml/qc_quality.onnx"

var (
	aiOnce             sync.Once
	aiModelPathCached  string
	aiLibPathCached    string
)

// embeddedAIFiles 返回 (模型路径, 动态库路径)。任一为空表示 AI 不可用，
// 调用方（QCManager）据此降级，其余 QC 不受影响。
func embeddedAIFiles() (string, string) {
	aiOnce.Do(func() {
		libName, libData := aiLibAsset()
		if libName == "" || len(libData) == 0 {
			log.Printf("AI: 当前平台没有内嵌的 ONNX Runtime 库，AI 检测不可用")
			return
		}
		modelData, err := aiModelFS.ReadFile(aiModelEmbedPath)
		if err != nil {
			log.Printf("AI: 读取内嵌模型失败（%v），AI 检测不可用", err)
			return
		}
		base, err := os.UserCacheDir()
		if err != nil {
			log.Printf("AI: 取缓存目录失败（%v），AI 检测不可用", err)
			return
		}
		dir := filepath.Join(base, "medreview", "tools", "qcml2")
		files := []struct {
			name string
			data []byte
		}{
			{"qc_quality.onnx", modelData},
			{libName, libData},
		}
		need, total := false, int64(0)
		for _, f := range files {
			total += int64(len(f.data))
			st, err := os.Stat(filepath.Join(dir, f.name))
			if err != nil || st.Size() != int64(len(f.data)) {
				need = true
			}
		}
		if need {
			log.Printf("首次运行：释放 AI 资源（共 %d MB）到 %s", total>>20, dir)
			if err := aiWriteAll(dir, files); err != nil {
				log.Printf("AI: 释放失败（%v），AI 检测不可用", err)
				return
			}
		}
		aiModelPathCached = filepath.Join(dir, "qc_quality.onnx")
		aiLibPathCached = filepath.Join(dir, libName)
	})
	return aiModelPathCached, aiLibPathCached
}

func aiWriteAll(dir string, files []struct {
	name string
	data []byte
}) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		tmp := filepath.Join(dir, f.name+".part")
		dst := filepath.Join(dir, f.name)
		if err := os.WriteFile(tmp, f.data, 0o644); err != nil {
			return fmt.Errorf("写 %s: %w", tmp, err)
		}
		os.Remove(dst) // Windows 下 rename 不能覆盖已存在文件
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("就位 %s: %w", dst, err)
		}
	}
	return nil
}
