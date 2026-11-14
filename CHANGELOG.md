# Changelog

All notable changes. The format is that of [Keep a Changelog](https://keepachangelog.com), the versions are [Semantic Versions](https://semver.org). The API of the module is the one of
`internal/api/openapi.yaml`; a change of it that breaks a caller is a change of the major version (of the minor one while the major is 0).

## [0.6.0] - 2026-11-14

### Added
* **The S3 door** (`BLOBGATE_S3_PORT`, off by default): the AWS CLI, `s3cmd`, the SDKs and presigned addresses read, write, list, delete and upload in parts to a tenant's objects (Signature Version 4, signed and streamed payloads); the access key is the
  tenant, the secret key is `blobgate s3-key <tenant> <secret>`, the bucket is a store. Parts are at most 8 MiB and all but the last of one size. Not answered: CopyObject, ListMultipartUploads, versions, ACLs, policies
  (`501 NotImplemented`). See docs/s3.md and docs/s3-reference.md.
* **Check before the write**: `POST /v1/stores/{store}/objects/{key}?link` with `{sha256, size}` makes the key the bytes that the tenant's store keeps already, sending nothing (a store with `dedup: "tenant"`); 404 `no_such_content`, 409 `dedup_not_tenant`.
* **A Go client**, `github.com/zystem/blobgate/client` (`Put`, `Get`, `Stat`, `Delete`, `List`, `ListAll`, `Link`, `PutOrLink`). See docs/go-client.md.
* `Content-MD5` of a write is checked (400 `md5_mismatch`; `BadDigest` in S3); `cmd/bench -dedup`.
* Fuzz targets for the signature and the chunked body of the S3 door; a test that kills the whole program with SIGKILL during deduplicating writes.

### Changed
* **The `ETag` of an object written in one call is the MD5 of its bytes**, as in S3; that of an object written in parts, or by an earlier release, is `"<hex>-<blocks>"`. Every object that was written before this release has a new ETag once
  (a cache that revalidates with `If-None-Match` fetches it once more). The same bytes now have the same ETag in a store that seals (before, each sealed write was a new version).
* The schema gains the columns `md5` (`storage_objects`, `storage_sets`), `scope` and `digest` (`storage_sets`), added at start.
* The read of an object asks the database once, not twice, for an object that points to a set.

### Fixed
* The listing named another version (`ETag`) than a read for an object that points to a set of blocks (0.5.0, `dedup` on), so that a client that lists and then reads with `If-Match` (the AWS CLI's `sync`) refused the object.

## [0.5.0] - 2026-11-14

### Added
* Deduplication: the store setting `dedup` (`""`, `"tenant"`, `"store"`); objects with the same bytes keep one set of blocks, counted in `storage_sets` in the transaction of the object that takes or gives the reference;
  sets with no reference are removed by the worker. Sealed stores seal such blocks convergently. `GET /v1/admin/stores/{name}:usage` shows `dedup`; metrics `blobgate_dedup_objects_total`,
  `blobgate_dedup_saved_bytes_total`, `blobgate_sets_removed_total`. See docs/deduplication.md.

### Changed
* The schema gains `storage_sets` and the column `cid` of `storage_objects`, added at start. Nothing changes for a store that does not set `dedup`.

## [0.4.0] - 2026-11-13

### Added
* Listing: `GET /v1/stores/{store}/objects` with `prefix`, `limit`, `after` (cursor), `delimiter` (folders) and `trash`; a tenant sees only its own keys.
* Usage and quotas per tenant and store: `GET /v1/stores/{store}/usage`, `GET /v1/admin/stores/{name}:usage`, `GET|PUT|DELETE /v1/admin/quotas[/{store}/{tenant}]`; a write over the limit is 507 `quota_exceeded`.
* Soft delete: the store setting `soft_delete_seconds`; a deleted object is kept in the trash, `POST ...?restore`, `DELETE ...?purge`, and the worker empties the trash. Crash point `delete:after-trash`.

### Changed
* With `soft_delete_seconds` above 0 a `DELETE` answers `{deleted, trash, restorable_until}`. Stores that do not set it behave as before.
* The schema gains `storage_quotas` and the column `deleted_at`, added by the program at start (nothing to do by hand; a downgrade is not supported: docs/upgrading.md).

## [0.3.0] - 2026-11-13

The program is no longer only the storage of one product: it is named and spoken to as a general blob storage service for many tenants, and it speaks plain HTTP. **Nothing that worked stops working**: the old
names of the settings, the old header, the old tokens and grants, and the old stored rows are still accepted (see "Names of the settings" in `docs/configuration.md`).

### Changed
* The settings are `BLOBGATE_<NAME>`; `CINIM_STORAGE_<NAME>` (and `CINIM_RQLITE_URL`, `CINIM_SHARD`, `CINIM_STORAGE_SHARDS`) still work, and the new one wins when both are set.
* A *shard* is now a *tenant*: its name is 1..64 of lowercase letters, digits, `-` and `_` (digits, as before, are fine); the header is `X-Blobgate-Tenant` (`X-Cinim-Shard` still works); `blobgate token`
  makes `v2` tokens and grants `g2` under a domain of the program's own; `v1` and `g1` are still accepted.
* Secrets in the database are sealed as `v3.` under a key of the program's own domain; `v2.` rows are still opened and are sealed again when their store is saved.
* The refusal for want of room ends with a general sentence; `BLOBGATE_FULL_HINT` replaces it with the operator's (the old text named the retention settings of CiNim).
* A range that is past the end of the object is 416 `range_not_satisfiable` with `Content-Range: bytes */<size>` (it was 400); a range is no longer limited to 256 MiB; a read with no `Range` is the whole object
  (it was 400 `range_required`).
* `X-Content-SHA256` is optional for an object (required for a part); an object may be of any length (`BLOBGATE_MAX_OBJECT_BYTES` limits it), of no bytes, and of unknown length (chunked). It was 8 MiB at most in one call.
* The chart: the Service and the Deployment are named after the release; the Secret key is `tenants` (`shards` of the first version is read too); `secrets.tenants` (`secrets.shards` is read too).

### Added
* Plain HTTP access for any client: `GET` of the whole object, `HEAD`, `ETag`, `Last-Modified`, `Accept-Ranges`, `Content-Range`, `If-None-Match` (304), `If-Match` (412), `Range: bytes=a-`, `bytes=-n`;
  `Content-Type` and `X-Blobgate-Meta-<name>` headers kept with the object (also at the beginning of a multipart upload); `If-None-Match: *` and `If-Match` on a write and a delete.

## [0.2.0] - 2026-11-13

### Added
* Secrets as files (`CINIM_STORAGE_*_FILE`), TLS on every port (`CINIM_STORAGE_TLS_CERT_FILE`, `CINIM_STORAGE_TLS_KEY_FILE`, TLS 1.2 or later, certificates read again when they change).
* Signed releases: the files, the image and the chart are signed keyless and have build provenance; the program is built statically and repeatably (the same bytes from the same source); a bill of materials is attached.
* Security documents (policy, threat model, assurance case, requirements), governance, contributing, roadmap, supported versions; CI gates (coverage of 80%, `staticcheck`, `gosec`, `govulncheck`, a repeatable build,
  DCO, dependency review, Scorecard, Trivy, fuzzing).

### Security
* Built with Go 1.26.11: 0.1.0 was built with Go 1.26.0, whose `net/http` has the vulnerability GO-2026-6859 (fixed in Go 1.26.11, found by `govulncheck`). Update from 0.1.0.

### Fixed
* The ids of backups made in one second now sort in the order they were made.
* The integer conversions that `gosec` flags (the id of a sealing key, the class of a status) are done with checks.

## [0.1.0] - 2026-11-13

The first release: the storage module of CiNim, rewritten in Go on rclone.

### Added
* Objects as blocks on the nodes of a store (a mirror of any servers rclone can talk to: FTP, WebDAV, SFTP, S3, a local disk), fixed by one row in rqlite; a block is never overwritten in place, and a read
  checks every block against its SHA-256.
* The HTTP API of the core, the port for step shims (grants), the admin API (`/v1/admin/stores`, `:check`, `:default`, `:reconcile`), described in OpenAPI and served at `/v1/openapi.yaml`.
* The repair queue, the check of the inventory (by size, or deep), the sweep of uploads that never ended, the lease of the replica that does them; the cleanup of a replaced or deleted generation is written
  down in the transaction that changes the row, so a process killed at any step leaves nothing that nobody knows of.
* A refusal with 507 `store_full` and words, before the write when the store says how much room it has and when the write fails for want of room when it does not.
* Sealing of the blocks of a store on the fly (AES-256-GCM, keys by id, rotation).
* Backup of the whole rqlite to nodes of its own and restore into an empty one (`blobgate backup`, `restore`, `verify`, `CINIM_STORAGE_RESTORE_ON_EMPTY`), also from an older backup.
* `blobgate validate` (what a node can do), `blobgate token`, Prometheus metrics, structured logs.
* The image (static, user 65532, a read-only root, linux/amd64 and linux/arm64), the Helm chart, the documents (README, reference, how-to, explanation, the speed against the Nim module).

### Known limits
See `docs/status.md`: a read holds a block in memory (75 MB peak measured against 47 MB of the Nim module), nodes were tested with SFTPGo, Garage and local directories, the case of names on a server that folds
case is not handled, the chart makes no bundled Garage or node.

[0.3.0]: https://github.com/zystem/blobgate/releases/tag/v0.3.0
[0.2.0]: https://github.com/zystem/blobgate/releases/tag/v0.2.0
[0.1.0]: https://github.com/zystem/blobgate/releases/tag/v0.1.0
