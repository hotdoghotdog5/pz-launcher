#!/usr/bin/env python3
r"""
Upload the output of tools/build_objects.py to S3 or an S3-compatible endpoint.

A dry run is the default and makes no S3 calls. A real upload needs BOTH --no-dry-run and --yes.

Keys (under --prefix, default pzpack):
  <prefix>/objects/<tree_sha256>.tar.gz   immutable, shared by every pack version
  <prefix>/<version>/pack.json            Cache-Control no-cache
  <prefix>/<version>/manifest.json        Cache-Control no-cache
  <prefix>/latest.json                    only with --update-latest; the one mutable pointer

Every upload records the local sha256 as S3 user metadata ("sha256"). Before the first write the tool
HEADs every key and classifies it:
  * object missing                      -> upload it
  * object present, recorded sha256 same -> skip (never overwritten)
  * object present, recorded sha256 differs or absent -> stop: the manifest would point at bytes that
    do not match. Nothing is uploaded.
  * metadata key missing                -> write it after the objects
  * metadata key present, same size and sha256 -> identical, not rewritten
  * metadata key present, otherwise     -> stop: a published version is never overwritten.
Objects are written before metadata, and metadata before latest.json, so a manifest never references a
missing object.

Credentials come only from the boto3 default chain (profile, environment, shared config, role). This tool
never reads or prints credential values; it prints the provider name only. A real upload without
resolvable credentials is refused. boto3 is imported only for a real upload.

Legacy launcher files (layers such as base.tar.gz) are NOT uploaded here; they stay on the
tools/build_dist.py and tools/upload_s3.py path.

Examples:
  python3 -I tools/upload_objects.py --dir /tmp/pzbuild-objects --bucket BUCKET --prefix pzpack
  python3 -I tools/upload_objects.py --dir /tmp/pzbuild-objects --bucket BUCKET --profile PROFILE \
      --region us-east-1 --no-dry-run --yes --update-latest
"""

import argparse
import hashlib
import json
import os
import sys
from typing import NamedTuple

OBJECT_CONTENT_TYPE = "application/gzip"
OBJECT_CACHE_CONTROL = "public, max-age=31536000, immutable"
JSON_CONTENT_TYPE = "application/json"
META_CACHE_CONTROL = "no-cache"
MISSING_CODES = frozenset({"404", "NoSuchKey", "NotFound"})
CHUNK = 1 << 20
EXIT_OK = 0
EXIT_ERROR = 1
EXIT_USAGE = 2


class UploadError(Exception):
    """The upload cannot proceed safely; nothing is sent after this."""


class CredentialsMissing(UploadError):
    """No credentials resolved through the boto3 default chain."""


class Upload(NamedTuple):
    kind: str
    local: str
    key: str
    size: int
    sha256: str
    content_type: str
    cache_control: str


class Head(NamedTuple):
    size: int
    sha256: str  # the "sha256" user-metadata value recorded at upload time, or ""


def _sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(CHUNK), b""):
            h.update(chunk)
    return h.hexdigest()


def _load_manifest(out_dir):
    path = os.path.join(out_dir, "manifest.json")
    if not os.path.isfile(path):
        raise UploadError(f"no manifest.json in {out_dir}")
    with open(path, "rb") as f:
        manifest = json.loads(f.read().decode("utf-8"))
    if manifest.get("schema") != 1 or manifest.get("items_schema") != 2:
        raise UploadError("manifest.json must have schema 1 and items_schema 2 (written by tools/build_objects.py)")
    return manifest


def build_plan(out_dir, prefix, update_latest):
    """Local-only checks and the keys to write. Makes no network calls."""
    prefix = prefix.strip("/")
    if not prefix:
        raise UploadError("--prefix must not be empty")
    manifest = _load_manifest(out_dir)
    version = manifest.get("pack_version")
    if not isinstance(version, str) or not version or "/" in version or version in (".", ".."):
        raise UploadError(f"unusable pack_version {version!r}")

    objects = []
    seen = {}
    for item in manifest.get("items", []):
        obj = item["object"]
        url = obj["url"]
        parts = url.split("/")
        if len(parts) != 2 or parts[0] != "objects" or not parts[1].endswith(".tar.gz") \
                or parts[1] in ("", ".", "..", ".tar.gz"):
            raise UploadError(f"unexpected object url {url!r}")
        if url in seen:
            if seen[url] != obj["sha256"]:
                raise UploadError(f"two items disagree on the sha256 of {url}")
            continue
        local = os.path.join(out_dir, parts[0], parts[1])
        if not os.path.isfile(local):
            raise UploadError(f"missing local object {local}")
        size = os.path.getsize(local)
        if size != obj["bytes"]:
            raise UploadError(f"{local}: {size} bytes on disk, manifest says {obj['bytes']}")
        digest = _sha256_file(local)
        if digest != obj["sha256"]:
            raise UploadError(f"{local}: sha256 does not match the manifest")
        seen[url] = digest
        objects.append(Upload("object", local, f"{prefix}/{url}", size, digest,
                              OBJECT_CONTENT_TYPE, OBJECT_CACHE_CONTROL))

    metas = []
    for name in ("pack.json", "manifest.json"):
        local = os.path.join(out_dir, name)
        if not os.path.isfile(local):
            raise UploadError(f"missing {local}")
        size = os.path.getsize(local)
        digest = _sha256_file(local)
        if name == "pack.json" and (digest != manifest.get("pack_sha256") or size != manifest.get("pack_bytes")):
            raise UploadError("pack.json does not match the manifest's pack_sha256 / pack_bytes")
        metas.append(Upload("meta", local, f"{prefix}/{version}/{name}", size, digest,
                            JSON_CONTENT_TYPE, META_CACHE_CONTROL))

    latest = metas[1]._replace(kind="latest", key=f"{prefix}/latest.json") if update_latest else None
    return {"version": version, "objects": objects, "meta": metas, "latest": latest}


def _error_code(exc):
    response = getattr(exc, "response", None) or {}
    error = response.get("Error") or {}
    return str(error.get("Code", ""))


def head_object(client, bucket, key):
    """Head for an existing key, None when missing. Any other error is raised, never read as 'absent'."""
    try:
        resp = client.head_object(Bucket=bucket, Key=key)
    except Exception as exc:  # botocore ClientError in production; a fake in tests
        code = _error_code(exc)
        if code in MISSING_CODES:
            return None
        raise UploadError(f"HEAD s3://{bucket}/{key} failed ({code or type(exc).__name__}); "
                          "not guessing whether it exists") from exc
    metadata = resp.get("Metadata") or {}
    return Head(int(resp.get("ContentLength", -1)), str(metadata.get("sha256", "")))


def make_s3_client(profile=None, region=None, endpoint_url=None, log=print):
    """S3 client through the boto3 default chain. Refuses when no credentials resolve."""
    try:
        import boto3  # pyright: ignore[reportMissingImports]  # lazy: only a real upload needs boto3
    except ImportError as exc:
        raise UploadError("boto3 is required for a real upload: pip install boto3") from exc
    try:
        session = boto3.session.Session(profile_name=profile or None, region_name=region or None)
        creds = session.get_credentials()
    except Exception as exc:  # e.g. unknown profile; the message carries no credential values
        raise UploadError(f"AWS session setup failed: {type(exc).__name__}: {exc}") from exc
    if creds is None:
        raise CredentialsMissing("no AWS credentials resolved through the boto3 default chain; nothing was uploaded. "
                                 "Set AWS_PROFILE, run `aws configure`, or pass --profile.")
    log(f"credentials: resolved via {getattr(creds, 'method', 'unknown')} (values not shown)")
    return session.client("s3", endpoint_url=endpoint_url or None)


def _put(client, bucket, up, log):
    with open(up.local, "rb") as f:
        body = f.read()
    client.put_object(Bucket=bucket, Key=up.key, Body=body, ContentType=up.content_type,
                      CacheControl=up.cache_control, Metadata={"sha256": up.sha256})
    check = head_object(client, bucket, up.key)
    if check is None or check.size != up.size or check.sha256 != up.sha256:
        raise UploadError(f"post-upload check failed for {up.key}")
    log(f"  wrote {up.key} ({up.size} bytes)")


def _print_plan(plan, bucket, real, log):
    mode = "UPLOAD" if real else "DRY RUN: no S3 calls will be made"
    total = sum(o.size for o in plan["objects"])
    log(f"{mode}  bucket={bucket}  pack version={plan['version']}")
    log(f"objects: {len(plan['objects'])} ({total / 1e9:.2f} GB); a real run checks each key first and "
        "skips any that already hold the same bytes")
    for o in plan["objects"]:
        log(f"  object  s3://{bucket}/{o.key}  {o.size} bytes  Cache-Control: immutable")
    for m in plan["meta"]:
        log(f"  meta    s3://{bucket}/{m.key}  {m.size} bytes  Cache-Control: no-cache")
    if plan["latest"] is not None:
        log(f"  latest  s3://{bucket}/{plan['latest'].key}  overwrites the mutable pointer")
    if not real:
        log("dry run only. A real upload needs --no-dry-run and --yes.")


def run_upload(out_dir, bucket, prefix="pzpack", dry_run=True, yes=False, update_latest=False,
               client=None, profile=None, region=None, endpoint_url=None, log=print):
    """Plan, and upload only when dry_run is False and yes is True. Returns a summary dict."""
    if not bucket:
        raise UploadError("--bucket is required")
    plan = build_plan(out_dir, prefix, update_latest)
    real = (not dry_run) and yes
    summary = {"real": real, "objects": len(plan["objects"]),
               "object_bytes": sum(o.size for o in plan["objects"]),
               "uploaded": 0, "skipped": 0, "uploaded_bytes": 0, "meta_written": 0, "latest_written": False}
    if not real:
        _print_plan(plan, bucket, False, log)
        return summary
    # Credentials are resolved before the plan is announced as an upload, so a refused run never claims one.
    if client is None:
        client = make_s3_client(profile, region, endpoint_url, log)
    _print_plan(plan, bucket, True, log)

    # Phase A: classify every key before anything is written. Any conflict stops here.
    to_upload = []
    present = []
    for obj in plan["objects"]:
        head = head_object(client, bucket, obj.key)
        if head is None:
            to_upload.append(obj)
        elif head.sha256 == obj.sha256 and head.size == obj.size:
            present.append(obj)
        else:
            raise UploadError(f"s3://{bucket}/{obj.key} exists with a different recorded sha256 (or none). "
                              "Publishing would point clients at bytes that do not match. Nothing was uploaded.")
    meta_to_write = []
    for meta in plan["meta"]:
        head = head_object(client, bucket, meta.key)
        if head is None:
            meta_to_write.append(meta)
        elif head.size == meta.size and head.sha256 == meta.sha256:
            log(f"  identical, already present: {meta.key}")
        else:
            raise UploadError(f"s3://{bucket}/{meta.key} already exists with different content; refusing to "
                              "overwrite a published version. Nothing was uploaded.")

    # Phase B: objects, never overwritten.
    for obj in present:
        summary["skipped"] += 1
        log(f"  exists with the same sha256, skipped: {obj.key}")
    for obj in to_upload:
        client.upload_file(obj.local, bucket, obj.key,
                           ExtraArgs={"ContentType": obj.content_type, "CacheControl": obj.cache_control,
                                      "Metadata": {"sha256": obj.sha256}})
        check = head_object(client, bucket, obj.key)
        if check is None or check.size != obj.size or check.sha256 != obj.sha256:
            raise UploadError(f"post-upload check failed for {obj.key}")
        summary["uploaded"] += 1
        summary["uploaded_bytes"] += obj.size
        log(f"  uploaded {obj.key} ({obj.size} bytes)")

    # Phase C: versioned metadata, only after every object it references.
    for meta in meta_to_write:
        _put(client, bucket, meta, log)
        summary["meta_written"] += 1

    # Phase D: the mutable pointer, only when asked for.
    if plan["latest"] is not None:
        log("  updating the mutable latest.json pointer")
        _put(client, bucket, plan["latest"], log)
        summary["latest_written"] = True
    return summary


def parse_args(argv=None):
    ap = argparse.ArgumentParser(description="Upload tools/build_objects.py output. Dry run unless --no-dry-run --yes.")
    ap.add_argument("--dir", required=True, help="output directory of tools/build_objects.py")
    ap.add_argument("--bucket", required=True, help="S3 bucket (or S3-compatible bucket)")
    ap.add_argument("--prefix", default="pzpack", help="key prefix (default: pzpack)")
    ap.add_argument("--region", default="", help="AWS region (default: from the boto3 chain)")
    ap.add_argument("--profile", default="", help="AWS profile name (default: from the boto3 chain)")
    ap.add_argument("--endpoint-url", default="", help="S3-compatible endpoint, e.g. R2, B2, MinIO")
    ap.add_argument("--dry-run", dest="dry_run", action="store_true", default=True,
                    help="print the plan and make no S3 calls (default)")
    ap.add_argument("--no-dry-run", dest="dry_run", action="store_false",
                    help="allow a real upload; also requires --yes")
    ap.add_argument("--yes", action="store_true", help="confirm a real upload (required with --no-dry-run)")
    ap.add_argument("--update-latest", action="store_true",
                    help="also point <prefix>/latest.json at this manifest (default: off)")
    return ap.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    if not args.dry_run and not args.yes:
        print("refusing: a real upload needs --yes as well as --no-dry-run", file=sys.stderr)
        return EXIT_USAGE
    if args.yes and args.dry_run:
        print("note: --yes has no effect without --no-dry-run; printing the plan only", file=sys.stderr)
    try:
        summary = run_upload(args.dir, args.bucket, prefix=args.prefix, dry_run=args.dry_run, yes=args.yes,
                             update_latest=args.update_latest, profile=args.profile or None,
                             region=args.region or None, endpoint_url=args.endpoint_url or None)
    except CredentialsMissing as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_USAGE
    except UploadError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return EXIT_ERROR
    if summary["real"]:
        print(f"done: uploaded {summary['uploaded']} object(s) ({summary['uploaded_bytes']} bytes), "
              f"skipped {summary['skipped']} already present")
    return EXIT_OK


if __name__ == "__main__":
    sys.exit(main())
