# Can Viewpoint and non-Viewpoint players share one server?

**Yes — and it needs no ZombieBuddy patch, no second server, and no fork.** The engine has no
client-only mod support, but the mod-list requirement is only an **id presence check**; the content
comparison is the Lua/script checksum, which is already disabled on this server.

All findings below come from decompiling the installed **42.21.0** `projectzomboid.jar` with CFR, not
from forum lore. Class and method names are given so this can be re-verified after a game update.

---

## 1. What the engine actually does

### 1.1 In multiplayer, a client loads *only* the server's mod list

`zombie/ZomboidFileSystem.loadMods(String)`:

```java
if (GameClient.client) {
    ArrayList<String> toLoad = new ArrayList<>();
    this.loadTranslationMods(toLoad);              // only pure-translation mods from default.txt
    toLoad.addAll(GameClient.instance.serverMods);
    this.loadMods(toLoad);
    return;
}
```

There is no path that loads a normal client-side mod the server did not list. This is why "just let
Viewpoint players enable it locally" cannot work by itself.

### 1.2 The client refuses to connect if a listed id is missing

`zombie/gameStates/ConnectToServerState.CheckMods()` reads the ids the server sent, then calls
`ZomboidFileSystem.loadModsAux(...)`. On the first id it cannot resolve it disconnects with the
`UI_OnConnectFailed_ModRequired` message.

Resolution goes through `ChooseGameInfo.getAvailableModDetails(id)`:

- `getModDetails` looks the id up across all mod folders, **by `mod.info` id**, not by folder name.
- `isAvailableSelf()` only rejects a mod whose `versionMin`/`versionMax` exclude the current game build.
- `isAvailableRequired()` only walks the `require=` chain.
- **Nothing checks for files, media, Lua, textures or a jar.**

So a folder containing nothing but a `mod.info` with the right `id=` is a valid, available mod to the
loader. That is the whole trick.

### 1.3 The id, not the folder, is the identity — and folder order picks the winner

`ChooseGameInfo.readModInfo` de-duplicates by `id` and keeps the copy from the **earlier** folder in
`getAllModFolders()`. That order is exactly the `-modfolders` argument, so
`-modfolders mods,workshop,steam` makes `~/Zomboid/mods` win over a Workshop copy of the same id.

### 1.4 The server never compares mod lists

`zombie/network/ConnectionDetails.writeMods` sends only `id`, `workshopId` and `name` to the client.
The server does not receive or diff a client mod list. The only content check is
`zombie/network/NetChecksum` (MD5 groups of Lua, scripts and anims) exchanged by `ChecksumPacket`;
a mismatch sets `connection.checksumState = Different`, and `AntiCheatChecksumUpdate` kicks after
`IsoWorld.LUA_CHECKSUM_TIMEOUT_MS`.

### 1.5 `DoLuaChecksum=false` disables that comparison entirely

`zombie/iso/IsoWorld`:

```java
if (GameClient.client && ServerOptions.instance.doLuaChecksum.getValue()) {
    NetChecksum.comparer.beginCompare();   // only caller of sendTotalChecksum()
    ...
}
```

`DoLuaChecksum` is a public server option, sent to the client in `receiveServerOptions()` before the
world loads. With it false the client never calls `beginCompare`, never sends a checksum, the server's
state stays `Init`, and the anti-cheat kick never arms. **The server therefore does not care whether
the client's files match — only that every listed id resolves.**

### 1.6 ZombieBuddy cannot solve this for the players who lack it

`ZombieBuddy/mod.info` declares `javaPkgName=me.zed_0xff.zombie_buddy` and `javaJarFile=...`; it is a
PZ Java mod, enabled only when the mod is loaded. In MP the loaded set is the server list. A lite
player has no ZombieBuddy to patch anything, and a full player's ZombieBuddy cannot change what the
server sends. Using it to relax requirements is circular; the stub approach needs no code injection at
all.

---

## 2. Options considered

| # | Approach | Verdict |
|---|---|---|
| **A** | **Compatibility stubs + `DoLuaChecksum=false`** — server lists every client-layer id; full clients install the real mods, compat clients install `mod.info`-only stubs with the same ids | **Recommended.** No engine changes, no per-player server logic, survives game updates, one server. |
| B | Server jar patch: filter the mod list per client / skip the presence check | Possible but invasive, version-fragile, and unnecessary: the presence check is client-side and the checksum is already off. |
| C | Client JVM agent (`-javaagent`) or a ZombieBuddy transformer that appends extra mods after `CheckMods` | Technically possible, but requires a custom launch wrapper for full players, maintenance on every PZ update, and still needs the server-side Lua of three of the mods. Not worth it. |
| D | Two servers (full / lite) | Works, but does not meet the goal of one private server. |
| E | Fork the mod ids (`pzsp_Viewpoint`) | Does **not** remove the presence check — the server still lists whatever id it uses — and it breaks every `require=` chain. No benefit here. |

---

## 3. The recommended setup

**Server** (one instance, the full 314-item pack):

```ini
WorkshopItems=
Mods=<all 314 ids, from `pzpack --server-ini full`>
DoLuaChecksum=false
```

The server runs the real client-layer mods, so the server-side Lua in Advanced Throwables, Immersive
Driving and Visible Loot still runs for everyone.

**Full client:** installs the 14 real client-layer items (16 mod folders).

**Compat client:** installs the base pack plus **16 stubs** — one folder per client-layer mod id, each
containing only:

```ini
name=Project Viewpoint
id=Viewpoint
description=Compatibility stub ...
```

No Lua, no jar, no textures. The id satisfies the connect check; the client simply has none of the
visual content.

Both launch with `-modfolders mods,workshop,steam`, so a Workshop subscription to the same id cannot
outrank the local real copy or the local stub.

`pzpack` now implements this directly: the **lite** profile installs base + stubs (10.06 GB, no
Viewpoint/ZombieBuddy), the **full** profile installs base + real (10.76 GB). The server's `Mods=` line
is identical for both.

---

## 4. Residual risks to test

1. **Server→client RPCs without a handler.** Three of the client-layer mods have server-side Lua:
   Advanced Throwables (`VAT_Server.lua`), Immersive Driving (3 server files) and Visible Loot
   (`VisibleLootFills.lua`). A compat client lacks their `OnClientCommand` handlers. Expect the game to
   ignore/log unknown commands rather than crash — **verify in the two-client test**. If it errors, add
   a tiny no-op `media/lua/client` file to that stub that acknowledges the command.
2. **Feature loss for compat players.** No Viewpoint models/camera, no Visible Loot overlays, no
   advanced-throwables visuals, no immersive-driving camera. Gameplay items remain server-authoritative.
   Document this as the meaning of the profile, not a bug.
3. **Stub completeness.** Every id the server lists must have a stub on the compat client, or that
   client gets `ModRequired`. Regenerate stubs whenever the pack changes (`tools/emit_stubs.py` or the
   launcher's lite profile).
4. **Checksum must stay off.** Re-enabling `DoLuaChecksum` makes stubs and real copies mismatch and
   kicks everyone with stubs. This is the one hard dependency of the design.
5. **Workshop precedence.** Launching without `-modfolders mods,workshop,steam` can let a Workshop copy
   win over the stub. The launcher sets it; manual players must set it in Steam Launch Options.

---

## 5. Verification plan (two clients, one server)

1. Server: 314 ids, `DoLuaChecksum=false`, `WorkshopItems=` empty. Start it.
2. Client A (full): join. Confirm Viewpoint visuals, Visible Loot overlays, immersive camera.
3. Client B (compat/stubs): join. **Pass:** no `ModRequired`, no `checksum` kick, `console.txt` free of
   RPC errors. **Fail:** kick reason or repeated Lua errors — capture the exact line.
4. On B, test the three server-Lua mods: throw a throwable, enter a vehicle, loot containers.
5. On B, confirm no Viewpoint models (expected) and that base gameplay is unaffected.
6. Reconnect both, save/reload the server, repeat once.

## 6. Evidence appendix

Decompiled with CFR 0.152 from `Project Zomboid.app/Contents/Java/projectzomboid.jar` (42.21.0,
rev `4a0e9546ec`):

- `zombie/ZomboidFileSystem.loadMods`, `loadModsAux`, `loadModAndRequired`
- `zombie/gameStates/ConnectToServerState.CheckMods`
- `zombie/gameStates/ChooseGameInfo.getModDetails`, `readModInfo`, `Mod.isAvailableSelf`, `Mod.isAvailableRequired`
- `zombie/network/ConnectionDetails.writeMods`
- `zombie/network/NetChecksum`, `zombie/network/packets/service/ChecksumPacket`
- `zombie/iso/IsoWorld` (DoLuaChecksum gate), `zombie/network/ServerOptions.doLuaChecksum`
- `zombie/network/anticheats/AntiCheatChecksumUpdate`
