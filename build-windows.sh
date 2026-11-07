#!/usr/bin/env bash
# Build the Windows portable binary (+ the AI inference helper).
#
# Order matters: medreview.exe embeds tools/qcml/qcai.exe, so qcai must exist first.
#   qcai.exe      : CGO_ENABLED=1 (onnxruntime_go is a cgo binding) -> needs gcc
#   medreview.exe : CGO_ENABLED=0 (stays pure-Go, single-file, no runtime deps)
#
# Prereqs on PATH: go, gcc (MinGW-w64; WinLibs works), and the embedded assets:
#   tools/qcml/qc_quality.onnx      (model, from the qc-ml repo)
#   tools/onnxruntime/onnxruntime.dll
#
# Usage: ./build-windows.sh [output]
set -euo pipefail
cd "$(dirname "$0")"
OUT="${1:-medreview.exe}"
: "${CC:=gcc}"
export CC

for f in tools/qcml/qc_quality.onnx tools/onnxruntime/onnxruntime.dll; do
    if [ ! -f "$f" ]; then
        echo "missing: $f"
        exit 1
    fi
done

echo "[1/2] qcai.exe (cgo)"
CGO_ENABLED=1 go build -trimpath -ldflags "-s -w" -o tools/qcml/qcai.exe ./tools/qcml/qcai
echo "      -> $(du -h tools/qcml/qcai.exe | cut -f1)"

echo "[2/2] $OUT (no cgo)"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$OUT" .
echo "      -> $(du -h "$OUT" | cut -f1)"
echo "done"
