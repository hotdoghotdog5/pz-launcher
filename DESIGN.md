# Launcher / deploy design

Goal: get the frozen, patched 310-item pack onto a new player's machine, with two audiences:

- **Full** — wants Viewpoint + ZombieBuddy and the 12 Viewpoint-dependent visual mods.
- **Lite** — wants the server content without that client visual layer.

and ensure our patched copies are the ones loaded, even if the player is also subscribed to the same
mods on the Steam Workshop.

## What the game actually does (research)

1. **Mods load from three folders, and the order is configurable.**
   `-modfolders {folders}` accepts `mods`, `workshop`, `steam` and sets both *where* mods load from and
   *their priority*. The official wiki documents it as the way to control load order:
   `-modfolders workshop,steam,mods` is its example.
   → [PZwiki: Startup parameters](https://pzwiki.net/wiki/Startup_parameters)

2. **`mods` is `~/Zomboid/mods`; `steam` is the Workshop cache; `workshop` is the author-upload folder.**
   Steam never writes to `~/Zomboid/mods`, so a local copy cannot be overwritten or auto-updated.
   The community's documented method for freezing Workshop mods is exactly this: copy the mod into
   `~/Zomboid/mods` and launch with `-modfolders mods,workshop,steam` so the local copy wins over the
   auto-updating Workshop copy.
   → [Steam discussion: saving local copies of Workshop mods](https://steamcommunity.com/app/108600/discussions/0/3198118348344288548/)

3. **Two copies with the same Mod ID clash.** PZ identifies mods by `mod.info` `id=`, not by folder.
   The wiki explicitly warns that duplicates in different recognised folders "clash together and
   overwrite each other". With an explicit `-modfolders` order the earlier folder wins, which is why
   local-first ordering is the supported way to pin a version.
   → [PZwiki: Mod structure](https://pzwiki.net/wiki/Mod_structure)

4. **There is no normal client-only mod in multiplayer.** The client loads the server's `Mods=` list plus
   only pure-translation mods from `default.txt`; other main-menu mods are not loaded in MP at all.
   `DoLuaChecksum` compares Lua/script/anim files when enabled, which is what produces the classic
   "file does not match" kick.
   → [PZwiki: Server settings](https://pzwiki.net/wiki/Server_settings) ·
   [Haptic: mod file kicks](https://haptic.gg/support/games/project-zomboid/mod-file-kicks)

**But the presence check is only an id check.** `ChooseGameInfo.isAvailableSelf` tests version bounds and
`require=` only — it never checks for media, Lua or a jar — and `ConnectionDetails.writeMods` sends only
id/workshopId/name. So a `mod.info`-only **stub** with the right id satisfies the client, and with
`DoLuaChecksum=false` the file-content comparison (`NetChecksum` / `IsoWorld`) never runs. That makes one
server possible: the server lists every id, Full clients install the real mods, Lite clients install
stubs. See [VIEWPOINT-COMPAT.md](VIEWPOINT-COMPAT.md) for the decompiled evidence.

## Chosen strategy

1. **Install local copies** into `~/Zomboid/mods/<ModFolder>` (never into the Steam cache).
2. **Launch with `-modfolders mods,workshop,steam`** so the local copies outrank Workshop and author
   copies for the same Mod ID. Steam can update the Workshop copy all it likes; it is not the copy that
   loads.
3. **Never touch the Workshop cache.** Same-ID copies are detected and reported, not deleted. Deleting
   would fight Steam, break other servers, and be undone by a verify.
4. **Optionally clear `default.txt`** (renamed with a backup) so mods enabled in the player's main menu do
   not load ahead of the server's list and shift the checksum comparison.
5. **Track what we own** in `~/Zomboid/mods/.pzpack/state.json`; a profile switch removes only folders the
   launcher installed, and an existing unowned folder is moved to `.pzpack/backup/<timestamp>/` instead
   of being destroyed.
6. **Stub the client layer for Lite** — same mod ids, `mod.info` only, no `require`, no version bounds.
   The server keeps the real mods (its server-side Lua still runs); only the client's content differs.

### Why not rename Mod IDs?

Renaming (`pzsp_<id>`) would make coexistence unconditional, but it breaks the dependency graph every
mod declares (`require=`, `loadAfter=`) and any hard-coded ID checks, and it makes the client disagree
with every server that uses the original IDs. The `-modfolders` mechanism is the documented, reversible
way to get the same guarantee without forking identity. If a future strict mode needs it, the pack
manifest already carries every `mod_ids` entry per item, so a rewriter can be added.

## Profiles

| Profile | Contents | Size |
|---|---|---|
| `lite` | base layer + **16 stubs** for the client-layer ids (joins the full server, no visuals) | ~10.00 GB |
| `full` | base + 14 Viewpoint/ZombieBuddy items (real client layer) | ~10.70 GB |

The server always runs the full 310-item pack and lists all ids; the profile only changes what the
**client** installs.

The client layer is the transitive dependency closure of `Viewpoint` and `ZombieBuddy`, computed from the
pack's `requires` graph: Project Viewpoint, ZombieBuddy, Viewpoint QOL, Light Fix, Advanced Throwables,
Surface Fix, KI5 Parts Fix, Immersive Driving, Visible Loot, Models Overhaul, Visceral, Character Texture
VRAM Leak Fix, Viewpoint x A-Life, Workstations Overhaul.

## Install layout

```
~/Zomboid/mods/
├── .pzpack/state.json          # owned folders, profile, version
├── <ModFolder>/                # one per mod.info root, exactly as shipped
└── (untouched player mods)
```

`pack.json` lists every workshop item, its mod folders (with source paths), layer, byte size and the
post-fix tree hash. `--server-ini lite|full` emits a matching `Mods=` line in dependency order (topological
sort of `requires`), with `WorkshopItems=` deliberately empty.

## Distribution

- Build a bundle from the repo: `pack/pack.json` + `mods/` tree (see `tools/build-bundle.sh`).
- Host it (LAN share, object storage, release asset) and have players extract it, or point
  `--source` at a checkout.
- `pzpack` verifies the target is writable and reports per-item sizes; a deep hash verification mode is
  the next addition (the manifest already carries `tree_sha256`).

## Limits / risks

- **Same-ID duplicates** are resolved by load order, not by deletion. If a player launches the game
  outside the launcher (plain Steam), the Workshop copy may win. The TUI launches with the argument and
  the README documents setting it in Steam Launch Options.
- **MP checksum must stay off** (`DoLuaChecksum=false`). It is the one hard dependency: with it on, stub
  and real copies mismatch and every Lite client is kicked. Keep the base pack version identical across
  clients even though the checksum no longer enforces it.
- **Java mods** (ZombieBuddy, Viewpoint jars) verify their uploader; they are redistributed as-is from the
  pack. Server operators are responsible for Workshop content licensing when hosting a bundle.
- **Windows/Linux paths** are coded but only macOS was exercised in this environment.
