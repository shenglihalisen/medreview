//go:build wails

package main

// buildMode 见 mode_console.go 的说明。
//
// wails 版（medreview_ui.exe）的控制台是 **WebView2 本地窗口**（见 gui_wails.go）：
// 窗口内容是 HTML/CSS（网页级好看），但不是浏览器 —— 它直接从进程内读状态和日志，
// 不经过 HTTP、不需要口令，也就不会把口令之类的信息暴露到网页里。
// 而且能就地改素材根目录 / 监听地址 / 下载口令，改完立刻生效，不用重启程序。
const buildMode = "wails"
