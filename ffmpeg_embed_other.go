//go:build !windows

package main

// 非 Windows 没有内嵌 ffmpeg（tools/ffmpeg 里是 Windows 版 exe）。
// 空实现让 findTool 的调用在所有平台都能编译。
func embeddedToolsDir() string { return "" }
