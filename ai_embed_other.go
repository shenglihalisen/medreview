//go:build !windows

package main

// 非 Windows 平台没有内嵌的 AI 推理资源（qcai 目前只有 Windows/cgo 构建）。
// 返回空串，qc.NewAIDetector 会失败 → AI 检测器禁用，其余 QC 照常。
// 以后要做 fnOS/Linux：用同一套 qcai 源码交叉编译 + 内嵌 libonnxruntime.so 即可。
func embeddedAIFiles() (string, string, string) { return "", "", "" }
