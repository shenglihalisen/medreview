//go:build !windows || !cgo

// 占位：qcai 真正实现在 main.go（需要 windows + cgo）。
// 这个文件保证任何构建配置下该目录都有可编译的包，
// 否则 `go test ./...` 会报 "build constraints exclude all Go files"。
package main

func main() {}
