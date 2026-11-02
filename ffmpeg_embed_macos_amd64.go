//go:build darwin && amd64

package main

// 内嵌 ffmpeg（macOS Intel 版）：把 tools/ffmpeg-macos-amd64/{ffmpeg,ffprobe}
// 编进二进制，首次运行释放到 ~/Library/Caches/medreview/ffmpeg-macos-amd64。
// 构建前需把 macOS amd64 的 ffmpeg/ffprobe 放进该目录（不入库）。
//
// ImageMagick 在 macOS 没有官方静态单文件版，不内嵌：用 brew install imagemagick
// 提供 magick（findTool 会走 PATH）。

import (
	"embed"
)

//go:embed tools/ffmpeg-macos-amd64/ffmpeg tools/ffmpeg-macos-amd64/ffprobe
var macAmdToolsEmbed embed.FS

// embeddedToolsDir 返回内嵌 ffmpeg 的释放目录；失败返回空串。
func embeddedToolsDir() string {
	return extractEmbedded(macAmdToolsEmbed, "tools/ffmpeg-macos-amd64")
}

// embeddedMagickDir macOS 不内嵌 ImageMagick（见文件头注释）。
func embeddedMagickDir() string { return "" }

// magickEnv 无内嵌 magick，无需额外环境变量。
func magickEnv(home string) []string { return nil }
