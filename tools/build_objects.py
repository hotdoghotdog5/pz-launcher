#!/usr/bin/env python3
r"""
Build per-item, content-addressed objects and the manifest for PZL-12.

Output directory (safe to rerun; nothing outside --out is written):

  <out>/objects/<tree_sha256>.tar.gz   one deterministic archive per pack item, entries mods/<workshop_id>/...
  <out>/pack.json                      copy of --pack (rewritten only with --accept-recomputed)
  <out>/manifest.json                  schema 1 + layers from --legacy-manifest + items_schema 2 + items

Tree hash (checked against pack.json): SHA-256 over the sorted lines "<relpath>\t<sha256 of file>\n",
one line per regular file under mods/<workshop_id>. Directories named .git are skipped, symlinks and
other non-regular entries are skipped, relpaths use "/". Item bytes: sum of the file sizes.

Archives are deterministic: sorted entries, uid/gid 0, empty uname/gname, mtime 0, files 0644,
directories 0755, gzip mtime 0 and no file name, compresslevel 1. The same tree always yields the
same archive sha256, which the tests check.

--base-url is the version directory URL, the same convention as tools/build_dist.py, for example
https://<host>/pzpack/2026.11.09. pack_url becomes <base>/pack.json. The objects root is the parent of
the version directory and is written as objects_base_url, so object urls objects/<tree>.tar.gz resolve
against it.

Exit codes: 0 ok, 1 build error, 2 usage error, 3 tree or bytes mismatch against pack.json
(rerun with --accept-recomputed to adopt the recomputed values).
Requires Python 3.10 or newer.

Example:
  python3 -I tools/build_objects.py --repo ../pz-server-modpack --pack pack/pack.json \
      --out /tmp/pzbuild-objects --base-url https://<host>/pzpack/2026.11.09
"""

import argparse
import gzip
import hashlib
import json
import os
import re
import shutil
import stat
import sys
import tarfile
import time
import zlib

ITEMS_SCHEMA = 2
MANIFEST_SCHEMA = 1
OBJECTS_DIR = "objects"
ARCHIVE_SUFFIX = ".tar.gz"
CHUNK = 1 << 20
GZIP_LEVEL = 1
DIR_MODE = 0o755
FILE_MODE = 0o644
EXIT_OK = 0
EXIT_ERROR = 1
EXIT_USAGE = 2
EXIT_MISMATCH = 3
_SAFE_ID = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}")


class BuildError(Exception):
    """The build cannot produce a correct output."""


class UsageError(BuildError):
    """A command-line value is unusable."""


class VerifyError(BuildError):
    """An existing or reused archive does not match its tree."""


def _default_log(msg):
    print(msg, flush=True)


def _quiet(msg):
    return None


def _stream_sha(stream):
    """Read stream to the end. Returns (sha256 object, bytes read)."""
    h = hashlib.sha256()
    count = 0
    while True:
        chunk = stream.read(CHUNK)
        if not chunk:
            break
        h.update(chunk)
        count += len(chunk)
    return h, count


def sha256_file(path):
    try:
        with open(path, "rb") as f:
            return _stream_sha(f)[0].hexdigest()
    except OSError as exc:
        raise BuildError(f"cannot read {path}: {exc}") from exc


class _HashWriter:
    """File-like wrapper that hashes every byte written through it."""

    def __init__(self, fileobj):
        self._fileobj = fileobj
        self.sha = hashlib.sha256()
        self.count = 0

    def write(self, data):
        self.sha.update(data)
        self.count += len(data)
        return self._fileobj.write(data)

    def flush(self):
        self._fileobj.flush()


class _HashReader:
    """File-like wrapper that hashes every byte read through it."""

    def __init__(self, fileobj):
        self._fileobj = fileobj
        self.sha = hashlib.sha256()
        self.count = 0

    def read(self, size=-1):
        data = self._fileobj.read(size)
        self.sha.update(data)
        self.count += len(data)
        return data


def tree_hash(pairs):
    """pairs: iterable of (relpath, sha256_hex). Hash of the sorted "<relpath>\t<sha>\n" lines."""
    h = hashlib.sha256()
    for line in sorted(f"{rel}\t{digest}\n" for rel, digest in pairs):
        h.update(line.encode("utf-8"))
    return h.hexdigest()


def _rel(base, full):
    return os.path.relpath(full, base).replace(os.sep, "/")


def walk_item(mod_root):
    """Return (files, skipped). files: sorted (relpath, abspath, size) for regular files only."""
    files = []
    skipped = []
    for root, dirs, names in os.walk(mod_root, followlinks=False):
        kept = []
        for d in dirs:
            full = os.path.join(root, d)
            if d == ".git":
                continue
            if os.path.islink(full):
                skipped.append(f"{_rel(mod_root, full)} (symlink)")
                continue
            kept.append(d)
        dirs[:] = kept
        for n in names:
            full = os.path.join(root, n)
            rel = _rel(mod_root, full)
            try:
                st = os.lstat(full)
            except OSError as exc:
                raise BuildError(f"cannot stat {full}: {exc}") from exc
            if stat.S_ISREG(st.st_mode):
                files.append((rel, full, st.st_size))
            else:
                kind = "symlink" if stat.S_ISLNK(st.st_mode) else "not regular"
                skipped.append(f"{rel} ({kind})")
    files.sort()
    skipped.sort()
    return files, skipped


class ItemTree:
    """One pack item as it exists in the worktree."""

    def __init__(self, workshop_id, mod_root, files, skipped):
        self.workshop_id = workshop_id
        self.mod_root = mod_root
        self.files = files  # sorted list of (relpath, abspath, sha256_hex, size)
        self.skipped = skipped
        self.total_bytes = sum(f[3] for f in files)
        self.tree_sha256 = tree_hash((f[0], f[2]) for f in files)

    def expected_members(self):
        """Archive member name -> sha256 of the file it must contain."""
        prefix = f"mods/{self.workshop_id}/"
        return {prefix + rel: digest for rel, _full, digest, _size in self.files}


def scan_item(repo, workshop_id):
    mod_root = os.path.join(repo, "mods", workshop_id)
    if not os.path.isdir(mod_root):
        raise BuildError(f"item {workshop_id}: folder not found: {mod_root}")
    raw, skipped = walk_item(mod_root)
    if not raw:
        raise BuildError(f"item {workshop_id}: no regular files under {mod_root}")
    files = []
    for rel, full, size in raw:
        try:
            with open(full, "rb") as f:
                h, read = _stream_sha(f)
        except OSError as exc:
            raise BuildError(f"item {workshop_id}: cannot read {full}: {exc}") from exc
        if read != size:
            raise BuildError(f"item {workshop_id}: {full} changed size while it was being hashed")
        files.append((rel, full, h.hexdigest(), read))
    return ItemTree(workshop_id, mod_root, files, skipped)


def _archive_entries(tree):
    """Sorted (name, is_dir, fullpath, sha256, size) for every directory and file in the archive."""
    top = "mods/" + tree.workshop_id
    dirs = {"mods", top}
    for rel, _full, _digest, _size in tree.files:
        parts = rel.split("/")
        for i in range(1, len(parts)):
            dirs.add(top + "/" + "/".join(parts[:i]))
    entries = [(name, True, "", "", 0) for name in dirs]
    entries += [(f"{top}/{rel}", False, full, digest, size) for rel, full, digest, size in tree.files]
    entries.sort(key=lambda e: e[0])
    return entries


def _normalise(info):
    info.uid = 0
    info.gid = 0
    info.uname = ""
    info.gname = ""
    info.mtime = 0
    return info


def _write_archive(tree, fileobj):
    gz_args = {"filename": "", "mode": "wb", "fileobj": fileobj, "mtime": 0, "compresslevel": GZIP_LEVEL}
    tar_args = {"mode": "w", "format": tarfile.PAX_FORMAT, "encoding": "utf-8"}
    with gzip.GzipFile(**gz_args) as gz, tarfile.open(fileobj=gz, **tar_args) as tar:
        for name, is_dir, full, digest, size in _archive_entries(tree):
            info = tarfile.TarInfo(name)
            if is_dir:
                info.type = tarfile.DIRTYPE
                info.mode = DIR_MODE
                tar.addfile(_normalise(info))
                continue
            info.type = tarfile.REGTYPE
            info.mode = FILE_MODE
            info.size = size
            try:
                with open(full, "rb") as fh:
                    reader = _HashReader(fh)
                    tar.addfile(_normalise(info), reader)
            except OSError as exc:
                raise BuildError(f"cannot read {full}: {exc}") from exc
            if reader.count != size or reader.sha.hexdigest() != digest:
                raise BuildError(f"{full} changed while its archive was being built")


def write_object(tree, dest):
    """Write the deterministic archive for tree to dest. Returns (sha256_hex, size_bytes)."""
    partial = dest + ".partial"
    try:
        with open(partial, "wb") as raw:
            hashed = _HashWriter(raw)
            _write_archive(tree, hashed)
        os.replace(partial, dest)
    except BaseException:
        if os.path.lexists(partial):
            os.remove(partial)
        raise
    return hashed.sha.hexdigest(), hashed.count


def verify_object(path, expected):
    """Stream the archive and require exactly the members in expected (name -> sha256)."""
    seen = set()
    try:
        with tarfile.open(path, "r:gz") as tar:
            for member in tar:
                if member.isdir():
                    continue
                if not member.isreg():
                    raise VerifyError(f"unexpected member type: {member.name}")
                if member.name not in expected or member.name in seen:
                    raise VerifyError(f"unexpected or duplicate member: {member.name}")
                src = tar.extractfile(member)
                if src is None:
                    raise VerifyError(f"cannot read member {member.name}")
                h, count = _stream_sha(src)
                if count != member.size or h.hexdigest() != expected[member.name]:
                    raise VerifyError(f"content mismatch: {member.name}")
                seen.add(member.name)
    except (tarfile.TarError, OSError, EOFError, zlib.error) as exc:
        raise VerifyError(f"unreadable archive {path}: {exc}") from exc
    missing = sorted(set(expected) - seen)
    if missing:
        raise VerifyError(f"{len(missing)} expected members missing from {path}, first: {missing[0]}")


def ensure_object(tree, objects_dir, reuse_dirs, log=_quiet):
    """Return (how, sha256_hex, size) where how is 'present', 'reused' or 'built'."""
    name = tree.tree_sha256 + ARCHIVE_SUFFIX
    dest = os.path.join(objects_dir, name)
    expected = tree.expected_members()
    if os.path.isfile(dest):
        try:
            verify_object(dest, expected)
            return "present", sha256_file(dest), os.path.getsize(dest)
        except VerifyError as exc:
            log(f"  replacing damaged object {name}: {exc}")
            os.remove(dest)
    for reuse in reuse_dirs:
        for candidate in (os.path.join(reuse, OBJECTS_DIR, name), os.path.join(reuse, name)):
            if not os.path.isfile(candidate):
                continue
            partial = dest + ".partial"
            try:
                if os.path.lexists(partial):
                    os.remove(partial)
            except OSError as exc:
                raise BuildError(f"cannot clear {partial}: {exc}") from exc
            try:
                os.link(candidate, partial)
            except OSError:
                shutil.copy2(candidate, partial)
            try:
                verify_object(partial, expected)
            except VerifyError as exc:
                os.remove(partial)
                log(f"  reuse candidate rejected: {exc}")
                continue
            os.replace(partial, dest)
            return "reused", sha256_file(dest), os.path.getsize(dest)
    sha, size = write_object(tree, dest)
    return "built", sha, size


def _validate_base_url(base_url):
    base = base_url.rstrip("/")
    if not base.startswith(("https://", "http://")):
        raise UsageError(f"--base-url must be an http(s) URL, got {base_url!r}")
    return base


def _write_bytes(path, data):
    tmp = path + ".tmp"
    try:
        with open(tmp, "wb") as f:
            f.write(data)
        os.replace(tmp, path)
    except OSError as exc:
        raise BuildError(f"cannot write {path}: {exc}") from exc


def _print_mismatches(rows, log):
    log("")
    log(f"TREE CHECK: {len(rows)} item(s) differ from pack.json (recomputed from the worktree)")
    for ws, layer, pack_tree, pack_bytes, tree in rows:
        log(f"  {ws} (layer {layer})")
        log(f"    pack.json   tree={pack_tree or '<missing>'} bytes={pack_bytes}")
        log(f"    recomputed  tree={tree.tree_sha256} bytes={tree.total_bytes}")
    log("")


def _differs(it, tree):
    return it.get("tree_sha256") != tree.tree_sha256 or it.get("bytes") != tree.total_bytes


def run_build(repo, pack_path, out_dir, base_url, reuse_dir=None, legacy_manifest=None,
              accept_recomputed=False, legacy_skew_ok=False, log=_default_log):
    """Build out_dir. Returns a dict with 'exit'; on success also 'manifest', 'counts', 'total_bytes'."""
    started = time.time()
    try:
        with open(pack_path, "rb") as f:
            pack_raw = f.read()
        pack = json.loads(pack_raw.decode("utf-8"))
    except (OSError, ValueError) as exc:
        raise BuildError(f"cannot read pack {pack_path}: {exc}") from exc
    version = pack["version"]
    base = _validate_base_url(base_url)
    if not base.endswith("/" + version):
        raise UsageError(f"--base-url must end with /{version} (the version directory), got {base}")
    root = base[: -len(version) - 1]
    items = pack["items"]

    ids = set()
    for it in items:
        ws = str(it["workshop_id"])
        if not _SAFE_ID.fullmatch(ws) or ".." in ws:
            raise BuildError(f"unsafe workshop_id {ws!r}: use letters, digits, '_', '-' or '.' only")
        if ws in ids:
            raise BuildError(f"duplicate workshop_id in pack: {ws}")
        ids.add(ws)

    log(f"scanning {len(items)} items under {repo}/mods")
    trees = []
    for n, it in enumerate(items, 1):
        trees.append(scan_item(repo, str(it["workshop_id"])))
        if n % 25 == 0 or n == len(items):
            log(f"  scanned {n}/{len(items)} ({time.time() - started:.1f}s)")
    for tree in trees:
        if tree.skipped:
            log(f"  note: {tree.workshop_id} has {len(tree.skipped)} skipped entries, e.g. {tree.skipped[0]}")

    by_hash = {}
    for tree in trees:
        by_hash.setdefault(tree.tree_sha256, []).append(tree.workshop_id)
    for digest, ws_list in sorted(by_hash.items()):
        if len(ws_list) > 1:
            raise BuildError(f"items {', '.join(ws_list)} have identical content (tree {digest}); "
                             "one object key cannot serve both")

    mismatches = []
    for it, tree in zip(items, trees, strict=True):
        if _differs(it, tree):
            mismatches.append((str(it["workshop_id"]), it["layer"], it.get("tree_sha256") or "",
                               it.get("bytes"), tree))
    if mismatches:
        _print_mismatches(mismatches, log)
        if not accept_recomputed:
            log("refusing to build: rerun with --accept-recomputed to adopt the recomputed values")
            return {"exit": EXIT_MISMATCH, "mismatches": [m[0] for m in mismatches]}
        for it, tree in zip(items, trees, strict=True):
            if _differs(it, tree):
                it["tree_sha256"] = tree.tree_sha256
                it["bytes"] = tree.total_bytes
        pack_out = (json.dumps(pack, indent=2, ensure_ascii=False) + "\n").encode("utf-8")
        log(f"accepting recomputed values for {len(mismatches)} item(s); pack.json in the output is rewritten")
    else:
        pack_out = pack_raw
    accepted = [m[0] for m in mismatches] if (mismatches and accept_recomputed) else []
    pack_sha = hashlib.sha256(pack_out).hexdigest()

    layers = {}
    if legacy_manifest:
        try:
            with open(legacy_manifest, "rb") as f:
                legacy = json.loads(f.read().decode("utf-8"))
        except (OSError, ValueError) as exc:
            raise BuildError(f"cannot read legacy manifest {legacy_manifest}: {exc}") from exc
        if legacy.get("schema") != MANIFEST_SCHEMA:
            raise BuildError(f"legacy manifest is not schema {MANIFEST_SCHEMA}")
        if legacy.get("pack_version") != version:
            if not legacy_skew_ok:
                raise BuildError(f"legacy manifest is for pack {legacy.get('pack_version')}, not {version}")
            log(f"WARNING: legacy layers are pack {legacy.get('pack_version')}; carried over under pack {version} (--legacy-skew-ok)")
        if legacy.get("pack_sha256") != pack_sha:
            if not legacy_skew_ok:
                raise BuildError(f"legacy manifest was built from a different pack.json (sha256 "
                                 f"{legacy.get('pack_sha256')}, here {pack_sha}); its layers would not match these items")
            log("WARNING: legacy layers were built from a different pack.json (--legacy-skew-ok); old launchers will install the legacy content")
        layers = legacy.get("layers", {})

    objects_dir = os.path.join(out_dir, OBJECTS_DIR)
    try:
        os.makedirs(objects_dir, exist_ok=True)
    except OSError as exc:
        raise BuildError(f"cannot create {objects_dir}: {exc}") from exc
    _write_bytes(os.path.join(out_dir, "pack.json"), pack_out)
    reuse_dirs = [reuse_dir] if reuse_dir else []

    results = {}
    counts = {"built": 0, "reused": 0, "present": 0}
    for n, tree in enumerate(trees, 1):
        if tree.tree_sha256 in results:
            continue
        how, sha, size = ensure_object(tree, objects_dir, reuse_dirs, log)
        results[tree.tree_sha256] = (sha, size)
        counts[how] += 1
        if how == "built":
            log(f"  built {tree.workshop_id}  {tree.tree_sha256[:16]}  {size / 1e6:.1f} MB")
        if n % 25 == 0 or n == len(trees):
            log(f"  objects {n}/{len(trees)} ({time.time() - started:.1f}s)")

    items_out = []
    for it, tree in zip(items, trees, strict=True):
        sha, size = results[tree.tree_sha256]
        items_out.append({
            "workshop_id": tree.workshop_id,
            "layer": it["layer"],
            "tree_sha256": tree.tree_sha256,
            "bytes": tree.total_bytes,
            "folders": [{"name": f["name"], "mod_id": f["mod_id"]} for f in it.get("folders", [])],
            "object": {"url": f"{OBJECTS_DIR}/{tree.tree_sha256}{ARCHIVE_SUFFIX}",
                       "bytes": size, "sha256": sha},
            **({"platforms": list(it["platforms"])} if it.get("platforms") else {}),
        })

    manifest = {
        "schema": MANIFEST_SCHEMA,
        "pack_version": version,
        "game_build": pack["game_build"],
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "pack_url": base + "/pack.json",
        "pack_sha256": pack_sha,
        "pack_bytes": len(pack_out),
        "layers": layers,
        "items_schema": ITEMS_SCHEMA,
        "objects_base_url": root + "/",
        "items": items_out,
    }
    if accepted:
        manifest["accepted_recomputed"] = accepted
    _write_bytes(os.path.join(out_dir, "manifest.json"),
                 (json.dumps(manifest, indent=1) + "\n").encode("utf-8"))

    total_bytes = sum(size for _sha, size in results.values())
    log("")
    log("summary")
    log(f"  items:                  {len(items)}")
    log(f"  distinct objects:       {len(results)} ({total_bytes / 1e9:.2f} GB)")
    log(f"  objects built:          {counts['built']}")
    log(f"  objects reused:         {counts['reused'] + counts['present']} "
        f"(from --reuse: {counts['reused']}, already in out: {counts['present']})")
    log(f"  tree mismatches:        {len(mismatches)} (accepted: {'yes' if accepted else 'no'})")
    log(f"  output:                 {out_dir}")
    log(f"  time:                   {time.time() - started:.1f} s")
    return {"exit": EXIT_OK, "manifest": manifest, "counts": counts,
            "total_bytes": total_bytes, "mismatches": [m[0] for m in mismatches]}


def build_parser():
    ap = argparse.ArgumentParser(description="Build per-item objects and the PZL-12 manifest.")
    ap.add_argument("--repo", required=True,
                    help="pz-server-modpack checkout; mods/<workshop_id> is read, never written")
    ap.add_argument("--pack", required=True, help="pack/pack.json")
    ap.add_argument("--out", required=True, help="output directory, created if missing")
    ap.add_argument("--base-url", required=True,
                    help="public URL of this version's directory, e.g. https://<host>/pzpack/2026.11.09")
    ap.add_argument("--reuse", metavar="DIR",
                    help="directory of already-built objects (DIR/objects/ or DIR/) to hard-link or copy "
                         "when they verify")
    ap.add_argument("--legacy-manifest", metavar="PATH",
                    help="existing schema-1 manifest.json; its layers are carried over unchanged")
    ap.add_argument("--legacy-skew-ok", action="store_true",
                    help="carry legacy layers from an older pack version or pack.json for the transition; "
                         "old launchers then install the legacy content")
    ap.add_argument("--accept-recomputed", action="store_true",
                    help="adopt recomputed tree/bytes where they differ from pack.json "
                         "(rewrites pack.json in --out)")
    return ap


def main(argv=None):
    args = build_parser().parse_args(argv)
    try:
        result = run_build(args.repo, args.pack, args.out, args.base_url, reuse_dir=args.reuse,
                           legacy_manifest=args.legacy_manifest, accept_recomputed=args.accept_recomputed, legacy_skew_ok=args.legacy_skew_ok)
    except UsageError as exc:
        print(f"usage: {exc}", file=sys.stderr)
        return EXIT_USAGE
    except BuildError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_ERROR
    return result["exit"]


if __name__ == "__main__":
    sys.exit(main())
