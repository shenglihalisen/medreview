//go:build (!windows && !linux && !darwin) || (linux && !amd64) || (darwin && !amd64 && !arm64)

package main

// 未覆盖的平台（如 linux/arm64）：没有内嵌动态库，AI 检测自动禁用。
// 以后要支持，放一份对应平台的 ORT 库并加一个 ai_embed_lib_*.go 即可。
func aiLibAsset() (string, []byte) { return "", nil }
