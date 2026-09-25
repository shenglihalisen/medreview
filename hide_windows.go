//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// hideWindow 让子进程不弹控制台窗口。
//
// 无黑窗版（medreview_ui.exe）里 `cmd /c start "" <url>` 会闪一下黑框 ——
// 用户要的就是"不要黑窗"，闪一下也算有。给 SysProcAttr.HideWindow 置位即可。
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}
