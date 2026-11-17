#!/usr/bin/env python3
"""Point an already-built dist directory at the real bucket URL without rebuilding archives.

Usage: retarget_dist.py --dir dist/2026.11.03 --base-url https://bucket.s3.region.amazonaws.com/pzpack/2026.11.03
"""
import argparse, json, os, time

ap = argparse.ArgumentParser()
ap.add_argument("--dir", required=True)
ap.add_argument("--base-url", required=True)
a = ap.parse_args()

base = a.base_url.rstrip("/")
p = os.path.join(a.dir, "manifest.json")
m = json.load(open(p, encoding="utf-8"))
m["pack_url"] = base + "/pack.json"
for name, layer in m.get("layers", {}).items():
    layer["url"] = "%s/%s.tar.gz" % (base, name)
m["generated_at"] = time.strftime("%Y-%m-%dT%H:%M:%S%z")
json.dump(m, open(p, "w"), indent=1)
print("manifest now points at", base)
for name, layer in m.get("layers", {}).items():
    print(" ", name, layer["url"], layer["bytes"], "bytes")
