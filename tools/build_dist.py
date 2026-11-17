#!/usr/bin/env python3
"""
Build the downloadable payload for a published pack.

Output layout (one directory per pack version):
  <out>/pack.json          the launcher manifest
  <out>/base.tar.gz        every base-layer item, paths mods/<workshop_id>/...
  <out>/client.tar.gz      the client layer (Viewpoint/ZombieBuddy + addons)
  <out>/manifest.json      URLs, sizes and SHA-256 for the three files above
  <out>/upload.sh          ready-to-run S3 commands

The base archive is ~10 GB; expect a few minutes of CPU for gzip -1.

Usage:
  build_dist.py --repo ../pz-server-modpack --pack pack/pack.json \
                --out dist/2026.11.03 --base-url https://bucket.s3.eu-west-1.amazonaws.com/pzpack/2026.11.03
"""

import argparse
import gzip
import hashlib
import json
import os
import shutil
import sys
import tarfile
import time


def sha256_file(path, progress=None):
    h = hashlib.sha256()
    size = os.path.getsize(path)
    done = 0
    with open(path, "rb") as f:
        while True:
            chunk = f.read(1 << 20)
            if not chunk:
                break
            h.update(chunk)
            done += len(chunk)
            if progress:
                progress(done, size)
    return h.hexdigest()


def build_layer(repo, out_path, items, label):
    t0 = time.time()
    tmp = out_path + ".tmp"
    with open(tmp, "wb") as raw:
        with gzip.GzipFile(fileobj=raw, mode="wb", compresslevel=1, mtime=0) as gz:
            with tarfile.open(fileobj=gz, mode="w|") as tar:
                for n, it in enumerate(items, 1):
                    src = os.path.join(repo, "mods", it["workshop_id"])
                    if not os.path.isdir(src):
                        raise SystemExit("missing item folder: %s" % src)
                    tar.add(src, arcname="mods/" + it["workshop_id"], recursive=True)
                    if n % 20 == 0 or n == len(items):
                        print("  %s: %d/%d items (%.1fs)" % (label, n, len(items), time.time() - t0), flush=True)
    os.replace(tmp, out_path)
    size = os.path.getsize(out_path)
    print("  %s: %.2f GB written in %.1fs, hashing…" % (label, size / 1e9, time.time() - t0), flush=True)
    digest = sha256_file(out_path, progress=lambda d, t: None)
    return size, digest


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", required=True, help="pz-server-modpack checkout")
    ap.add_argument("--pack", required=True, help="pack/pack.json")
    ap.add_argument("--out", required=True, help="output directory")
    ap.add_argument("--base-url", required=True, help="public URL prefix where the files will live")
    ap.add_argument("--layers", default="base,client")
    args = ap.parse_args()

    pack = json.load(open(args.pack, encoding="utf-8"))
    base_url = args.base_url.rstrip("/")
    os.makedirs(args.out, exist_ok=True)

    by_layer = {}
    for it in pack["items"]:
        by_layer.setdefault(it["layer"], []).append(it)
    # keep the pack's own order inside each layer
    order = {it["workshop_id"]: i for i, it in enumerate(pack["items"])}
    for layer in by_layer:
        by_layer[layer].sort(key=lambda it: order[it["workshop_id"]])

    shutil.copy2(args.pack, os.path.join(args.out, "pack.json"))
    pack_size = os.path.getsize(os.path.join(args.out, "pack.json"))
    pack_sha = sha256_file(os.path.join(args.out, "pack.json"))

    layers = {}
    for label in args.layers.split(","):
        label = label.strip()
        if not label:
            continue
        items = by_layer.get(label, [])
        if not items:
            print("skip empty layer", label)
            continue
        print("building %s layer (%d items, %.2f GB source)" % (
            label, len(items), sum(it["bytes"] for it in items) / 1e9), flush=True)
        path = os.path.join(args.out, label + ".tar.gz")
        size, digest = build_layer(args.repo, path, items, label)
        layers[label] = {
            "url": "%s/%s.tar.gz" % (base_url, label),
            "bytes": size,
            "sha256": digest,
        }

    manifest = {
        "schema": 1,
        "pack_version": pack["version"],
        "game_build": pack["game_build"],
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "pack_url": base_url + "/pack.json",
        "pack_sha256": pack_sha,
        "pack_bytes": pack_size,
        "layers": layers,
    }
    json.dump(manifest, open(os.path.join(args.out, "manifest.json"), "w"), indent=1)

    policy = {
        "Version": "2012-10-17",
        "Statement": [{
            "Sid": "PublicReadPack",
            "Effect": "Allow",
            "Principal": "*",
            "Action": "s3:GetObject",
            "Resource": "arn:aws:s3:::BUCKET/pzpack/*",
        }],
    }
    json.dump(policy, open(os.path.join(args.out, "bucket-policy.json"), "w"), indent=2)

    upload = [
        "#!/usr/bin/env bash",
        "set -euo pipefail",
        "# usage: BUCKET=... [PREFIX=...] [AWS_PROFILE=...] ./upload.sh",
        'BUCKET="${BUCKET:?set BUCKET}"',
        'PREFIX="${PREFIX:-pzpack/%s}"' % pack["version"],
        'ACS="${ACL:-}"',
        'HERE="$(cd "$(dirname "$0")" && pwd)"',
        'EXTRA=()',
        '[ -n "$ACS" ] && EXTRA+=(--acl "$ACS")',
        'aws s3 cp "$HERE/pack.json" "s3://$BUCKET/$PREFIX/pack.json" --content-type application/json "${EXTRA[@]}"',
    ]
    for label, l in layers.items():
        upload.append('aws s3 cp "$HERE/%s.tar.gz" "s3://$BUCKET/$PREFIX/%s.tar.gz" --content-type application/gzip "${EXTRA[@]}"' % (label, label))
    upload += [
        'aws s3 cp "$HERE/manifest.json" "s3://$BUCKET/$PREFIX/manifest.json" --content-type application/json --cache-control "no-cache" "${EXTRA[@]}"',
        'aws s3 cp "$HERE/manifest.json" "s3://$BUCKET/pzpack/latest.json" --content-type application/json --cache-control "no-cache" "${EXTRA[@]}"',
        'echo "manifest: https://$BUCKET.s3.amazonaws.com/$PREFIX/manifest.json"',
    ]
    with open(os.path.join(args.out, "upload.sh"), "w") as f:
        f.write("\n".join(upload) + "\n")
    os.chmod(os.path.join(args.out, "upload.sh"), 0o755)

    print("\nmanifest written to %s" % os.path.join(args.out, "manifest.json"))
    print("latest.json is uploaded to s3://$BUCKET/pzpack/latest.json")
    print("launcher: pzpack --from <manifest-url> --profile lite|full")


if __name__ == "__main__":
    sys.exit(main())
