"""Offline tests for tools/build_objects.py and tools/upload_objects.py (PZL-12).

No network: the S3 client is a fake, every test works in its own temporary directory, and the
directories are removed afterwards. Run from the project root:

    python3 -I -m unittest discover -s tools/tests -v
"""

import contextlib
import hashlib
import io
import json
import os
import shutil
import sys
import tarfile
import tempfile
import types
import unittest
from unittest import mock

TOOLS_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
if TOOLS_DIR not in sys.path:
    sys.path.insert(0, TOOLS_DIR)

import build_objects as bo  # noqa: E402
import upload_objects as uo  # noqa: E402

BASE = "https://example.invalid/pzpack"
VERSION_1 = "2026.01.01"
VERSION_2 = "2026.01.02"
SPECS = [("111", "base"), ("222", "base"), ("333", "client")]
META_PACK_KEY = f"pzpack/{VERSION_1}/pack.json"
META_MANIFEST_KEY = f"pzpack/{VERSION_1}/manifest.json"


def _quiet(msg):
    return None


def _sha(data):
    return hashlib.sha256(data).hexdigest()


def _read(path):
    with open(path, "rb") as f:
        return f.read()


def _load(path):
    return json.loads(_read(path).decode("utf-8"))


def _dump(path, data):
    with open(path, "w", encoding="utf-8") as f:
        json.dump(data, f)
    return path


def write_files(root, files):
    for rel, data in files.items():
        path = os.path.join(root, *rel.split("/"))
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as f:
            f.write(data)


def make_repo(repo, items):
    for ws, files in items.items():
        write_files(os.path.join(repo, "mods", ws), files)


def write_pack(path, repo, version, specs, corrupt=()):
    """pack.json whose tree_sha256 and bytes come from the worktree, except for corrupt workshop ids."""
    entries = []
    for ws, layer in specs:
        if ws in corrupt:
            tree_sha, size = "0" * 64, 1
        else:
            tree = bo.scan_item(repo, ws)
            tree_sha, size = tree.tree_sha256, tree.total_bytes
        entries.append({
            "workshop_id": ws,
            "layer": layer,
            "title": f"Title {ws}",
            "mod_ids": [f"M{ws}"],
            "requires": [],
            "folders": [{"name": f"F{ws}", "mod_id": f"M{ws}", "src": f"x/{ws}"}],
            "tree_sha256": tree_sha,
            "bytes": size,
        })
    pack = {"schema": 1, "name": "test", "version": version, "game_build": "42.21.0",
            "base_bytes": 0, "client_bytes": 0, "load_order": [], "profiles": {}, "items": entries}
    return _dump(path, pack)


def silent_build_main(argv):
    with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
        return bo.main(argv)


class TreeHashTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="bo-tree-")
        self.repo = os.path.join(self.tmp, "repo")

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def test_known_answer_matches_the_documented_formula(self):
        make_repo(self.repo, {"1": {"a.txt": b"hello", "sub/b.txt": b"world!"}})
        lines = [f"a.txt\t{_sha(b'hello')}\n", f"sub/b.txt\t{_sha(b'world!')}\n"]
        expected = _sha("".join(sorted(lines)).encode("utf-8"))
        tree = bo.scan_item(self.repo, "1")
        self.assertEqual(tree.tree_sha256, expected)
        self.assertEqual(tree.total_bytes, len(b"hello") + len(b"world!"))

    def test_git_directories_and_symlinks_do_not_change_the_hash(self):
        make_repo(self.repo, {"1": {"a.txt": b"hello"}})
        before = bo.scan_item(self.repo, "1")
        mod = os.path.join(self.repo, "mods", "1")
        write_files(os.path.join(mod, ".git"), {"config": b"[core]", "objects/ab/cd": b"zz"})
        os.symlink("a.txt", os.path.join(mod, "link"))
        after = bo.scan_item(self.repo, "1")
        self.assertEqual(after.tree_sha256, before.tree_sha256)
        self.assertEqual(after.total_bytes, before.total_bytes)
        self.assertTrue(any(s.startswith("link (") for s in after.skipped), after.skipped)

    def test_item_without_regular_files_is_refused(self):
        os.makedirs(os.path.join(self.repo, "mods", "9"))
        with self.assertRaises(bo.BuildError):
            bo.scan_item(self.repo, "9")

    def test_missing_item_folder_is_refused(self):
        os.makedirs(os.path.join(self.repo, "mods"))
        with self.assertRaises(bo.BuildError):
            bo.scan_item(self.repo, "404")


class ArchiveTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="bo-archive-")
        self.repo = os.path.join(self.tmp, "repo")
        make_repo(self.repo, {"111": {"a.lua": b"print(1)\n", "media/t.png": b"\x89PNG",
                                      "media/deep/x.txt": b"x"}})

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def dest(self, name):
        return os.path.join(self.tmp, name)

    def test_the_same_tree_gives_the_same_archive_sha256_twice(self):
        tree = bo.scan_item(self.repo, "111")
        sha1, size1 = bo.write_object(tree, self.dest("first.tar.gz"))
        sha2, size2 = bo.write_object(tree, self.dest("second.tar.gz"))
        self.assertEqual(sha1, sha2)
        self.assertEqual(size1, size2)
        self.assertEqual(_read(self.dest("first.tar.gz")), _read(self.dest("second.tar.gz")))
        self.assertEqual(sha1, _sha(_read(self.dest("first.tar.gz"))))

    def test_archive_does_not_depend_on_file_mtimes(self):
        sha1, _ = bo.write_object(bo.scan_item(self.repo, "111"), self.dest("a.tar.gz"))
        for root, _dirs, names in os.walk(os.path.join(self.repo, "mods", "111")):
            for name in names:
                os.utime(os.path.join(root, name), (1_000_000_000, 1_000_000_000))
        sha2, _ = bo.write_object(bo.scan_item(self.repo, "111"), self.dest("b.tar.gz"))
        self.assertEqual(sha1, sha2)

    def test_members_are_sorted_and_normalised(self):
        dest = self.dest("x.tar.gz")
        bo.write_object(bo.scan_item(self.repo, "111"), dest)
        with tarfile.open(dest, "r:gz") as tar:
            members = tar.getmembers()
        names = [m.name for m in members]
        self.assertEqual(names, sorted(names))
        self.assertIn("mods/111/media/deep/x.txt", names)
        for m in members:
            self.assertEqual((m.uid, m.gid, m.uname, m.gname, m.mtime), (0, 0, "", "", 0), m.name)
            self.assertEqual(m.mode, 0o755 if m.isdir() else 0o644, m.name)
            self.assertTrue(m.name in ("mods", "mods/111") or m.name.startswith("mods/111/"), m.name)

    def test_gzip_header_records_no_name_and_a_zero_mtime(self):
        dest = self.dest("x.tar.gz")
        bo.write_object(bo.scan_item(self.repo, "111"), dest)
        head = _read(dest)[:10]
        self.assertEqual(head[:2], b"\x1f\x8b")
        self.assertEqual(head[3] & 0x08, 0)  # FNAME clear: no source file name in the header
        self.assertEqual(head[4:8], b"\x00\x00\x00\x00")  # MTIME 0

    def test_verify_accepts_the_tree_it_was_built_from(self):
        tree = bo.scan_item(self.repo, "111")
        dest = self.dest("x.tar.gz")
        bo.write_object(tree, dest)
        bo.verify_object(dest, tree.expected_members())

    def test_verify_detects_a_source_file_that_changed_afterwards(self):
        dest = self.dest("x.tar.gz")
        bo.write_object(bo.scan_item(self.repo, "111"), dest)
        write_files(os.path.join(self.repo, "mods", "111"), {"a.lua": b"print(2)\n"})  # same size, new bytes
        changed = bo.scan_item(self.repo, "111")
        with self.assertRaises(bo.VerifyError):
            bo.verify_object(dest, changed.expected_members())

    def test_verify_detects_a_missing_member(self):
        tree = bo.scan_item(self.repo, "111")
        dest = self.dest("x.tar.gz")
        bo.write_object(tree, dest)
        expected = tree.expected_members()
        expected["mods/111/not-there.txt"] = _sha(b"")
        with self.assertRaises(bo.VerifyError):
            bo.verify_object(dest, expected)


class BuildTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="bo-build-")
        self.repo = os.path.join(self.tmp, "repo")
        make_repo(self.repo, {
            "111": {"a.lua": b"A\n", "media/a.png": b"PNG-A"},
            "222": {"b.lua": b"B\n"},
            "333": {"c.lua": b"C\n", "x/y.txt": b"Y"},
        })

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def path(self, *parts):
        return os.path.join(self.tmp, *parts)

    def pack(self, name, version, specs=SPECS, corrupt=()):
        return write_pack(self.path(name), self.repo, version, specs, corrupt=corrupt)

    def build(self, pack, out_name, version, **kwargs):
        return bo.run_build(self.repo, pack, self.path(out_name), f"{BASE}/{version}", log=_quiet, **kwargs)

    def cli(self, pack, out_name, version, *extra):
        return silent_build_main(["--repo", self.repo, "--pack", pack, "--out", self.path(out_name),
                                  "--base-url", f"{BASE}/{version}", *extra])

    def test_manifest_contract_and_object_fields(self):
        pack = self.pack("p1.json", VERSION_1)
        result = self.build(pack, "out1", VERSION_1)
        self.assertEqual(result["exit"], 0)
        out = self.path("out1")
        m = _load(os.path.join(out, "manifest.json"))
        self.assertEqual(m["schema"], 1)
        self.assertEqual(m["items_schema"], 2)
        self.assertEqual(m["pack_version"], VERSION_1)
        self.assertEqual(m["game_build"], "42.21.0")
        self.assertEqual(m["pack_url"], f"{BASE}/{VERSION_1}/pack.json")
        self.assertEqual(m["objects_base_url"], f"{BASE}/")
        self.assertEqual(m["layers"], {})
        self.assertEqual([it["workshop_id"] for it in m["items"]], ["111", "222", "333"])
        self.assertEqual(m["items"][2]["layer"], "client")
        for it in m["items"]:
            obj = it["object"]
            self.assertEqual(obj["url"], f"objects/{it['tree_sha256']}.tar.gz")
            archive = os.path.join(out, "objects", f"{it['tree_sha256']}.tar.gz")
            self.assertEqual(os.path.getsize(archive), obj["bytes"])
            self.assertEqual(_sha(_read(archive)), obj["sha256"])
            bo.verify_object(archive, bo.scan_item(self.repo, it["workshop_id"]).expected_members())
        self.assertEqual(m["items"][0]["folders"], [{"name": "F111", "mod_id": "M111"}])
        pack_bytes = _read(os.path.join(out, "pack.json"))
        self.assertEqual(pack_bytes, _read(pack))
        self.assertEqual(m["pack_sha256"], _sha(pack_bytes))
        self.assertEqual(m["pack_bytes"], len(pack_bytes))
        self.assertFalse([n for n in os.listdir(os.path.join(out, "objects")) if n.endswith(".partial")])

    def test_two_builds_of_the_same_pack_produce_the_same_objects(self):
        pack = self.pack("p1.json", VERSION_1)
        self.build(pack, "a", VERSION_1)
        self.build(pack, "b", VERSION_1)
        ma = _load(self.path("a", "manifest.json"))
        mb = _load(self.path("b", "manifest.json"))
        self.assertEqual([(i["tree_sha256"], i["object"]) for i in ma["items"]],
                         [(i["tree_sha256"], i["object"]) for i in mb["items"]])

    def test_unchanged_items_are_reused_and_only_the_changed_item_is_rebuilt(self):
        first = self.build(self.pack("p1.json", VERSION_1), "out1", VERSION_1)
        self.assertEqual(first["counts"]["built"], 3)
        write_files(os.path.join(self.repo, "mods", "222"), {"b.lua": b"B changed\n"})
        second = self.build(self.pack("p2.json", VERSION_2), "out2", VERSION_2, reuse_dir=self.path("out1"))
        self.assertEqual(second["counts"]["built"], 1)
        self.assertEqual(second["counts"]["reused"], 2)
        by1 = {i["workshop_id"]: i for i in _load(self.path("out1", "manifest.json"))["items"]}
        by2 = {i["workshop_id"]: i for i in _load(self.path("out2", "manifest.json"))["items"]}
        self.assertEqual(by1["111"]["object"], by2["111"]["object"])
        self.assertEqual(by1["333"]["object"], by2["333"]["object"])
        self.assertNotEqual(by1["222"]["tree_sha256"], by2["222"]["tree_sha256"])
        self.assertNotEqual(by1["222"]["object"]["sha256"], by2["222"]["object"]["sha256"])
        self.assertTrue(os.path.isfile(self.path("out2", "objects", by2["222"]["tree_sha256"] + ".tar.gz")))

    def test_damaged_reuse_candidate_is_rejected_and_rebuilt(self):
        reuse = self.path("reuse")
        os.makedirs(os.path.join(reuse, "objects"))
        tree = bo.scan_item(self.repo, "111")
        with open(os.path.join(reuse, "objects", tree.tree_sha256 + ".tar.gz"), "wb") as f:
            f.write(b"not a tarball")
        result = self.build(self.pack("p1.json", VERSION_1), "out-r", VERSION_1, reuse_dir=reuse)
        self.assertEqual(result["counts"]["built"], 3)
        self.assertEqual(result["counts"]["reused"], 0)
        bo.verify_object(self.path("out-r", "objects", tree.tree_sha256 + ".tar.gz"), tree.expected_members())

    def test_damaged_existing_object_is_replaced(self):
        pack = self.pack("p1.json", VERSION_1)
        self.build(pack, "out-d", VERSION_1)
        tree = bo.scan_item(self.repo, "222")
        target = self.path("out-d", "objects", tree.tree_sha256 + ".tar.gz")
        with open(target, "wb") as f:
            f.write(b"garbage")
        result = self.build(pack, "out-d", VERSION_1)
        self.assertEqual(result["counts"]["built"], 1)
        self.assertEqual(result["counts"]["present"], 2)
        bo.verify_object(target, tree.expected_members())

    def test_tree_mismatch_exits_nonzero_and_accept_recomputed_adopts_the_values(self):
        pack = self.pack("bad.json", VERSION_1, corrupt=("222",))
        out = self.path("out-bad")
        self.assertEqual(self.cli(pack, "out-bad", VERSION_1), bo.EXIT_MISMATCH)
        self.assertFalse(os.path.exists(os.path.join(out, "manifest.json")))
        self.assertEqual(self.cli(pack, "out-bad", VERSION_1, "--accept-recomputed"), 0)
        m = _load(os.path.join(out, "manifest.json"))
        item = next(i for i in m["items"] if i["workshop_id"] == "222")
        recomputed = bo.scan_item(self.repo, "222")
        self.assertEqual(item["tree_sha256"], recomputed.tree_sha256)
        self.assertEqual(item["bytes"], recomputed.total_bytes)
        self.assertEqual(m["accepted_recomputed"], ["222"])
        patched = _load(os.path.join(out, "pack.json"))
        self.assertEqual(next(i for i in patched["items"] if i["workshop_id"] == "222")["tree_sha256"],
                         recomputed.tree_sha256)
        self.assertEqual(m["pack_sha256"], _sha(_read(os.path.join(out, "pack.json"))))

    def test_duplicate_content_across_workshop_ids_is_refused(self):
        write_files(os.path.join(self.repo, "mods", "444"), {"a.lua": b"A\n", "media/a.png": b"PNG-A"})
        pack = self.pack("dup.json", VERSION_1, specs=SPECS + [("444", "client")])
        with self.assertRaises(bo.BuildError):
            self.build(pack, "out-dup", VERSION_1)

    def test_missing_item_folder_is_refused(self):
        pack = _dump(self.path("ghost.json"), {"schema": 1, "version": VERSION_1, "game_build": "42.21.0", "items": [
            {"workshop_id": "999", "layer": "base", "tree_sha256": "0" * 64, "bytes": 1,
             "folders": [], "mod_ids": [], "requires": [], "title": "ghost"}]})
        with self.assertRaises(bo.BuildError):
            self.build(pack, "out-ghost", VERSION_1)

    def test_unsafe_workshop_id_is_refused_before_any_path_is_built(self):
        pack = _dump(self.path("evil.json"), {"schema": 1, "version": VERSION_1, "game_build": "42.21.0", "items": [
            {"workshop_id": "../escape", "layer": "base", "tree_sha256": "0" * 64, "bytes": 1,
             "folders": [], "mod_ids": [], "requires": [], "title": "evil"}]})
        with self.assertRaises(bo.BuildError):
            self.build(pack, "out-evil", VERSION_1)

    def test_base_url_must_end_with_the_pack_version_and_be_http(self):
        pack = self.pack("p1.json", VERSION_1)
        with self.assertRaises(bo.UsageError):
            bo.run_build(self.repo, pack, self.path("out-x"), f"{BASE}/{VERSION_1}/extra", log=_quiet)
        with self.assertRaises(bo.UsageError):
            bo.run_build(self.repo, pack, self.path("out-y"), f"ftp://example.invalid/pzpack/{VERSION_1}",
                         log=_quiet)

    def test_legacy_layers_are_carried_over_when_the_pack_matches(self):
        pack = self.pack("p1.json", VERSION_1)
        layers = {"base": {"url": f"{BASE}/{VERSION_1}/base.tar.gz", "bytes": 10, "sha256": "ab" * 32},
                  "client": {"url": f"{BASE}/{VERSION_1}/client.tar.gz", "bytes": 20, "sha256": "cd" * 32}}
        legacy = _dump(self.path("legacy.json"), {"schema": 1, "pack_version": VERSION_1,
                                                   "pack_sha256": _sha(_read(pack)), "pack_bytes": 1,
                                                   "layers": layers})
        self.build(pack, "out-l", VERSION_1, legacy_manifest=legacy)
        self.assertEqual(_load(self.path("out-l", "manifest.json"))["layers"], layers)

    def test_legacy_manifest_for_another_pack_version_is_refused(self):
        pack = self.pack("p1.json", VERSION_1)
        legacy = _dump(self.path("legacy.json"), {"schema": 1, "pack_version": "2025.01.01",
                                                   "pack_sha256": _sha(_read(pack)), "layers": {}})
        with self.assertRaises(bo.BuildError):
            self.build(pack, "out-l2", VERSION_1, legacy_manifest=legacy)

    def test_legacy_manifest_built_from_a_different_pack_json_is_refused(self):
        pack = self.pack("p1.json", VERSION_1)
        legacy = _dump(self.path("legacy.json"), {"schema": 1, "pack_version": VERSION_1,
                                                   "pack_sha256": "0" * 64, "layers": {}})
        with self.assertRaises(bo.BuildError):
            self.build(pack, "out-l3", VERSION_1, legacy_manifest=legacy)


class FakeClientError(Exception):
    def __init__(self, code):
        super().__init__(code)
        self.response = {"Error": {"Code": code}}


class FakeS3:
    """Records every call and keeps objects in memory. Never touches the network."""

    def __init__(self, existing=None, fail_head_code=None):
        self.calls = []
        self.store = dict(existing or {})
        self.fail_head_code = fail_head_code

    def head_object(self, Bucket, Key):
        self.calls.append(("head_object", Key))
        if self.fail_head_code:
            raise FakeClientError(self.fail_head_code)
        if Key not in self.store:
            raise FakeClientError("404")
        return dict(self.store[Key])

    def upload_file(self, Filename, Bucket, Key, ExtraArgs=None):
        extra = dict(ExtraArgs or {})
        self.calls.append(("upload_file", Key, extra))
        self.store[Key] = {"ContentLength": os.path.getsize(Filename), "Metadata": dict(extra.get("Metadata", {}))}

    def put_object(self, Bucket, Key, Body, ContentType=None, CacheControl=None, Metadata=None):
        extra = {"ContentType": ContentType, "CacheControl": CacheControl, "Metadata": dict(Metadata or {})}
        self.calls.append(("put_object", Key, extra))
        self.store[Key] = {"ContentLength": len(Body), "Metadata": dict(Metadata or {})}

    def writes(self):
        return [c for c in self.calls if c[0] in ("upload_file", "put_object")]


class UploadTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="uo-test-")
        self.repo = os.path.join(self.tmp, "repo")
        make_repo(self.repo, {"111": {"a.lua": b"A"}, "222": {"b.lua": b"B"}})
        pack = write_pack(os.path.join(self.tmp, "p.json"), self.repo, VERSION_1,
                          [("111", "base"), ("222", "client")])
        self.out = os.path.join(self.tmp, "out")
        bo.run_build(self.repo, pack, self.out, f"{BASE}/{VERSION_1}", log=_quiet)
        self.manifest = _load(os.path.join(self.out, "manifest.json"))

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)

    def object_key(self, index):
        return f"pzpack/{self.manifest['items'][index]['object']['url']}"

    def object_record(self, index):
        obj = self.manifest["items"][index]["object"]
        return {"ContentLength": obj["bytes"], "Metadata": {"sha256": obj["sha256"]}}

    def real_run(self, client, **kwargs):
        return uo.run_upload(self.out, "example-bucket", dry_run=False, yes=True, client=client,
                             log=_quiet, **kwargs)

    def test_dry_run_is_the_default_and_makes_no_s3_calls(self):
        no_client = mock.patch.object(uo, "make_s3_client", side_effect=AssertionError("no client in a dry run"))
        with no_client, contextlib.redirect_stdout(io.StringIO()):
            rc = uo.main(["--dir", self.out, "--bucket", "example-bucket"])
        self.assertEqual(rc, uo.EXIT_OK)
        fake = FakeS3()
        summary = uo.run_upload(self.out, "example-bucket", dry_run=True, client=fake, log=_quiet)
        self.assertFalse(summary["real"])
        self.assertEqual(fake.calls, [])

    def test_no_dry_run_without_yes_is_refused_before_any_work(self):
        no_client = mock.patch.object(uo, "make_s3_client", side_effect=AssertionError("no client"))
        with no_client, contextlib.redirect_stderr(io.StringIO()):
            rc = uo.main(["--dir", self.out, "--bucket", "example-bucket", "--no-dry-run"])
        self.assertEqual(rc, uo.EXIT_USAGE)

    def test_yes_alone_still_only_plans(self):
        no_client = mock.patch.object(uo, "make_s3_client", side_effect=AssertionError("no client"))
        with no_client, contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            rc = uo.main(["--dir", self.out, "--bucket", "example-bucket", "--yes"])
        self.assertEqual(rc, uo.EXIT_OK)

    def test_objects_then_metadata_then_latest_with_immutable_and_no_cache_headers(self):
        fake = FakeS3()
        summary = self.real_run(fake, update_latest=True)
        uploads = [c for c in fake.writes() if c[0] == "upload_file"]
        puts = [c for c in fake.writes() if c[0] == "put_object"]
        self.assertEqual(len(uploads), 2)
        for _kind, key, extra in uploads:
            self.assertTrue(key.startswith("pzpack/objects/"), key)
            self.assertEqual(extra["ContentType"], "application/gzip")
            self.assertEqual(extra["CacheControl"], "public, max-age=31536000, immutable")
            self.assertIn("sha256", extra["Metadata"])
        self.assertEqual([c[1] for c in puts], [META_PACK_KEY, META_MANIFEST_KEY, "pzpack/latest.json"])
        for _kind, _key, extra in puts:
            self.assertEqual(extra["CacheControl"], "no-cache")
            self.assertEqual(extra["ContentType"], "application/json")
        last_upload = max(i for i, c in enumerate(fake.calls) if c[0] == "upload_file")
        first_put = min(i for i, c in enumerate(fake.calls) if c[0] == "put_object")
        self.assertLess(last_upload, first_put)
        self.assertEqual(summary["uploaded"], 2)
        self.assertTrue(summary["latest_written"])

    def test_latest_pointer_is_not_touched_without_the_flag(self):
        fake = FakeS3()
        self.real_run(fake, update_latest=False)
        self.assertNotIn("pzpack/latest.json", [c[1] for c in fake.writes()])

    def test_existing_object_with_the_same_sha256_is_skipped_never_overwritten(self):
        key = self.object_key(0)
        fake = FakeS3(existing={key: self.object_record(0)})
        summary = self.real_run(fake)
        self.assertNotIn(key, [c[1] for c in fake.writes()])
        self.assertEqual(summary["skipped"], 1)
        self.assertEqual(summary["uploaded"], 1)

    def test_existing_object_with_a_different_sha256_stops_the_run_with_nothing_written(self):
        fake = FakeS3(existing={self.object_key(1): {"ContentLength": 5, "Metadata": {"sha256": "ee" * 32}}})
        with self.assertRaises(uo.UploadError):
            self.real_run(fake)
        self.assertEqual(fake.writes(), [])

    def test_metadata_conflict_stops_before_any_object_is_sent(self):
        fake = FakeS3(existing={META_MANIFEST_KEY: {"ContentLength": 3, "Metadata": {"sha256": "ff" * 32}}})
        with self.assertRaises(uo.UploadError):
            self.real_run(fake)
        self.assertEqual(fake.writes(), [])

    def test_identical_metadata_is_not_rewritten(self):
        pack_bytes = _read(os.path.join(self.out, "pack.json"))
        manifest_bytes = _read(os.path.join(self.out, "manifest.json"))
        fake = FakeS3(existing={
            META_PACK_KEY: {"ContentLength": len(pack_bytes), "Metadata": {"sha256": _sha(pack_bytes)}},
            META_MANIFEST_KEY: {"ContentLength": len(manifest_bytes), "Metadata": {"sha256": _sha(manifest_bytes)}},
        })
        self.real_run(fake)
        written = [c[1] for c in fake.writes()]
        self.assertNotIn(META_PACK_KEY, written)
        self.assertNotIn(META_MANIFEST_KEY, written)
        self.assertEqual(len([c for c in fake.writes() if c[0] == "upload_file"]), 2)

    def test_a_head_failure_other_than_not_found_is_never_read_as_absent(self):
        fake = FakeS3(fail_head_code="403")
        with self.assertRaises(uo.UploadError):
            self.real_run(fake)
        self.assertEqual(fake.writes(), [])

    def test_local_object_that_does_not_match_the_manifest_is_refused_even_in_dry_run(self):
        tampered = os.path.join(self.tmp, "tampered")
        shutil.copytree(self.out, tampered)
        url = self.manifest["items"][0]["object"]["url"]
        with open(os.path.join(tampered, *url.split("/")), "ab") as f:
            f.write(b"x")
        with self.assertRaises(uo.UploadError):
            uo.run_upload(tampered, "example-bucket", dry_run=True, log=_quiet)

    def test_a_manifest_without_items_schema_2_is_refused(self):
        legacy = os.path.join(self.tmp, "legacy-out")
        shutil.copytree(self.out, legacy)
        data = _load(os.path.join(legacy, "manifest.json"))
        del data["items_schema"]
        _dump(os.path.join(legacy, "manifest.json"), data)
        with self.assertRaises(uo.UploadError):
            uo.build_plan(legacy, "pzpack", update_latest=False)

    def test_missing_credentials_refuse_a_real_upload(self):
        class NoCredentialsSession:
            def __init__(self, profile_name=None, region_name=None):
                self.profile_name = profile_name

            def get_credentials(self):
                return None

            def client(self, *args, **kwargs):
                raise AssertionError("no client may be created without credentials")

        fake_boto3 = types.SimpleNamespace(session=types.SimpleNamespace(Session=NoCredentialsSession))
        fake_modules = mock.patch.dict(sys.modules, {"boto3": fake_boto3})
        with fake_modules, contextlib.redirect_stderr(io.StringIO()), contextlib.redirect_stdout(io.StringIO()):
            rc = uo.main(["--dir", self.out, "--bucket", "example-bucket", "--no-dry-run", "--yes"])
        self.assertEqual(rc, uo.EXIT_USAGE)

    def test_credential_values_are_never_printed(self):
        access_marker = "AKIA" + "FAKE" + "TESTMARKER0001"  # sentinel string, not a credential
        secret_marker = "marker-" + "not-a-credential-" + "0002"  # sentinel string, not a credential
        fake = FakeS3()

        class Creds:
            method = "env"

            def __init__(self):
                self.access_key = access_marker
                self.secret_key = secret_marker
                self.token = None

        class CredentialsSession:
            def __init__(self, profile_name=None, region_name=None):
                self.profile_name = profile_name

            def get_credentials(self):
                return Creds()

            def client(self, *args, **kwargs):
                return fake

        fake_boto3 = types.SimpleNamespace(session=types.SimpleNamespace(Session=CredentialsSession))
        lines = []
        with mock.patch.dict(sys.modules, {"boto3": fake_boto3}):
            uo.run_upload(self.out, "example-bucket", dry_run=False, yes=True, log=lines.append)
        text = "\n".join(lines)
        self.assertIn("via env", text)
        self.assertNotIn(access_marker, text)
        self.assertNotIn(secret_marker, text)
        self.assertTrue(fake.writes())

    def test_boto3_is_not_imported_by_the_dry_run(self):
        saved = sys.modules.pop("boto3", None)
        try:
            with contextlib.redirect_stdout(io.StringIO()):
                rc = uo.main(["--dir", self.out, "--bucket", "example-bucket"])
            self.assertEqual(rc, uo.EXIT_OK)
            self.assertNotIn("boto3", sys.modules)
        finally:
            if saved is not None:
                sys.modules["boto3"] = saved

    def test_plan_keys_live_under_the_prefix(self):
        plan = uo.build_plan(self.out, "/custom/", update_latest=True)
        self.assertTrue(all(o.key.startswith("custom/objects/") for o in plan["objects"]))
        self.assertEqual([m.key for m in plan["meta"]],
                         [f"custom/{VERSION_1}/pack.json", f"custom/{VERSION_1}/manifest.json"])
        self.assertEqual(plan["latest"].key, "custom/latest.json")


if __name__ == "__main__":
    unittest.main()
