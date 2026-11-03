package main

// 交互式读取密码（不回显）。启动时问超管密码用。
//
// 刻意不引第三方依赖：
//   - Windows：改控制台模式去掉 ENABLE_ECHO_INPUT，读完恢复
//   - Linux/macOS：stty -echo / stty echo
//   - 非终端（管道/服务）：直接读一行，方便 `echo pass | medreview ...` 自动化

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// 共享一个 Scanner：每次 new 一个会各自缓冲，第二次读就 EOF
// （向导要连续问"数量→账号→密码…"，踩过这个坑）。
var stdinScanner = bufio.NewScanner(os.Stdin)

// readLine 读一行（去掉行尾换行）。
func readLine() (string, error) {
	if !stdinScanner.Scan() {
		if err := stdinScanner.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("没有读到输入")
	}
	return strings.TrimRight(stdinScanner.Text(), "\r\n"), nil
}

// promptPassword 打印提示并读密码（终端下不回显）。
func promptPassword(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	s, err := readPasswordTTY()
	if err != nil {
		return "", err
	}
	fmt.Fprintln(os.Stderr)
	return strings.TrimSpace(s), nil
}
