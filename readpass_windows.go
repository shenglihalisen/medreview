//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// readPasswordTTY 关掉控制台回显读一行；非控制台（管道）回退到普通读行。
func readPasswordTTY() (string, error) {
	h := windows.Handle(os.Stdin.Fd())
	var old uint32
	if err := windows.GetConsoleMode(h, &old); err != nil {
		return readLine() // 不是控制台，按管道处理
	}
	// 去掉回显位；Ctrl+C 仍然有效
	if err := windows.SetConsoleMode(h, old&^windows.ENABLE_ECHO_INPUT); err != nil {
		return readLine()
	}
	defer windows.SetConsoleMode(h, old)
	return readLine()
}
