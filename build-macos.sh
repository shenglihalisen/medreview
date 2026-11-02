#!/usr/bin/env bash
# Build the headless macOS binary for medreview.
#
#   ./build-macos.sh              host arch (arm64 on Apple silicon, amd64 on Intel)
#   ./build-macos.sh arm64|amd64
#   ./build-macos.sh universal    lipo both into one universal binary (macOS only)
#
set -euo pipefail
cd "$(dirname "$0")"

ARCH="${1:-$(uname -m)}"
OUTDIR="dist"
mkdir -p "$OUTDIR"

build() {
    local a="$1" out="$2"
    CGO_ENABLED=0 GOOS=darwin GOARCH="$a" go build -trimpath -ldflags "-s -w" -o "$out" .
    echo "built: $out ($(du -h "$out" | cut -f1))"
}

case "$ARCH" in
arm64 | amd64)
    build "$ARCH" "$OUTDIR/medreview-darwin-$ARCH"
    ;;
universal)
    build arm64 "$OUTDIR/.medreview-arm64"
    build amd64 "$OUTDIR/.medreview-amd64"
    lipo -create -output "$OUTDIR/medreview-darwin-universal" \
        "$OUTDIR/.medreview-arm64" "$OUTDIR/.medreview-amd64"
    rm -f "$OUTDIR/.medreview-arm64" "$OUTDIR/.medreview-amd64"
    echo "built: $OUTDIR/medreview-darwin-universal"
    ;;
*)
    echo "usage: $0 [arm64|amd64|universal]"
    exit 1
    ;;
esac

cat <<'NOTE'

runtime notes:
  - ffmpeg / ffprobe: put them next to the binary under tools/ffmpeg/, or `brew install ffmpeg`
  - ImageMagick (RAW/HEIC): `brew install imagemagick` (provides `magick`)
  - unsigned binary: first launch may be blocked by Gatekeeper. Allow it in
    System Settings > Privacy & Security, or run:
      xattr -d com.apple.quarantine ./medreview
  - hardware acceleration is not wired up for macOS (CPU only)
NOTE
