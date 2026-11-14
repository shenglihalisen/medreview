//go:build windows

package main

import _ "embed"

// 本平台内嵌的 ONNX Runtime 动态库（ORT 1.23.0，API 23，MIT）。
//go:embed tools/onnxruntime/win-x64/onnxruntime.dll
var aiLibEmbed []byte

// aiLibAsset 返回释放到缓存目录时使用的文件名与内容。
func aiLibAsset() (string, []byte) {
	return "onnxruntime.dll", aiLibEmbed
}
