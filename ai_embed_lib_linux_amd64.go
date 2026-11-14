//go:build linux && amd64

package main

import _ "embed"

// 本平台内嵌的 ONNX Runtime 动态库（ORT 1.23.0，API 23，MIT）。
//go:embed tools/onnxruntime/linux-x64/libonnxruntime.so
var aiLibEmbed []byte

// aiLibAsset 返回释放到缓存目录时使用的文件名与内容。
func aiLibAsset() (string, []byte) {
	return "libonnxruntime.so", aiLibEmbed
}
