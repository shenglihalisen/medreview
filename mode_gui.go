//go:build gui

package main

// buildMode 见 mode_console.go 的说明。
//
// gui 版（纯 Win32 手写控件那个备用版）的控制台是**本地桌面窗口**（见 gui_win32.go 的 runGUI），
// 不是浏览器里的网页面板 —— 双击启动不弹黑窗口，弹出一个 Windows 窗口，
// 里面显示本机/局域网地址、下载口令、实时日志，还有「打开下载页」「停止服务」按钮。
// 整个控制台完全不依赖浏览器，符合"不要网页"的要求。
const buildMode = "gui"
