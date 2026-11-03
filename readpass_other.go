//go:build !windows

package main

import (
	"os"
	"os/exec"
)

// readPasswordTTY 用 stty 关回显；非终端直接读一行。
func readPasswordTTY() (string, error) {
	if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
		return readLine() // 管道输入
	}
	// stty 不可用时退化为回显读取，但至少能走通
	if err := exec.Command("stty", "-echo").Run(); err == nil {
		defer exec.Command("stty", "echo").Run()
	}
	return readLine()
}
