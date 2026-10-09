//go:build windows

package main

// 内置 ImageMagick：把 tools/imagemagick 下的 magick.exe + 运行时配置 xml/icc
// 一起编译进 exe，首次运行释放到 %LocalAppData%\medreview\tools\imagemagick\。
//
// 为什么要嵌：与 ffmpeg 同理 —— exe 可能被拷到任何地方双击运行，那个目录下
// 没有 tools\imagemagick，QC 对相机 RAW / HEIC / AVIF / JXL / PSD 的解码就废了。
//
// 与 ffmpeg 的关键差别：magick 不是自包含单文件。它启动时要找同目录的一整套
// 配置文件（delegates.xml / policy.xml / colors.xml / sRGB.icc …），并且依赖
// MAGICK_HOME（老版本用 HOME）来定位这些配置。所以这里把 exe 与配置一起释放到
// 同一个目录，并在启动前通过 magickEnv() 注入 MAGICK_HOME / MAGICK_CONFIGURE_PATH；
// 只释放 exe 而把配置留在原地是跑不起来的。
//
// 释放策略与 ffmpeg 一致：
//   - 目标 %LocalAppData%\medreview\tools\imagemagick\（多个 exe 副本共用一份）；
//   - 只在「文件缺失或大小对不上」时才释放（内置版本更新后大小会变，自动重释）；
//   - 先写 .part 再 rename，防止半截文件被并发实例当成品用；
//   - 释放失败不致命：返回空串，findTool 继续走磁盘 tools/ 与 PATH 兜底。

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
)

//go:embed tools/imagemagick/magick.exe tools/imagemagick/colors.xml tools/imagemagick/configure.xml tools/imagemagick/delegates.xml tools/imagemagick/english.xml tools/imagemagick/locale.xml tools/imagemagick/log.xml tools/imagemagick/mime.xml tools/imagemagick/policy.xml tools/imagemagick/sRGB.icc tools/imagemagick/thresholds.xml tools/imagemagick/type-ghostscript.xml tools/imagemagick/type.xml
var magickEmbed embed.FS

const magickEmbeddedDirName = "imagemagick"

// magickEmbeddedFiles 是要释放到缓存目录的文件（相对 tools/imagemagick/）。
// magick.exe 必须在最前，其余为它启动时读取的配置文件。
var magickEmbeddedFiles = []string{
	"magick.exe",
	"colors.xml", "configure.xml", "delegates.xml", "english.xml", "locale.xml",
	"log.xml", "mime.xml", "policy.xml", "sRGB.icc", "thresholds.xml",
	"type-ghostscript.xml", "type.xml",
}

var (
	magickOnce sync.Once
	magickPath string // 释放成功后的 magick.exe 所在目录；失败为空串
)

// embeddedMagickDir 确保内置 ImageMagick 已释放到本地缓存目录，返回该目录
// （失败返回空串）。带 sync.Once：整个进程只释放一次。
func embeddedMagickDir() string {
	magickOnce.Do(func() {
		base, err := os.UserCacheDir() // Windows = %LocalAppData%
		if err != nil {
			return
		}
		dir := filepath.Join(base, "medreview", "tools", magickEmbeddedDirName)

		need, total := checkMagickEmbedded(dir)
		if need {
			log.Printf("首次运行：正在释放内置 ImageMagick（共 %d MB）到 %s", total>>20, dir)
			if err := extractMagickEmbedded(dir); err != nil {
				log.Printf("释放内置 ImageMagick 失败（%v），RAW/HEIC/AVIF/JXL/PSD 解码不可用", err)
				return
			}
			log.Printf("内置 ImageMagick 释放完成")
		}
		magickPath = dir
	})
	return magickPath
}

// checkMagickEmbedded 检查目标目录里每个文件是否齐全且大小与内嵌版本一致。
func checkMagickEmbedded(dir string) (bool, int64) {
	var total int64
	need := false
	for _, name := range magickEmbeddedFiles {
		data, err := fs.Stat(magickEmbed, "tools/imagemagick/"+name)
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

// extractMagickEmbedded 把内嵌的 exe 与配置释放到 dir（.part + rename 原子落盘）。
func extractMagickEmbedded(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range magickEmbeddedFiles {
		src, err := magickEmbed.Open("tools/imagemagick/" + name)
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

// magickEnv 返回给 magick 子进程用的环境变量：在继承当前环境的基础上，指向
// 内置（或磁盘）ImageMagick 的配置目录，让它能定位 delegates.xml / policy.xml 等。
//
// 同时设置三个变量，覆盖不同 ImageMagick 版本的查找习惯：
//   - MAGICK_HOME           ：ImageMagick 7 首选；
//   - MAGICK_CONFIGURE_PATH ：7.x 显式配置路径；
//   - HOME                  ：6.x 老版本用它找配置（仅当调用者没设过 HOME 时才补）。
func magickEnv(home string) []string {
	if home == "" {
		return nil // 没有可用目录，沿用调用方默认（继承环境）
	}
	env := os.Environ()
	env = append(env, "MAGICK_HOME="+home, "MAGICK_CONFIGURE_PATH="+home)
	hasHome := false
	for _, kv := range env {
		if len(kv) >= 5 && kv[:5] == "HOME=" {
			hasHome = true
			break
		}
	}
	if !hasHome {
		env = append(env, "HOME="+home)
	}
	return env
}