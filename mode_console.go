//go:build !gui && !wails

package main

// buildMode 标记这个 exe 是「带黑窗口的控制台版」还是「无黑窗的后台版」。
// 由构建标签决定：
//
//	go build                    → console（默认，有黑窗口，能看到地址/口令/进度）
//	go build -tags gui          → gui（无黑窗，纯 Win32 本地窗口当控制台，备用版）
//	go build -tags wails        → wails（无黑窗，WebView2 网页级本地窗口，默认发行版）
//
// 之所以用构建标签而不是运行时猜：Windows 下 -H=windowsgui 编出来的程序也可能被
// 从已有控制台里启动，靠「有没有控制台窗口」判断不稳定；构建期就定死最省心。
const buildMode = "console"
