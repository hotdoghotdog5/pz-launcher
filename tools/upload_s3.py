#!/usr/bin/env python3
"""
Upload a built pack to S3 (or any S3-compatible endpoint: R2, B2, MinIO).

Credentials come from the normal boto3 chain: AWS_PROFILE, ~/.aws/credentials,
AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, or an instance role. Nothing is stored
by this script.

Usage:
  upload_s3.py --dir dist/2026.11.03 --bucket my-pz-pack --region eu-west-1
  upload_s3.py --dir dist/2026.11.03 --bucket my-pz-pack --public-read
  upload_s3.py --dir dist/2026.11.03 --bucket my-pz-pack --endpoint-url https://xxx.r2.cloudflarestorage.com

After upload the launcher URL is:
  https://<bucket>.s3.<region>.amazonaws.com/pzpack/latest.json
or your CDN in front of it.
"""

import argparse
import json
import os
import sys


def human(n):
    return "%.2f GB" % (n / 1e9) if n >= 1e9 else "%.1f MB" % (n / 1e6)


def main():
    try:
        import boto3
        from boto3.s3.transfer import TransferConfig
    except ImportError:
        print("boto3 is required: pip install boto3", file=sys.stderr)
        return 2

    ap = argparse.ArgumentParser()
    ap.add_argument("--dir", required=True)
    ap.add_argument("--bucket", required=True)
    ap.add_argument("--prefix", default="")
    ap.add_argument("--region", default="")
    ap.add_argument("--profile", default="")
    ap.add_argument("--endpoint-url", default="")
    ap.add_argument("--public-read", action="store_true", help="set ACL public-read on every object")
    ap.add_argument("--latest-key", default="pzpack/latest.json")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    manifest_path = os.path.join(args.dir, "manifest.json")
    manifest = json.load(open(manifest_path, encoding="utf-8"))
    version = manifest["pack_version"]
    prefix = (args.prefix or "pzpack/%s" % version).strip("/")

    files = [("pack.json", "application/json"),
             ("manifest.json", "application/json")]
    for label in manifest.get("layers", {}):
        files.append(("%s.tar.gz" % label, "application/gzip"))

    session = boto3.session.Session(profile_name=args.profile or None,
                                    region_name=args.region or None)
    if not args.dry_run and session.get_credentials() is None:
        print("No AWS credentials found.\n"
              "Run `aws configure` (or set AWS_PROFILE / AWS_ACCESS_KEY_ID+SECRET), then retry.\n"
              "Nothing was uploaded.", file=sys.stderr)
        return 2

    s3 = session.client("s3", endpoint_url=args.endpoint_url or None)
    cfg = TransferConfig(multipart_threshold=64 * 1024 * 1024,
                         multipart_chunksize=64 * 1024 * 1024,
                         max_concurrency=4, use_threads=True)

    def progress(name, total):
        state = {"done": 0}

        def cb(n):
            state["done"] += n
            pct = int(state["done"] * 100 / total) if total else 0
            if pct % 5 == 0:
                sys.stdout.write("\r  %s  %3d%%  %s / %s" % (name, pct, human(state["done"]), human(total)))
                sys.stdout.flush()
        return cb

    for name, ctype in files:
        local = os.path.join(args.dir, name)
        if not os.path.exists(local):
            print("missing %s" % local, file=sys.stderr)
            return 1
        key = "%s/%s" % (prefix, name)
        size = os.path.getsize(local)
        extra = {"ContentType": ctype}
        if name.endswith(".tar.gz"):
            extra["CacheControl"] = "public, max-age=31536000, immutable"
        elif name == "manifest.json":
            extra["CacheControl"] = "no-cache"
        else:
            extra["CacheControl"] = "public, max-age=300"
        if args.public_read:
            extra["ACL"] = "public-read"
        print("\n%s -> s3://%s/%s (%s)" % (name, args.bucket, key, human(size)))
        if args.dry_run:
            print("  dry run, not uploading")
            continue
        s3.upload_file(local, args.bucket, key, ExtraArgs=extra, Config=cfg,
                       Callback=progress(name, size))
        print()

    latest_src = "%s/manifest.json" % prefix
    print("\ncopying manifest to s3://%s/%s" % (args.bucket, args.latest_key))
    if not args.dry_run:
        extra = {"ContentType": "application/json", "CacheControl": "no-cache",
                 "MetadataDirective": "REPLACE"}
        if args.public_read:
            extra["ACL"] = "public-read"
        s3.copy_object(Bucket=args.bucket, Key=args.latest_key,
                       CopySource={"Bucket": args.bucket, "Key": latest_src},
                       **extra)

    if args.endpoint_url:
        base = args.endpoint_url.rstrip("/") + "/" + args.bucket
    else:
        region = args.region or session.region_name or "us-east-1"
        base = "https://%s.s3.%s.amazonaws.com" % (args.bucket, region)
    print("\ndone. manifest URL:")
    print("  %s/%s" % (base, args.latest_key))
    return 0


if __name__ == "__main__":
    sys.exit(main())
