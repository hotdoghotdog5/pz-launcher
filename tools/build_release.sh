#!/usr/bin/env bash
# Build a player-facing launcher with the manifest URL baked in.
# Usage: tools/build_release.sh <manifest-url> [output]
set -euo pipefail
URL="${1:?usage: build_release.sh <manifest-url> [output]}"
OUT="${2:-dist/pzpack}"
HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(dirname "$HERE")"
export PATH="$ROOT/../tools/go/bin:$PATH"
mkdir -p "$(dirname "$OUT")"
cd "$ROOT"
go build -trimpath -ldflags "-s -w -X main.defaultManifestURL=$URL" -o "$OUT" .
echo "built $OUT (manifest $URL)"
