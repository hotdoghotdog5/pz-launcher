#!/usr/bin/env python3
"""Regenerate pack/pack.json from the frozen repo's manifest.post-fix.json."""
import json, os, sys, heapq, collections

repo = sys.argv[1] if len(sys.argv) > 1 else "../pz-server-modpack"
out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "pack", "pack.json")
m = json.load(open(os.path.join(repo, "manifest.post-fix.json")))
client = {"3619862853","3809306528","3811256204","3811344119","3813194971","3814398576",
          "3815019204","3817413313","3818189653","3818700431","3820965001","3821525282",
          "3822854313","3828755546"}

def read_id(d):
    import glob
    for pat in ("mod.info", "*/mod.info", "*/*/mod.info"):
        for f in sorted(glob.glob(os.path.join(d, pat))):
            try:
                for line in open(f, encoding="utf-8", errors="replace"):
                    if line.strip().startswith("id="):
                        return line.split("=", 1)[1].strip()
            except OSError:
                pass
    return os.path.basename(d)


def has_info(d, depth=2):
    if os.path.exists(os.path.join(d, "mod.info")):
        return True
    if depth <= 0:
        return False
    try:
        subs = [x for x in os.listdir(d) if os.path.isdir(os.path.join(d, x))]
    except OSError:
        return False
    return any(has_info(os.path.join(d, x), depth - 1) for x in subs)

items = []
by_mod = {}
for idx, e in enumerate(m["mods"]):
    wid = e["workshop_id"]
    item = os.path.join(repo, e["path"])
    mods_dir = os.path.join(item, "mods")
    srcs = []
    if os.path.isdir(mods_dir):
        for d in sorted(os.listdir(mods_dir)):
            p = os.path.join(mods_dir, d)
            if os.path.isdir(p) and has_info(p):
                srcs.append({"name": d, "src": f'{e["path"]}/mods/{d}', "mod_id": read_id(p)})
    if not srcs and has_info(item):
        srcs = [{"name": os.path.basename(item), "src": e["path"], "mod_id": read_id(item)}]
    if not srcs:
        srcs = [{"name": os.path.basename(item), "src": e["path"], "mod_id": read_id(item)}]
    req = [d.lstrip("\\").lower() for d in (e.get("requires") or [])]
    items.append({"workshop_id": wid, "title": e["title"],
                  "layer": "client" if wid in client else "base",
                  "folders": srcs, "bytes": e["byte_count"],
                  "tree_sha256": e["tree_sha256"], "mod_ids": e.get("mod_ids", []),
                  "requires": req, "idx": idx})
    for mid in e.get("mod_ids", []):
        by_mod[mid.lower()] = wid

indeg = {i["workshop_id"]: 0 for i in items}
adj = collections.defaultdict(list)
for it in items:
    for d in it["requires"]:
        p = by_mod.get(d)
        if p and p != it["workshop_id"]:
            adj[p].append(it["workshop_id"])
            indeg[it["workshop_id"]] += 1
idx = {i["workshop_id"]: i["idx"] for i in items}
heap = [(idx[w], w) for w, d in indeg.items() if d == 0]
heapq.heapify(heap)
order = []
while heap:
    _, w = heapq.heappop(heap)
    order.append(w)
    for v in adj[w]:
        indeg[v] -= 1
        if indeg[v] == 0:
            heapq.heappush(heap, (idx[v], v))
order += [i["workshop_id"] for i in items if i["workshop_id"] not in order]
for it in items:
    it.pop("idx")
base = sum(i["bytes"] for i in items if i["layer"] == "base")
cl = sum(i["bytes"] for i in items if i["layer"] == "client")
pack = {"schema": 1, "name": "PZ Server Modpack", "version": "2026.11.03",
        "game_build": "42.21.0", "base_bytes": base, "client_bytes": cl,
        "profiles": {"lite": {"label": "Compatible - stubs the Viewpoint/ZombieBuddy layer (joins the same server, no visuals)",
                              "layers": ["base", "client"], "approx_bytes": base, "stub_layer": "client"},
                     "full": {"label": "Full - real Viewpoint + ZombieBuddy visual layer",
                              "layers": ["base", "client"], "approx_bytes": base + cl}},
        "load_order": order, "items": items}
json.dump(pack, open(out, "w"), indent=1)
print(f"wrote {out}: {len(items)} items, {sum(len(i['folders']) for i in items)} folders, "
      f"base {base/1e9:.2f} GB, client {cl/1e9:.2f} GB")
