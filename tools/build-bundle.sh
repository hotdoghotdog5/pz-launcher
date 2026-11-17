#!/usr/bin/env bash
# Build a distributable payload: pack.json + mods/ tree.
# Usage: tools/build-bundle.sh <path-to-pz-server-modpack> [out-dir]
set -euo pipefail
SRC="${1:?usage: build-bundle.sh <repo> [out-dir]}"
OUT="${2:-dist}"
VER="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["version"])' "$(dirname "$0")/../pack/pack.json")"
NAME="pzpack-${VER}"
mkdir -p "$OUT/$NAME"
cp -R "$(dirname "$0")/../pack" "$OUT/$NAME/pack"
cp "$(dirname "$0")/../pzpack" "$OUT/$NAME/pzpack" 2>/dev/null || true
echo "copying mod payload from $SRC (this is the big step)…"
rsync -a --info=progress2 "$SRC/mods" "$OUT/$NAME/mods"
echo "bundle at $OUT/$NAME"
echo "archive: tar -C $OUT -czf $OUT/$NAME.tar.gz $NAME   (or zstd)"
