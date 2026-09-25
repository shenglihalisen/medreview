//go:build windows

package main

// 内置 ffmpeg：把 tools/ffmpeg 下的 ffmpeg.exe / ffprobe.exe 直接编译进 exe。
//
// 为什么要嵌：exe 可能被拷到任何地方双击运行（比如桌面），那个目录下没有
// tools\ffmpeg，转码/图片预览就废了（界面上会显示「未找到 ffmpeg」）。
// 嵌进去之后**任何一台 Windows 拷过去就能用**，不再依赖绿色文件夹结构。
//
// 释放策略：
//   - 目标位置 %LocalAppData%\medreview\tools\ffmpeg\（桌面/程序目录保持干净，
//     多个 exe 副本共用一份）；
//   - 只在「文件缺失或大小对不上」时才释放（内置版本更新后大小会变，自动重释）；
//   - 先写 .part 再 rename，防止半截文件被并发实例当成品用；
//   - 释放失败不致命：返回空串，findTool 继续走 PATH 兜底。

import (
	"embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
)

//go:embed tools/ffmpeg/ffmpeg.exe tools/ffmpeg/ffprobe.exe
var toolsEmbed embed.FS

const embeddedDirName = "ffmpeg"

var embeddedFileNames = []string{"ffmpeg.exe", "ffprobe.exe"}

var (
	embeddedOnce sync.Once
	embeddedPath string // 释放成功后的 ffmpeg.exe 所在目录；失败为空串
)

// embeddedToolsDir 确保内置 ffmpeg 已释放到本地缓存目录，返回该目录（失败返回空串）。
// 带.sync.Once：整个进程只释放一次。
func embeddedToolsDir() string {
	embeddedOnce.Do(func() {
		base, err := os.UserCacheDir() // Windows = %LocalAppData%
		if err != nil {
			return
		}
		dir := filepath.Join(base, "medreview", "tools", embeddedDirName)

		need, total := checkEmbedded(dir)
		if need {
			log.Printf("首次运行：正在释放内置 ffmpeg/ffprobe（共 %d MB）到 %s", total>>20, dir)
			if err := extractEmbedded(dir); err != nil {
				log.Printf("释放内置 ffmpeg 失败（%v），转码功能不可用", err)
				return
			}
			log.Printf("内置 ffmpeg 释放完成")
		}
		embeddedPath = dir
	})
	return embeddedPath
}

// checkEmbedded 检查目标目录里的两个 exe 是否齐全且大小与内嵌版本一致。
// 返回（是否需要释放, 内嵌总字节数）。
func checkEmbedded(dir string) (bool, int64) {
	var total int64
	need := false
	for _, name := range embeddedFileNames {
		data, err := fs.Stat(toolsEmbed, filepath.ToSlash(filepath.Join("tools", embeddedDirName, name)))
		if err != nil {
			return true, 0 // 内嵌资源本身读不到，按需要释放处理（会报错出来）
		}
		total += data.Size()
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || st.Size() != data.Size() {
			need = true
		}
	}
	return need, total
}

// extractEmbedded 把内嵌的两个 exe 释放到 dir（.part + rename 原子落盘）。
func extractEmbedded(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range embeddedFileNames {
		src, err := toolsEmbed.Open(filepath.ToSlash(filepath.Join("tools", embeddedDirName, name)))
		if err != nil {
			return err
		}
		tmp := filepath.Join(dir, name+".part")
		dst := filepath.Join(dir, name)
		if err := copyToFile(src, tmp); err != nil {
			src.Close()
			os.Remove(tmp)
			return fmt.Errorf("写 %s: %w", tmp, err)
		}
		src.Close()
		// Windows 允许覆盖 rename 前先删旧文件（rename 不能覆盖已存在文件）
		os.Remove(dst)
		if err := os.Rename(tmp, dst); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("就位 %s: %w", dst, err)
		}
	}
	return nil
}

// copyToFile 把 reader 的内容落成一个完整文件。
func copyToFile(src io.Reader, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
