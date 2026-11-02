//go:build linux && !slim

package main

// 内嵌 ffmpeg（Linux 版）：把 tools/ffmpeg-linux/{ffmpeg,ffprobe} 编进二进制，
// 首次运行释放到用户缓存目录（$XDG_CACHE_HOME/medreview/ffmpeg-linux，通常是
// ~/.cache/medreview/...）。这样二进制拷到任何 Linux 机器都能直接转码 / 预览，
// 不再依赖目标机装了 ffmpeg，也不需要旁边带 tools 目录。
//
// 与 Windows 版的差异：Linux 的内嵌文件没有执行位，释放后必须 chmod 0755；
// 配置/依赖都走系统库（jellyfin-ffmpeg 构建，动态链接 glibc，需 >= 2.28）。
//
// slim 构建（-tags slim）不内嵌，见 ffmpeg_embed_linux_slim.go —— fnOS 包用 slim，
// 工具外置在 app/tools/ffmpeg/，安装后不额外占一份缓存。

import (
	"embed"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

//go:embed tools/ffmpeg-linux/ffmpeg tools/ffmpeg-linux/ffprobe
var linuxToolsEmbed embed.FS

// embeddedToolsDir 返回内嵌 ffmpeg 的释放目录；释放失败返回空串，
// findTool 会继续走 PATH 兜底。
func embeddedToolsDir() string {
	return extractEmbedded(linuxToolsEmbed, "tools/ffmpeg-linux")
}

// extractEmbedded 把 srcDir 下的所有文件释放到用户缓存目录，返回目录路径。
// 与 Windows 版同策略：只按大小比对（内置版本更新后大小会变，自动重释）；
// 先写 .part 再 rename，避免半截文件被并发实例当成品。
func extractEmbedded(fsys embed.FS, srcDir string) string {
	cache, err := os.UserCacheDir()
	if err != nil || cache == "" {
		return ""
	}
	dst := filepath.Join(cache, "medreview", filepath.Base(srcDir))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return ""
	}
	entries, err := fs.ReadDir(fsys, srcDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		name := e.Name()
		src, err := fsys.Open(path.Join(srcDir, name))
		if err != nil {
			continue
		}
		info, err := src.Stat()
		if err != nil {
			src.Close()
			continue
		}
		target := filepath.Join(dst, name)
		if st, err := os.Stat(target); err == nil && st.Size() == info.Size() {
			src.Close()
			continue
		}
		tmp := target + ".part"
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			src.Close()
			continue
		}
		_, cpErr := io.Copy(out, src)
		closeErr := out.Close()
		src.Close()
		if cpErr != nil || closeErr != nil {
			os.Remove(tmp)
			continue
		}
		if err := os.Rename(tmp, target); err != nil {
			os.Remove(tmp)
			continue
		}
		_ = os.Chmod(target, 0o755)
	}
	return dst
}
