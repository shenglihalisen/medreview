//go:build !windows && !linux

package main

import (
	"fmt"
	"runtime"
)

// printGPUInfo 的 macOS / 其他平台版本：Windows 分支靠注册表枚举显卡，
// 给 start.bat 决定要问哪些问题；macOS 没有这套交互，-gpuinfo 只保留可用性，
// 输出一句说明后正常退出（构建模式判定不受影响）。
func printGPUInfo() {
	fmt.Printf("gpuinfo: 当前平台（%s）不提供显卡枚举；macOS 上转码/检测默认走 CPU\n", runtime.GOOS)
}
