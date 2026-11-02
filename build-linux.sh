#!/usr/bin/env bash
# Build the headless Linux binary for medreview.
#
#   ./build-linux.sh            fat single-file build:
#                               embeds ffmpeg/ffprobe + ImageMagick; the binary runs
#                               anywhere (first run extracts tools to ~/.cache/medreview)
#   ./build-linux.sh --slim     slim build (~12 MB): tools are external
#                               (tools/ffmpeg/, tools/imagemagick/, or PATH) - fnOS package uses this
#
# Usage: ./build-linux.sh [--slim] [output-path]
set -euo pipefail
cd "$(dirname "$0")"

SLIM=0
if [ "${1:-}" = "--slim" ]; then SLIM=1; shift; fi
OUT="${1:-dist/medreview-linux-amd64}"
mkdir -p "$(dirname "$OUT")"

TAGS=""
if [ "$SLIM" = "1" ]; then
    TAGS="-tags slim"
    echo "mode: slim  (external tools: tools/ffmpeg/, tools/imagemagick/, or PATH)"
else
    echo "mode: fat   (embedding ffmpeg + ImageMagick)"
    for f in tools/ffmpeg-linux/ffmpeg tools/ffmpeg-linux/ffprobe tools/imagemagick-linux/magick; do
        if [ ! -f "$f" ]; then
            echo "missing: $f"
            echo "put the Linux binaries there first (see README 2.3), or use --slim"
            exit 1
        fi
    done
fi

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $TAGS -trimpath -ldflags "-s -w" -o "$OUT" .
echo "built: $OUT  ($(du -h "$OUT" | cut -f1))"
