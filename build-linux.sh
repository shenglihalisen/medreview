#!/usr/bin/env bash
# Build a headless Linux binary for medreview (cross-compile friendly).
# Usage: ./build-linux.sh [output-path]
set -euo pipefail
cd "$(dirname "$0")"

OUT="${1:-dist/medreview-linux-amd64}"
mkdir -p "$(dirname "$OUT")"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "$OUT" .
echo "built: $OUT"
echo "note: ffmpeg/ffprobe are not embedded on Linux;"
echo "      put them next to the binary under tools/ffmpeg/ (or install them in PATH)."
