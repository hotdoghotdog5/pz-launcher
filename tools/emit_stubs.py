#!/usr/bin/env python3
"""Emit compatibility stubs for every client-layer mod in pack.json.

Usage: emit_stubs.py <out-dir>
Produces <out-dir>/<ModFolder>/mod.info for each client-layer folder.
Players who do not want Viewpoint/ZombieBuddy install these instead of the real mods;
the ids match, so the server's mod list is satisfied while no client content loads.
"""
import json, os, sys

out = sys.argv[1] if len(sys.argv) > 1 else "stubs"
pack = json.load(open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "pack", "pack.json")))
n = 0
for item in pack["items"]:
    if item["layer"] != "client":
        continue
    for f in item["folders"]:
        d = os.path.join(out, f["name"])
        os.makedirs(d, exist_ok=True)
        mid = f.get("mod_id") or f["name"]
        body = (f"name={item['title']}\nid={mid}\n"
                f"description=Compatibility stub for {item['title']}. "
                "The server supplies this mod's gameplay; install the Full profile for its client-side content.\n")
        open(os.path.join(d, "mod.info"), "w").write(body)
        n += 1
print(f"wrote {n} stubs to {out}")
