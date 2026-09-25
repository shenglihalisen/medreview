//go:build !gui && !wails

package main

// gui 版（medreview_ui.exe）才有本地桌面窗口；console 版走黑窗口，不需要 runGUI。
// 这里给个空实现 + 同款 config 结构，让 main.go 在两种构建下都能编译、
// 且只在 gui 分支真正调用它。
type guiConfig struct {
	ReviewURL   string
	DownloadURL string
	LANURLs     []string
	Token       string
	NoToken     bool
	LogPath     string
	Logs        *logRing
	OpenBrowser func(string)
	Shutdown    func()
}

func runGUI(cfg guiConfig) {}
