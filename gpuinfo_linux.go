//go:build linux

package main

import "fmt"

// printGPUInfo 的 Linux 版：Windows 分支靠注册表枚举显卡，给 start.bat 决定要问哪些问题。
// fnOS/native 包没有这个交互（启动参数由 wizard + cmd 脚本给），这里只保留 -gpuinfo 的可用性，
// 输出一句说明并让调用方照常退出。
func printGPUInfo() {
	fmt.Println("gpuinfo: 本构建为 Linux 原生版，显卡枚举仅 Windows 版提供（fnOS 上请用 -gpuqc 指定）")
}
