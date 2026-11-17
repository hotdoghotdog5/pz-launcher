#!/usr/bin/env bash
# Build player binaries for every supported OS/arch with the manifest URL baked in.
# Usage: tools/build_all.sh <manifest-url> [out-dir]
set -euo pipefail
URL="${1:?usage: build_all.sh <manifest-url> [out-dir]}"
OUT="${2:-dist}"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
export PATH="$ROOT/../tools/go/bin:$PATH"
mkdir -p "$OUT"
cd "$ROOT"
build() { # GOOS GOARCH output
  echo "building $3"
  CGO_ENABLED=0 GOOS="$1" GOARCH="$2" go build -trimpath \
    -ldflags "-s -w -X main.defaultManifestURL=$URL" -o "$OUT/$3" .
}
build darwin  arm64  pzpack-macos-arm64
build darwin  amd64  pzpack-macos-amd64
build windows amd64  pzpack-windows-amd64.exe
build windows arm64  pzpack-windows-arm64.exe
build linux   amd64  pzpack-linux-amd64
echo "done:"
ls -lh "$OUT"/pzpack-*
