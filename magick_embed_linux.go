//go:build linux && !slim

package main

// 内嵌 ImageMagick（Linux 版）：把 tools/imagemagick-linux/magick 编进二进制，
// 首次运行释放到用户缓存。用于 QC / 预览解 RAW、HEIC、AVIF、JXL、PSD 等
// Go 标准库和 ffmpeg 都不认的格式；缺失不影响主流程（走 formatRegistry 的降级路径）。
//
// 用的是 ImageMagick 官方的静态单文件构建（magick），不需要同目录 xml/icc 配置，
// 与 Windows 版（magick.exe + 一整套配置）不同，释放逻辑共用 extractEmbedded。

import (
	"embed"
)

//go:embed tools/imagemagick-linux/magick
var linuxMagickEmbed embed.FS

// embeddedMagickDir 返回内嵌 magick 的释放目录；失败返回空串（findTool 继续走 PATH）。
func embeddedMagickDir() string {
	return extractEmbedded(linuxMagickEmbed, "tools/imagemagick-linux")
}

// magickEnv 给调用方拼额外环境变量。Linux 用的是官方静态单文件构建，
// 配置已编进二进制，不需要 MAGICK_HOME / MAGICK_CONFIGURE_PATH（设了反而可能
// 指向不存在的路径导致启动告警），所以返回 nil。
func magickEnv(home string) []string { return nil }
