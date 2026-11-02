//go:build linux && slim

package main

// slim 构建：不内嵌 ffmpeg / ImageMagick，二进制只有 ~12 MB。
// 工具按 findTool 的顺序外置：程序目录的 tools/ffmpeg/、tools/imagemagick/，再到 PATH。
// fnOS 包用这个模式（app/tools/ffmpeg/ 随包带工具，安装后不额外占一份缓存）。

func embeddedToolsDir() string  { return "" }
func embeddedMagickDir() string { return "" }
func magickEnv(home string) []string { return nil }
