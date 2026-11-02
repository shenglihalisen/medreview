//go:build linux && !slim

package main

// 内嵌 ffmpeg（Linux 版）：把 tools/ffmpeg-linux/{ffmpeg,ffprobe} 编进二进制，
// 首次运行释放到用户缓存目录（$XDG_CACHE_HOME/medreview/ffmpeg-linux，通常是
// ~/.cache/medreview/...）。这样二进制拷到任何 Linux 机器都能直接转码 / 预览，
// 不再依赖目标机装了 ffmpeg，也不需要旁边带 tools 目录。
//
// 与 Windows 版的差异：Linux 的内嵌文件没有执行位，释放后必须 chmod 0755；
// 配置/依赖都走系统库（jellyfin-ffmpeg 构建，动态链接 glibc，需 >= 2.28）。
// 释放逻辑见 embed_extract.go（Linux / macOS 共用）。
//
// slim 构建（-tags slim）不内嵌，见 ffmpeg_embed_linux_slim.go —— fnOS 包用 slim，
// 工具外置在 app/tools/ffmpeg/，安装后不额外占一份缓存。

import (
	"embed"
)

//go:embed tools/ffmpeg-linux/ffmpeg tools/ffmpeg-linux/ffprobe
var linuxToolsEmbed embed.FS

// embeddedToolsDir 返回内嵌 ffmpeg 的释放目录；失败返回空串。
func embeddedToolsDir() string {
	return extractEmbedded(linuxToolsEmbed, "tools/ffmpeg-linux")
}
