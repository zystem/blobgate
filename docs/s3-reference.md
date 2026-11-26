# The S3 door: what it answers

Reference page. How to use it: [s3.md](s3.md). The door (`BLOBGATE_S3_PORT`) turns each call of S3 into a call of the core on the same code, so an object written through one is read through the other.

## Names

| In S3 | Here |
|---|---|
| access key | the name of a tenant |
| secret key | `blobgate s3-key <tenant> <secret>` (HMAC of the tenant's secret; it does not give the secret away) |
| bucket | the name of a store (`_default` is the store with no name) |
| key | the key of the object under that tenant (the same key as in the core's API: at most 1024 characters, no empty, `.` or `..` part) |
| region | any; the answer says `us-east-1` |
| address | path-style only: `http://host:port/bucket/key` |

A tenant sees its own keys only, through either door.

## Signatures

Signature Version 4 only (**boto3** makes a presigned address with Signature Version 2 unless the client is made with `Config(signature_version="s3v4")`), in the header or in the query (a presigned address, up to 7 days). The payload may be signed (`x-amz-content-sha256` is its digest, which is checked as the body is written), `UNSIGNED-PAYLOAD`, or streamed
(`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, with or without `-TRAILER`, and `STREAMING-UNSIGNED-PAYLOAD-TRAILER`): every chunk of a signed stream is checked as its last byte is read, and a signed trailer is checked too; a chunk or trailer that is wrong ends the body with an error, and nothing is committed of a body that did not end well.

What the signature must cover: a header that decides what a call does must be in `SignedHeaders` (`x-amz-copy-source` and its `-range` and `-if-*`, `x-amz-metadata-directive`, `x-amz-tagging-directive`, `x-amz-meta-*`, `x-amz-checksum-*`, `x-amz-trailer`, `x-amz-sdk-checksum-algorithm`, `x-amz-decoded-content-length`, `Content-MD5`); a call that carries one that is not signed is refused with `403 AccessDenied`, as S3 does.

What is checked of a body: a signed payload digest (also of the XML of DeleteObjects and CompleteMultipartUpload, before anything is done with it), `Content-MD5`, and a declared checksum (`x-amz-checksum-crc32`, `-crc32c`, `-crc64nvme`, `-sha1`, `-sha256`, in a header or in the trailer): a wrong one is `400 BadDigest`, and a checksum of another algorithm is refused rather than ignored. `UNSIGNED-PAYLOAD` with no checksum is not checked. The time of the request must be within 15 minutes of the time of the module.

## Calls

| Call | Answer |
|---|---|
| ListBuckets | the stores |
| HeadBucket, GetBucketLocation | yes |
| GetBucketVersioning | yes: an empty configuration (a store keeps no versions); enabling it is `501` |
| ListObjectsV2, ListObjects (v1) | yes: `prefix`, `delimiter`, `max-keys` (up to 1000), `continuation-token`, `start-after`, `marker`, `encoding-type=url`; read from the inventory, no node is asked |
| PutObject | yes: `Content-Type`, `x-amz-meta-*`, `If-Match`, `If-None-Match` (decided in the transaction that writes: of two writers under one condition one is `412`; an `If-Match` takes a strong tag only), `Content-MD5` (checked; a wrong one is `BadDigest`) |
| GetObject, HeadObject | yes: `Range`, `If-Match`, `If-None-Match`, `If-Modified-Since`; streamed |
| DeleteObject, DeleteObjects | yes (up to 1000 keys). With a trash (`soft_delete_seconds`) the object goes to the trash |
| CreateMultipartUpload, UploadPart, CompleteMultipartUpload, AbortMultipartUpload, ListParts | yes, with the limit below |
| CopyObject | yes: `x-amz-copy-source` (`/bucket/key`, URL-encoded; a store of the same tenant), `x-amz-metadata-directive` `COPY` (default) or `REPLACE`. In one store whose object is a set of blocks (`dedup` on) it takes a reference on the set: no byte is read or written, whatever the size. Any other copy (another store, or a store with no `dedup`) reads the source and writes the destination, a block at a time. A copy onto itself is `400 InvalidRequest` unless it replaces the metadata. `x-amz-copy-source-if-match`, `-if-none-match`, `-if-modified-since` and `-if-unmodified-since` are checked (`412 PreconditionFailed`); a `?versionId` in the source is `501` |
| UploadPartCopy | yes: a part of an upload that is a range (`x-amz-copy-source-range: bytes=first-last`) of an object of the tenant, or all of it, with the conditions of CopyObject. The part is read and held whole, so it is at most 8 MiB and the other limits of a part (all but the last of one size) hold; this is what the AWS CLI does for an object above its `multipart_threshold` (8 MB by default), with its default part of 8 MiB |
| ListMultipartUploads | yes: the uploads in parts of the tenant that have begun and not ended, by key and upload id; `prefix`, `delimiter` (folded into `CommonPrefixes`), `key-marker`, `upload-id-marker`, `max-uploads` (up to 1000), `encoding-type=url`. A write in one call is not listed. An upload that nobody finishes goes after `BLOBGATE_UPLOAD_TTL_SECONDS` (24 hours); one list reads at most 20000 uploads |
| CreateBucket, DeleteBucket | `409 BucketAlreadyOwnedByYou` for a store, `403 AccessDenied` otherwise: the operator makes and removes stores |
| acl, tagging, versioning, lifecycle, policy, cors, retention, select, ... | `501 NotImplemented` (a call with such a subresource is refused, never taken for a plain read or write) |

Virtual-hosted addresses (`bucket.host`) are not served. Multipart parts: **at most 8 MiB, and every part but the last as long as the first** (the blocks of an object are of one size). A part over 8 MiB is `400 EntityTooLarge` with the
words that say what to set; parts of different sizes are `400 InvalidRequest` when the upload is completed, and nothing is written.

## What differs from S3

* **ETag**: the MD5 of the bytes (in quotes) for an object written in one call, as S3 has it, so that the tools that check it (`s3cmd put`) are content. For an object made of parts it is what S3 makes it: the MD5 of the MD5s
  of the parts one after another, a dash and the number of parts (`"<32 hex>-<number of parts>"`), the same as S3 gives for the same parts (checked with the AWS CLI, boto3 and the SDKs of the scripts in `tools/sdk-check/`).
  An object of an upload that began before 0.11.0, or whose parts came through the shim port with no MD5 kept, has the older name `"<32 hex>-<number of blocks>"` (not the MD5 of anything the tools can compare), and an object
  of a store with `dedup` whose bytes were written in one call too has the MD5 of the bytes. The ETag of a part (the answer of UploadPart) is the module's own name (`<sha256>-<size>`), not the MD5 of the part: send it back as it is.
  It is the same through the core's API, in a listing and after a copy.
* **Same bytes**: with `dedup` the objects of the same bytes share their blocks ([deduplication.md](deduplication.md)); nothing in the answers shows it.
* **Time of creation of a bucket** is 1970: a store has none that is kept.
* **Errors** are the words of S3 (`NoSuchKey`, `NoSuchBucket`, `NoSuchUpload`, `PreconditionFailed`, `InvalidRange`, `BadDigest`, `XAmzContentSHA256Mismatch`, `SignatureDoesNotMatch` (also for an access key that is no tenant: the answer does not say which tenants there are; the log does), `RequestTimeTooSkewed`,
  `EntityTooLarge`, `NotImplemented`, `ServiceUnavailable`); a refusal for want of room or a quota is `507 InsufficientStorage` with the words of the core's refusal. `Retry-After` is set with 503 and 507. The words of a failure of the database or of a node end with `(ref <id>)`, the reference of the log line that has the whole text.
* Consistency, quotas, the limit of the size of an object (`BLOBGATE_MAX_OBJECT_BYTES`) and the checks of the nodes are those of the core.

Metrics: the routes `s3_buckets`, `s3_bucket`, `s3_delete_many`, `s3_put`, `s3_get`, `s3_head`, `s3_delete`, `s3_part_put`, `s3_parts`, `s3_upload_begin`, `s3_upload_complete`, `s3_upload_abort` ([metrics.md](metrics.md)).
