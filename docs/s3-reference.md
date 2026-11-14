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

Signature Version 4, in the header or in the query (a presigned address, up to 7 days). The payload may be signed (`x-amz-content-sha256` is its digest, which is checked as the body is written), `UNSIGNED-PAYLOAD`, or streamed
(`STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, with or without `-TRAILER`, and `STREAMING-UNSIGNED-PAYLOAD-TRAILER`): every chunk of a signed stream is checked before its bytes are used. A checksum that comes in a trailer
(`x-amz-checksum-*`) is read and **not checked**: the module checks the digests it makes itself. The time of the request must be within 15 minutes of the time of the module.

## Calls

| Call | Answer |
|---|---|
| ListBuckets | the stores |
| HeadBucket, GetBucketLocation | yes |
| GetBucketVersioning | yes: an empty configuration (a store keeps no versions); enabling it is `501` |
| ListObjectsV2, ListObjects (v1) | yes: `prefix`, `delimiter`, `max-keys` (up to 1000), `continuation-token`, `start-after`, `marker`, `encoding-type=url`; read from the inventory, no node is asked |
| PutObject | yes: `Content-Type`, `x-amz-meta-*`, `If-Match`, `If-None-Match`, `Content-MD5` (checked; a wrong one is `BadDigest`) |
| GetObject, HeadObject | yes: `Range`, `If-Match`, `If-None-Match`, `If-Modified-Since`; streamed |
| DeleteObject, DeleteObjects | yes (up to 1000 keys). With a trash (`soft_delete_seconds`) the object goes to the trash |
| CreateMultipartUpload, UploadPart, CompleteMultipartUpload, AbortMultipartUpload, ListParts | yes, with the limit below |
| CopyObject | `501 NotImplemented`: read and write, or use `?link` of the core's API for bytes the store keeps |
| ListMultipartUploads | `501 NotImplemented` |
| CreateBucket, DeleteBucket | `409 BucketAlreadyOwnedByYou` for a store, `403 AccessDenied` otherwise: the operator makes and removes stores |
| acl, tagging, versioning, lifecycle, policy, cors, retention, select, ... | `501 NotImplemented` (a call with such a subresource is refused, never taken for a plain read or write) |

Virtual-hosted addresses (`bucket.host`) are not served. Multipart parts: **at most 8 MiB, and every part but the last as long as the first** (the blocks of an object are of one size). A part over 8 MiB is `400 EntityTooLarge` with the
words that say what to set; parts of different sizes are `400 InvalidRequest` when the upload is completed, and nothing is written.

## What differs from S3

* **ETag**: the MD5 of the bytes (in quotes) for an object written in one call, as S3 has it, so that the tools that check it (`s3cmd put`) are content. An object of a multipart upload, or one written by an earlier release, has a name
  with a dash (`"<32 hex>-<number of blocks>"`), as a multipart object of S3 has, which the tools do not compare with the bytes. It is the same through the core's API.
* **Same bytes**: with `dedup` the objects of the same bytes share their blocks ([deduplication.md](deduplication.md)); nothing in the answers shows it.
* **Time of creation of a bucket** is 1970: a store has none that is kept.
* **Errors** are the words of S3 (`NoSuchKey`, `NoSuchBucket`, `NoSuchUpload`, `PreconditionFailed`, `InvalidRange`, `BadDigest`, `XAmzContentSHA256Mismatch`, `SignatureDoesNotMatch`, `InvalidAccessKeyId`, `RequestTimeTooSkewed`,
  `EntityTooLarge`, `NotImplemented`, `ServiceUnavailable`); a refusal for want of room or a quota is `507 InsufficientStorage` with the words of the core's refusal. `Retry-After` is set with 503 and 507.
* Consistency, quotas, the limit of the size of an object (`BLOBGATE_MAX_OBJECT_BYTES`) and the checks of the nodes are those of the core.

Metrics: the routes `s3_buckets`, `s3_bucket`, `s3_delete_many`, `s3_put`, `s3_get`, `s3_head`, `s3_delete`, `s3_part_put`, `s3_parts`, `s3_upload_begin`, `s3_upload_complete`, `s3_upload_abort` ([metrics.md](metrics.md)).
