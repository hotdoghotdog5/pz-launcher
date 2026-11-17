# Distribution deployment — pack 2026.11.03

> **Status:** this document records the first release (pack 2026.11.03, two big layer archives). Current releases publish one archive per workshop item under `pzpack/objects/` (`items_schema` 2); see the README, `tools/build_objects.py` and `tools/upload_objects.py`. Sizes and checks below are historical.

## Bucket

| | |
|---|---|
| Bucket | `pzpack-895941021447` |
| Region | `us-east-1` |
| Base URL | `https://pzpack-895941021447.s3.us-east-1.amazonaws.com` |
| Manifest (stable) | `…/pzpack/latest.json` |
| This version | `…/pzpack/2026.11.03/manifest.json` |
| Objects | `pack.json`, `base.tar.gz` (8.71 GB), `client.tar.gz` (0.30 GB), `manifest.json` |

## Client access model

The launcher needs **only anonymous HTTPS GET** on exact object keys. No AWS key is shipped or
configured on players' machines. The bucket policy grants:

* `s3:GetObject` on `arn:aws:s3:::pzpack-895941021447/pzpack/*` to everyone;
* `Deny` on `s3:*` when `aws:SecureTransport=false` (TLS only).

Everything else is denied, including `s3:ListBucket`, `PutObject` and `DeleteObject`. Verified:

| Anonymous request | Result |
|---|---|
| `GET /pzpack/<object>` | 200 |
| `HEAD /pzpack/<object>` | 200 |
| `PUT /pzpack/<object>` | 403 |
| `DELETE /pzpack/<object>` | 403 |
| `GET /?list-type=2` | 403 |

The bucket has ACLs disabled (`BucketOwnerEnforced`), Block Public ACLs on, and the policy is the only
public path. The policy JSON template is `dist/2026.11.03/bucket-policy.json`.

**Private alternative:** if the pack should not be public, drop the `PublicReadPack` statement and put
CloudFront (signed URLs/cookies) in front, or generate presigned URLs per release. Presigned URLs
expire (max 7 days), so the baked `latest.json` model needs a CDN or a tiny token service.

## Live verification (2026-11-03)

| Check | Result |
|---|---|
| `base.tar.gz` HEAD | 8,706,899,044 bytes — matches manifest |
| `client.tar.gz` HEAD | 299,924,281 bytes — matches manifest |
| `base.tar.gz` downloaded + SHA-256 | `b03c690f…5d58c46` — matches manifest |
| `client.tar.gz` downloaded + SHA-256 | `af4380e0…6ea60a6` — matches manifest |
| `pzpack --list` from an empty dir | fetched `latest.json`, printed Lite 10.00 GB / Full 10.70 GB |
| `pzpack-macos` download | 7,057,442 bytes, SHA-256 matches the download page |
| Anonymous GET / HEAD | 200 |
| Anonymous PUT / DELETE / ListBucket | 403 |

Costs: ~9 GB S3 Standard storage (~$0.21/month) and ~$0.09/GB egress, so roughly **$0.80 per Full
download** and **$0.78 per Lite download** at us-east-1 list prices.

## Player install

Player binaries (manifest URL baked in; macOS arm64/Intel tested, Windows/Linux compile but are untested):

| Platform | File (under `…/pzpack/`) | Size |
|---|---|---|
| macOS Apple Silicon | `pzpack-macos-arm64` | 6.7 MB |
| macOS Intel | `pzpack-macos-amd64` | 7.1 MB |
| Windows x64 | `pzpack-windows-amd64.exe` | 7.3 MB |
| Windows ARM64 | `pzpack-windows-arm64.exe` | 6.7 MB |
| Linux x64 | `pzpack-linux-amd64` | 7.0 MB |

`dist/pzpack` is the original macOS arm64 build with the manifest URL baked in:

```
https://pzpack-895941021447.s3.us-east-1.amazonaws.com/pzpack/latest.json
```

Run it, pick **Lite** (8.71 GB, no Viewpoint) or **Full** (9.01 GB), and it downloads to the OS cache,
verifies SHA-256, installs, writes `~/Zomboid/mods/default.txt` in the pack's load order, and can launch.

On launch it also prepares the game JVM: `-Xmx8g` on machines with 16 GiB RAM or less (10 GiB above),
and for Full it copies `ZombieBuddy.jar` into the game and adds the loader
(`-javaagent:ZombieBuddy.jar` on macOS/Linux, `-agentlib:zbNative` on Windows). Originals are kept as
`*.pzpack.bak`; changes are re-applied after a Steam update. `--apply-launch-config` does it without
launching, `--no-launch-config` skips it.

Headless:

```bash
./pzpack --from https://pzpack-895941021447.s3.us-east-1.amazonaws.com/pzpack/latest.json \
         --profile lite --no-tui
```

## Publishing the next pack version

```bash
cd pz-launcher
python3 tools/build_dist.py --repo ../pz-server-modpack --pack pack/pack.json \
  --out dist/<version> \
  --base-url https://pzpack-895941021447.s3.us-east-1.amazonaws.com/pzpack/<version>
python3 tools/upload_s3.py --dir dist/<version> \
  --bucket pzpack-895941021447 --region us-east-1
tools/build_all.sh https://pzpack-895941021447.s3.us-east-1.amazonaws.com/pzpack/latest.json dist
```

`upload_s3.py` copies `manifest.json` to `pzpack/latest.json` last, so players only see a complete
release.

## Credentials

Publishing needs object writes on the bucket only. Use a least-privilege IAM identity (a policy template is in
`dist/iam-publisher-policy.json`) through the normal AWS credential chain (`AWS_PROFILE`, `~/.aws/credentials`).
`upload_s3.py` and `upload_objects.py` read that chain and store nothing. Do not publish with root credentials,
and never commit keys. Players need no credentials: they use anonymous HTTPS GET.

Routine publishing does **not** need permission to change the bucket policy. Keep the public-read policy in
place; the client depends on it.
