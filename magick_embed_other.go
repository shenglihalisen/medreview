//go:build !windows

package main

// 非 Windows 没有内嵌 ImageMagick（tools/imagemagick 里是 Windows 版 exe，
// 且本工具的目标平台就是 Windows）。空实现让 findTool / magickEnv 的调用
// 在所有平台都能编译。
func embeddedMagickDir() string { return "" }

func magickEnv(home string) []string { return nil }