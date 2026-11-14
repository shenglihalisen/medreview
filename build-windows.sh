#!/usr/bin/env bash
# Build the Windows portable binary.
#
# 2026-11-14: AI inference now uses a pure-Go (purego) ONNX binding, so there is
# no separate qcai.exe and no cgo anywhere. One build, CGO_ENABLED=0 throughout.
# The model and the platform ONNX Runtime library are embedded via go:embed.
#
# Prereqs on PATH: go only (no gcc / MinGW needed).
# Embedded assets must be present:
#   tools/qcml/qc_quality.onnx
#   tools/onnxruntime/win-x64/onnxruntime.dll
#   (other platforms embed their own .so / .dylib from tools/onnxruntime/<plat>/)
#
# Usage: ./build-windows.sh [output]
set -euo pipefail
cd "$(dirname "$0")"
OUT="${1:-medreview.exe}"

for f in tools/qcml/qc_quality.onnx tools/onnxruntime/win-x64/onnxruntime.dll; do
    if [ ! -f "$f" ]; then
        echo "missing: $f"
        exit 1
    fi
done

echo "[1/1] $OUT (pure Go, CGO_ENABLED=0)"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$OUT" .
echo "      -> $(du -h "$OUT" | cut -f1)"
echo "done"
