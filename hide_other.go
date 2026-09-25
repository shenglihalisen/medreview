//go:build !windows

package main

import "os/exec"

// hideWindow 非 Windows 没有控制台窗口这个概念，留空实现。
func hideWindow(cmd *exec.Cmd) {}
