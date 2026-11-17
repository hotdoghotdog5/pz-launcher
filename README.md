# pzpack — Project Zomboid server modpack launcher

A Bubble Tea TUI that installs the frozen, patched modpack into `~/Zomboid/mods` and launches Project
Zomboid with **local mods first** (`-modfolders mods,workshop,steam`), so Steam Workshop copies of the
same mods can never overwrite or supersede our patched versions.

> **Live distribution:** bucket `pzpack-895941021447` (us-east-1), player page
> `https://pzpack-895941021447.s3.us-east-1.amazonaws.com/pzpack/index.html`.
> Deployment details: [DISTRIBUTION.md](DISTRIBUTION.md).


> Unofficial community tool, not affiliated with The Indie Stone. This repository holds the launcher only:
> no mod files are included. `pack/pack.json` lists the mods (Workshop ids, folders, hashes); the files are
> fetched from the manifest URL the binary is built with.

## Current state (pack 2026.11.11, Project Zomboid 42.22.2)

The sections below were written for the first, layer-based release and still describe the flags and the
install model correctly. What changed since:

- **One archive per workshop item** (`items_schema` 2). Each item is a `tar.gz` object named by its tree hash
  under `objects/`; the manifest lists items, folders and platforms. A later pack version downloads only the
  items whose hash changed. Builder and uploader: `tools/build_objects.py`, `tools/upload_objects.py`.
- **Downloads resume and keep retrying.** `Range` requests guarded by `If-Match`, a stall watchdog, capped
  jittered backoff on transport errors, 408/425/429 and 5xx; other 4xx fail at once. An object that fails its
  SHA-256 is fetched again at most three times.
- **Platform-gated items.** An item with `"platforms": ["darwin/arm64"]` in `pack.json` is installed only on
  that build (the Metal renderer for Viewpoint). On Apple Silicon the launcher also applies the Viewpoint
  Metal patches at launch (`internal/pz/vpmetal.go`); the patch is a no-op elsewhere.
- **Steam library detection.** Registry, `libraryfolders.vdf`, `appmanifest_108600.acf` and a drive scan; a
  leftover folder without the game files is ignored. `pzpack --where` prints what was searched.
- **Honest progress and a free-space check.** One overall counter (item N of M, GB done of total), a named
  step for extract, link, copy and verify, and a status line at least every 30 s. Before anything is
  downloaded the launcher checks that the cache volume and the install volume have room, and refuses with
  the drive, the need and the free space if not.
- **Guards.** Writes are refused while the game is running; the launcher backs up every file it edits and
  every folder it moves aside (`.pzpack/backup`), and only removes folders it recorded in `.pzpack/state.json`.

Known limits: Windows has had its unit tests run on Windows 11 but not a full install on this release;
`--restore-default` restores the newest `default.txt` backup, which after a re-run is the launcher's own
previous list rather than the original.

## Build

```bash
# Go 1.24+ (see go.mod)
go build -o pzpack .
```

The binary is self-contained. `pack/pack.json` is the manifest. With no `--source`, the launcher uses
`./` when it contains `mods/`, then `../pz-server-modpack`, and otherwise downloads the published pack
(from the baked manifest URL or `--from`).

## Run

```bash
./pzpack                       # TUI wizard
./pzpack --source /path/to/pack-root
```

Wizard: detected paths → **Lite / Full** profile → options → plan (shows same-ID copies found outside the
pack) → install progress → **launch**.

| Profile | What it is | Size |
|---|---|---|
| **Lite** | base pack + **16 compatibility stubs** for the Viewpoint/ZombieBuddy ids — joins the same server, no client visuals | ~10.00 GB |
| **Full** | base pack + the real Viewpoint/ZombieBuddy layer | ~10.70 GB |

> **One server, both profiles.** The server lists every id (including the client layer) with
> `DoLuaChecksum=false`; Lite players install `mod.info`-only stubs that satisfy the connect check, Full
> players install the real mods. See [VIEWPOINT-COMPAT.md](VIEWPOINT-COMPAT.md) for the decompiled
> evidence and the exact constraints.

## Headless

```bash
./pzpack --pack pack/pack.json --source . --profile lite --no-tui
./pzpack --profile lite --dry-run            # plan + conflicts + stub count, copy nothing
./pzpack --root /tmp/test --profile lite --no-tui --limit 3   # smoke test
./pzpack --uninstall                          # remove only launcher-managed folders
./pzpack --list
```

| Flag | Meaning |
|---|---|
| `--source` | pack root containing `mods/` (default: `./` if it has `mods/`, else `../pz-server-modpack`, else download) |
| `--pack` | path to `pack.json` (default `pack/pack.json`) |
| `--root` | install target (default: detected `~/Zomboid/mods`) |
| `--profile lite\|full` | headless profile; omit for the wizard |
| `--dry-run` | plan and conflict report only |
| `--limit N` | install only the first N items (testing) |
| `--from URL` | install from a published manifest (S3/CDN) instead of a local tree |
| `--cache DIR` | download cache (default: OS cache dir + `/pzpack`) |
| `--refresh` | re-download cached layers |
| `--local` | ignore a baked/env manifest URL and use `--source` |
| `--no-default` | do not rewrite `Zomboid/mods/default.txt` |
| `--no-launch-config` | do not edit `Info.plist` / `ProjectZomboid64.json` before launching |
| `--apply-launch-config` | write the heap + ZombieBuddy settings and exit (no launch) |
| `--launch-only` | launch the game without installing, downloading or rewriting `default.txt` |
| `--restore-default` | restore the `default.txt` backup and exit |
| `--write-server-ini FILE` | rewrite `Mods=`/`WorkshopItems=` in a server ini and exit |
| `--clear-default` | disable auto load order and move `default.txt` aside (backup kept) |
| `--launch` | start the game after install |
| `--server-ini lite\|full` | print the server `Mods=` fragment (identical for both; includes the client layer) |
| `--uninstall` | remove folders recorded in `.pzpack/state.json` |

## Launch configuration (Full / Viewpoint)

`--launch` (and the TUI's **l** key) prepares the game before starting it:

- **Heap.** Replaces `-Xmx…` with **8 GiB** when the machine has 16 GiB of RAM or less, **10 GiB** above
  that. macOS: `Project Zomboid.app/Contents/Info.plist` → `JVMOptions`. Windows/Linux:
  `ProjectZomboid64.json` → `vmArgs`.
- **ZombieBuddy (Full only).** Copies `ZombieBuddy.jar` from the installed mod into the game's Java
  folder and adds the agent:
  - macOS/Linux: `-javaagent:ZombieBuddy.jar`
  - Windows: `-agentlib:zbNative` (plus `ZombieBuddy.jar` and `zbNative.dll` in the game folder, as the
    official installer does)
- The `--` in the official `-javaagent:ZombieBuddy.jar --` line belongs to the game's launch-options
  parser. `JVMOptions`/`vmArgs` are already separate from game arguments, so no `--` is written there.
- Each edited file is backed up once as `<file>.pzpack.bak`; running again is a no-op. Steam updates can
  overwrite these files — the next launch re-applies them.
- `--apply-launch-config --profile full` applies the same changes without launching.
- A Full launch aborts with an explanation if the config cannot be written; use `--no-launch-config` to
  override.

## Automatic mod load order

On every install the launcher writes **`~/Zomboid/mods/default.txt`** with exactly the pack's mod ids, in
the pack's load order (the same order the server gets in `Mods=`). The previous file is backed up once at
`~/Zomboid/mods/.pzpack/default.txt.bak`; `--restore-default` puts it back.

This is what makes the game start with only our mods enabled — any Workshop mod the player subscribed to
outside the pack is left on disk but not enabled. Run the game through the launcher (or the same install
step) after changing the pack; Project Zomboid rewrites `default.txt` when the player edits the mod list
in-game.

For a dedicated server, either paste the `--server-ini` output or let the launcher edit the ini in place:

```bash
./pzpack --profile full --write-server-ini ~/Zomboid/Server/servertest.ini
```

It replaces `Mods=` and `WorkshopItems=` (empty), leaves every other setting alone, and keeps
`servertest.ini.pzpack.bak`.

## Distribution (S3 / CDN)

Build the archives and a manifest once, upload, then hand players a launcher that downloads them:

```bash
# 1. archives: base (~10 GB) + client (~0.7 GB)
python3 tools/build_dist.py --repo ../pz-server-modpack --pack pack/pack.json \
    --out dist/2026.11.03 \
    --base-url https://my-bucket.s3.eu-west-1.amazonaws.com/pzpack/2026.11.03

# 2. upload (boto3 credential chain; no keys are stored by the script)
python3 tools/upload_s3.py --dir dist/2026.11.03 --bucket my-bucket --region eu-west-1 --public-read

# 3. build a launcher with the manifest URL baked in
tools/build_release.sh https://my-bucket.s3.eu-west-1.amazonaws.com/pzpack/latest.json
```

`tools/build_all.sh <manifest-url>` builds macOS arm64/Intel, Windows x64/ARM64 and Linux x64.

Players then run `pzpack` (no pack tree needed): the wizard downloads the layers into the OS cache,
verifies SHA-256, extracts, installs the chosen profile, writes `default.txt` and can launch. Downloads
resume from a `.part` file and are skipped when the cache marker matches. On a headless box:

```bash
./pzpack --from https://.../pzpack/latest.json --profile lite --no-tui
```

Private buckets: generate presigned URLs or put CloudFront in front and update the manifest URLs; the
launcher treats them as opaque strings. The bucket must allow GET for `pack.json`, both `.tar.gz`
layers and `manifest.json`.

## Compatibility stubs

`tools/emit_stubs.py <dir>` writes the 16 stub folders standalone, for players who install by hand.
Each stub is just `mod.info` with the same `id` and no `require`/version bounds — the loader only checks
that the id resolves (`ChooseGameInfo.isAvailableSelf`), so no content is needed. The launcher's Lite
profile generates them during install.

## Conflict model

- Installs into `~/Zomboid/mods` — Steam never writes there, so our files are never updated.
- Launches with `-modfolders mods,workshop,steam`: local copies outrank Workshop and author copies.
- The plan screen lists every same-ID mod found in the Workshop cache / `Zomboid/Workshop` / other local
  folders. **Nothing is deleted.** If the player is subscribed, Steam updates its copy; ours still wins.
- Not using the launcher to start the game? Set Steam → Project Zomboid → Properties → Launch Options:
  `-modfolders mods,workshop,steam`.

## Server side

Emit the config fragment (the same `Mods=` for both client profiles — it must include the client layer):

```bash
./pzpack --server-ini full > pzpack-mods.ini
```

It prints `WorkshopItems=` (empty — local mods only, no auto-updates) and the dependency-ordered
`Mods=` list. Paste those two lines into the server's `servertest.ini`, set **`DoLuaChecksum=false`**,
and place the same pack under the server's `zomboid/mods/`. The server runs the real client-layer mods;
clients choose real (Full) or stub (Lite).

## Layout

```
pz-launcher/
├── main.go                 # flags, headless mode, TUI bootstrap
├── internal/pz/            # paths, mod.info ids, launch, default.txt, server ini
├── internal/pack/          # pack.json, install/verify/uninstall, conflicts
├── internal/dist/          # manifest, resumable download, safe tar.gz extract
├── internal/ui/            # Bubble Tea wizard
├── pack/pack.json          # generated manifest (items, layers, load order)
└── tools/                  # build_dist.py, upload_s3.py, build_release.sh, packgen.py
```

## Tests

```bash
go test ./...
go vet ./...
./pzpack --pack pack/pack.json --source ../pz-server-modpack --root /tmp/pzpack-test \
         --profile lite --no-tui --limit 3
./pzpack --root /tmp/pzpack-test --uninstall
```
