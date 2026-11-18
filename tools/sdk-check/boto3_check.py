#!/usr/bin/env python3
"""What the S3 door answers to boto3 (the Python SDK of AWS): objects, parts, copies, lists, presigned addresses, deletes.

usage: boto3_check.py ENDPOINT ACCESS_KEY SECRET_KEY BUCKET     (the keys: `blobgate s3-key <tenant> <secret>`)
Exit status 0 when every step passed. Written to a bucket of a store: the keys it makes are removed at the end.
"""
import hashlib
import os
import sys
import tempfile
import urllib.request

import boto3
from boto3.s3.transfer import TransferConfig
from botocore.config import Config
from botocore.exceptions import ClientError

endpoint, ak, sk, bucket = sys.argv[1:5]
s3 = boto3.client("s3", endpoint_url=endpoint, aws_access_key_id=ak, aws_secret_access_key=sk, region_name="eu-west-1",
                  config=Config(s3={"addressing_style": "path"}, signature_version="s3v4", retries={"max_attempts": 1}))  # presigned addresses: the default for a custom endpoint is Signature Version 2, which the door does not take
failed = []


def step(name, fn):
    try:
        fn()
        print("PASS", name)
    except Exception as e:  # noqa: BLE001 - every kind of failure is a failed step
        failed.append(name)
        print("FAIL", name, "->", type(e).__name__, str(e)[:300])


def body(n):
    return (hashlib.sha256(b"seed").digest() * (n // 32 + 1))[:n]


def sha(b):
    return hashlib.sha256(b).hexdigest()


big = body(20 * 1024 * 1024 + 321)
small = b"hello from boto3"


def put_get():
    r = s3.put_object(Bucket=bucket, Key="b/small", Body=small, ContentType="text/x-boto", Metadata={"owner": "py"})
    assert r["ETag"].strip('"') == hashlib.md5(small).hexdigest(), r["ETag"]
    h = s3.head_object(Bucket=bucket, Key="b/small")
    assert h["ContentLength"] == len(small) and h["ContentType"] == "text/x-boto" and h["Metadata"] == {"owner": "py"}, h
    assert s3.get_object(Bucket=bucket, Key="b/small")["Body"].read() == small


def ranged():
    r = s3.get_object(Bucket=bucket, Key="b/small", Range="bytes=2-5")
    assert r["Body"].read() == small[2:6] and r["ResponseMetadata"]["HTTPStatusCode"] == 206


def managed_upload_download():
    with tempfile.TemporaryDirectory() as d:
        src, dst = os.path.join(d, "src"), os.path.join(d, "dst")
        open(src, "wb").write(big)
        cfg = TransferConfig(multipart_threshold=8 * 1024 * 1024, multipart_chunksize=8 * 1024 * 1024)
        s3.upload_file(src, bucket, "b/big", Config=cfg)
        s3.download_file(bucket, "b/big", dst, Config=cfg)
        assert sha(open(dst, "rb").read()) == sha(big)


def copies():
    s3.copy_object(Bucket=bucket, Key="b/small-copy", CopySource={"Bucket": bucket, "Key": "b/small"})
    assert s3.get_object(Bucket=bucket, Key="b/small-copy")["Body"].read() == small
    s3.copy_object(Bucket=bucket, Key="b/small-meta", CopySource={"Bucket": bucket, "Key": "b/small"}, MetadataDirective="REPLACE", Metadata={"owner": "other"})
    assert s3.head_object(Bucket=bucket, Key="b/small-meta")["Metadata"] == {"owner": "other"}
    # the managed copy: an object above the threshold is copied in parts (UploadPartCopy), with the conditions on the source
    cfg = TransferConfig(multipart_threshold=8 * 1024 * 1024, multipart_chunksize=8 * 1024 * 1024)
    s3.copy({"Bucket": bucket, "Key": "b/big"}, bucket, "b/big-copy", Config=cfg)
    got = s3.get_object(Bucket=bucket, Key="b/big-copy")["Body"].read()
    assert sha(got) == sha(big), len(got)


def listing():
    keys, token = [], None
    while True:
        kw = {"Bucket": bucket, "Prefix": "b/", "MaxKeys": 2}
        if token:
            kw["ContinuationToken"] = token
        r = s3.list_objects_v2(**kw)
        keys += [o["Key"] for o in r.get("Contents", [])]
        if not r["IsTruncated"]:
            break
        token = r["NextContinuationToken"]
    assert sorted(keys) == ["b/big", "b/big-copy", "b/small", "b/small-copy", "b/small-meta"], keys
    r = s3.list_objects_v2(Bucket=bucket, Delimiter="/")
    assert "b/" in [p["Prefix"] for p in r.get("CommonPrefixes", [])], r


def presigned():
    url = s3.generate_presigned_url("get_object", Params={"Bucket": bucket, "Key": "b/small"}, ExpiresIn=300)
    assert urllib.request.urlopen(url).read() == small
    put = s3.generate_presigned_url("put_object", Params={"Bucket": bucket, "Key": "b/presigned-put"}, ExpiresIn=300)
    req = urllib.request.Request(put, data=b"through a presigned address", method="PUT")
    urllib.request.urlopen(req).read()
    assert s3.get_object(Bucket=bucket, Key="b/presigned-put")["Body"].read() == b"through a presigned address"


def missing():
    try:
        s3.head_object(Bucket=bucket, Key="b/not-there")
    except ClientError as e:
        assert e.response["ResponseMetadata"]["HTTPStatusCode"] == 404, e
    else:
        raise AssertionError("a key that is not there was found")
    try:
        s3.get_object(Bucket=bucket, Key="b/not-there")
    except ClientError as e:
        assert e.response["Error"]["Code"] == "NoSuchKey", e
    else:
        raise AssertionError("a key that is not there was read")


def uploads_in_progress():
    ids = [s3.create_multipart_upload(Bucket=bucket, Key="b/u/%d" % i)["UploadId"] for i in range(3)]
    r = s3.list_multipart_uploads(Bucket=bucket, Prefix="b/u/")
    assert sorted(u["Key"] for u in r["Uploads"]) == ["b/u/0", "b/u/1", "b/u/2"] and not r["IsTruncated"], r
    pages, marker = [], {}
    while True:
        r = s3.list_multipart_uploads(Bucket=bucket, Prefix="b/u/", MaxUploads=1, **marker)
        pages += [u["Key"] for u in r.get("Uploads", [])]
        if not r["IsTruncated"]:
            break
        marker = {"KeyMarker": r["NextKeyMarker"], "UploadIdMarker": r["NextUploadIdMarker"]}
    assert pages == ["b/u/0", "b/u/1", "b/u/2"], pages
    for i, up in enumerate(ids):
        s3.abort_multipart_upload(Bucket=bucket, Key="b/u/%d" % i, UploadId=up)
    assert not s3.list_multipart_uploads(Bucket=bucket, Prefix="b/u/").get("Uploads")


def deletes():
    s3.delete_object(Bucket=bucket, Key="b/small")
    r = s3.delete_objects(Bucket=bucket, Delete={"Objects": [{"Key": k} for k in ["b/big", "b/big-copy", "b/small-copy", "b/small-meta", "b/presigned-put"]]})
    assert not r.get("Errors"), r
    assert s3.list_objects_v2(Bucket=bucket, Prefix="b/").get("KeyCount", 0) == 0


for name, fn in [("put, head, get, the MD5 ETag", put_get), ("range read", ranged), ("upload_file and download_file of 20 MiB in parts", managed_upload_download),
                 ("CopyObject, metadata REPLACE, managed copy in parts", copies), ("listing, pages, delimiter", listing), ("presigned GET and PUT", presigned),
                 ("uploads in progress: list, pages, abort", uploads_in_progress), ("a key that is not there", missing), ("delete, delete_objects", deletes)]:
    step(name, fn)
print("boto3", boto3.__version__, "-", "FAILED: " + ", ".join(failed) if failed else "all passed")
sys.exit(1 if failed else 0)
