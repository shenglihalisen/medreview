//go:build !wails

package main

// 只有 wails 构建才有 WebView2 本地窗口（gui_wails.go）。
// console / gui 两种构建走到不到那个分支，这里给个空实现让 main.go 三种构建都能编译。
func runWails(a *app) {}
