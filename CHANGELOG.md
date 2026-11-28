# Changelog

All notable changes. The format is that of [Keep a Changelog](https://keepachangelog.com), the versions are [Semantic Versions](https://semver.org). The API of the module is the one of
`internal/api/openapi.yaml`; a change of it that breaks a caller is a change of the major version (of the minor one while the major is 0).

## [Unreleased]

### Security
* **Sending a part again can no longer damage what was acknowledged.** Every attempt of a part wrote the same file on a node: a retry that failed removed the block of the part that had been accepted (the completion then answered `200` and the object could not be read), and in a sealed store a retry that came late replaced the block with other bytes. Every attempt now has a file of its own, which is never written twice; a failed attempt removes only its own, and the files of the attempts that an object does not use are removed after its completion. **All releases up to 0.13.1 have this.**

### Changed
* **Not compatible with earlier releases** (the layout of a store is now 2): the name of a block of a part carries the attempt. Stop every replica of an older version before this one writes; an older program cannot read objects written in parts by this one. Objects written by earlier releases are read as before.

## [0.13.1] - 2026-11-28

### Security
* **An abort no longer removes the blocks of a completed upload.** `DELETE ?upload=<id>` (S3: AbortMultipartUpload) removed the directory of the upload without asking whether it was still one: after a completion those blocks are the object's, so the object was lost on every node (with `dedup`, every object that shares them). An abort at the same moment as the completion could acknowledge the completion and remove its blocks. An abort now claims the upload first, in one transaction, and removes blocks only when it got it. **All releases up to 0.13.0 have this.**
* A sweep that died between claiming an expired upload and writing down the removal left blocks that nothing remembered; the removal is now written down in the transaction of the claim.

## [0.13.0] - 2026-11-27

### Security
* **An error no longer tells a caller about the inside.** The detail of a failure of the database or of a node carried its text: hosts, paths, node ids, the address of rqlite; `/readyz`, which asks for no token, did too. A caller now gets what kind of failure it was and `(ref <id>)`; the log line with that `ref` has the whole text, with the passwords and tokens of URLs taken out. A `507 store_full` no longer names the nodes.
* **S3: one answer for a wrong signature and for an access key that is no tenant** (`403 SignatureDoesNotMatch`, and the same work is done for both). `InvalidAccessKeyId` is not answered any more.

### Added
* `BLOBGATE_HELD_BLOCKS_PER_TENANT` (chart `config.heldBlocksPerTenant`). With more than one tenant, one of them now takes at most three quarters of the places by default.
* Metrics `blobgate_held_blocks`, `blobgate_held_wait_seconds`, `blobgate_staged_bytes`; `blobgate_refused_total` counts `busy`.
* The report of a reconcile has `strays_total`.

### Changed
* **Quotas are hard limits.** They are decided by the transaction that records the object or the part, so writers at once cannot pass them together; the parts of uploads that have not ended count (a part over the limit is `507` when it is sent, and no longer only the completion), and so does an object that comes back from the trash. The table `storage_guard` gains a column `quota`.
* A cell of the database that is not what its column is (a text where a number belongs) is now an error (`503`), not a silent zero.
* A reconcile asks the database about 500 directories at a time and no longer keeps the listing of the nodes and the whole inventory in memory; its report lists at most 1000 strays; `apply` still queues all that are old enough. New indexes by generation are made at start.
* Integers from rqlite are no longer read through a floating-point number, which loses the last digits above 2^53.
* The documents say that the secrets must be random text: there is no password stretching.

## [0.12.0] - 2026-11-26

From this release on only the latest release is supported ([SECURITY.md](SECURITY.md)): the earlier releases, 0.11 and 0.10 among them, are no longer supported and the fixes below are not backported.

### Security
* **An expired upload and its completion can no longer both win.** The sweep removed the blocks of an upload before it deleted the row, while a completion could publish the same generation: an object could be left whose blocks were deleted from every node. The sweep now claims the upload (deletes the row, in a transaction, only if still old enough), and a commit requires the row in its own transaction; of the two exactly one wins and a completion that lost is `404 no_such_upload`. A removal queued by the reconcile asks again who owns the directory when it is done. A commit that answered with a database error no longer removes the blocks unless the database says the generation is not the object's.
* **S3: what the signature does not cover is not believed.** A header that decides what a call does (the source of a copy, the metadata directive, metadata, checksums, `Content-MD5`) that is not in `SignedHeaders` is `403 AccessDenied`; the XML bodies of DeleteObjects and CompleteMultipartUpload are checked against the signed digest (before, a body could be changed in flight and the delete was done); declared checksums (`x-amz-checksum-*`, in a header or a trailer) are checked, and one of an unknown algorithm is refused; the trailer of a signed stream is checked. A chunk of a streamed body is no longer held whole (up to 16 MiB) before it is checked.
* **`If-Match` and `If-None-Match` are decided by the transaction that writes.** Two writers under the same condition could both pass and the second overwrote the first; now one is `412`. A weak tag (`W/"..."`) no longer matches `If-Match`. A completion of an upload honours them too.
* **Encryption keys are checked.** A key id is written one way (`1`, not `01`); an update that would drop a key that blocks are sealed with, or change the text of a key under its id, is refused; two spellings of one id with different texts no longer leave the choice of the key to the order of a map.
* **Repair tasks are settled by revision, and a copy that cannot be written down refuses the write.** A task replaced in the same second could be closed by the old one; an error of the queue was ignored while the write was acknowledged with fewer copies. New metric `blobgate_repair_enqueue_errors_total`.

### Changed
* **Slow readers no longer keep writers out of the places of `BLOBGATE_HELD_BLOCKS`.** Measured in a cluster with 0.11.6 (docs/benchmark.md): 40 slow readers that came first took all 32 places, and every writer and every part after them was told `503 busy` for as long as the readers lasted. A kind of call (reads; writes of many blocks and parts through the S3 door) now takes at most three quarters of the places (24 of 32); the last quarter is left for the other kind, and the total is still the limit of memory (32 blocks, 256 MiB). The price: with no writers about, slow readers alone can hold 24 blocks and not 32, so the 25th reader is told `503 busy` a little sooner; raise `BLOBGATE_HELD_BLOCKS` if that matters (and the memory limit with it). Test: `TestSlowReadersDoNotTakeThePlacesOfWriters`.

## [0.11.6] - 2026-11-21

### Fixed
* **The status page finds a local node whose directory cannot be read.** The local backend of rclone logs such a directory and lists it as empty, so the probe called the node `ok` (shown with mode 000). The probe now reads the directory of a local node itself; it is `dead` after three probes. Network backends were found before.
* **A part of a multipart upload through the S3 door is held under the places of `BLOBGATE_HELD_BLOCKS` too.** The part is read whole into memory, and 24 clients that sent 6 MiB of a part of 8 and stopped held 200 MiB with no bound (the same test with the places: 33 MiB with 4 places, the others told `503 SlowDown` with `Retry-After`). Found while measuring the limit of 0.11.5 in a cluster, which covered reads and writes of one object but not the parts.
* **A part whose client leaves, or is ended for being quiet, is answered `400 IncompleteBody`.** The handler answered with status 0 and `net/http` stopped it with a panic (`invalid WriteHeader code 0`, the connection was closed and a line was logged).

## [0.11.5] - 2026-11-20

### Added
* `BLOBGATE_HELD_BLOCKS` (default 32) and `BLOBGATE_CLIENT_IDLE_SECONDS` (default 120), in the chart `config.heldBlocks` and `config.clientIdleSeconds`; the new answer `503 busy` with `Retry-After`.

### Fixed
* **Clients that go slow or quiet no longer hold memory and places without end.** Measured with 48 clients that took one byte of a big object and stopped: the module held 385 MiB (a block of 8 MiB each), and the same for 48 writers of no known length that sent 3 MiB and stopped; the limit `BLOBGATE_THREADS` that the documents named for it bounded only the reads from the nodes (the place was freed before the block was sent). With the default memory limit of the chart (512 MiB) some 60 slow readers would have ended the process. Now the blocks held for clients are bounded by `BLOBGATE_HELD_BLOCKS` (the same test: 32 MiB with 4 places), a call that finds no place is told `503 busy`, and a client with no byte moving for `BLOBGATE_CLIENT_IDLE_SECONDS` is ended, which also frees the places of `BLOBGATE_THREADS` that quiet writers into a store that seals held for ever (four quiet writers stopped every such write).
* Tests of a client that leaves in the middle of an object (no slot or goroutine is left), one that stalls (it does not stop the others even with one read at a time), and a node that gives out after the first blocks went (the reader meets a cut, never a short answer that looks whole).

## [0.11.4] - 2026-11-20

### Fixed
* **A read that meets a damaged copy repairs it.** A node that gives back a block with the wrong bytes or the wrong length was already passed over (the client never gets wrong bytes: tested with a flipped byte, zeros, a block cut short or in half, longer, and empty, in a plain and a sealed store, on either node, and with both nodes damaged the read is refused with 5xx), but the damaged copy stayed until the deep check (`BLOBGATE_DEEP_CHECK`, off by default) found it, and with the other copy the only good one a second damage would have lost the block. The read now queues a repair for the node it found damaged (the metric `blobgate_node_errors_total` has `op="damaged"`). A damaged copy on a node that the reads do not reach (the first node answers) is still found by the deep check only.
* **A read that meets a node with no copy of a block repairs that too.** A node that lost a disk, or a directory, was put right only by the check of the inventory; a read that is answered by another node now queues the copy for it as well. Copies (`CopyObject` of the bytes, `UploadPartCopy`) and objects with `dedup` take the same path and were tested with a damaged node: they never carry its bytes.

## [0.11.3] - 2026-11-20

### Fixed
* **A node that stops in the middle of a block no longer takes the healthy nodes with it.** The bytes of a write went to the nodes one after the other, so a node whose connection was open and silent held up the next node's bytes until its own timeout, and the healthy node was cut by its own idle timeout at the same moment: `0 of 2 nodes took the block` with one node well (shown with a link that stops, `TestRealSlowAndStalledLink`). Every node now has a short queue (2 MiB) of its own; a node that takes nothing for half of `BLOBGATE_NODE_TIMEOUT_SECONDS` is left behind and the write goes on with the others (the copy for it is queued).
* **A dead node is met once, not for every block.** Every block of every write, and every read that began with it, paid the whole timeout of the dead node again (a write of 40 MiB took 87 seconds in the test, one of 100 MiB would take many minutes with the defaults). A node that fails a write or a read is now left for 10 seconds (the others are tried first; a write does not wait for it, and the copy is queued); the test above takes 23 seconds. During those 10 seconds a write that needs every node (`min_acks` equal to the number of nodes) is refused. Slow links: the idle timeout is measured from the moment the block is in the buffers of the connection, so a link slower than about 150 KB/s needs a larger `BLOBGATE_NODE_TIMEOUT_SECONDS` (docs/configuration.md); a link that is slow but moving is not cut (tested at 2 MiB/s with a timeout of 5 seconds).

## [0.11.2] - 2026-11-20

### Fixed
* **A store whose node is down or silent when it is loaded still serves.** The SFTP backend of rclone connects when it is made, so a replica that loaded a store (at start, or after a change of its settings) while one SFTP node was away or silent could not open the store at all: every call answered `502`, though `min_acks` and the other nodes were enough. Now such a node is put in as one that fails every call (the others serve and the copies for it are queued), and the nodes are opened again every 10 seconds, in the background, until it is back. A store that cannot reach `min_acks` nodes at load is still refused. Found with a frozen SFTPGo (`docker pause`: nothing answered, nothing closed), which also showed that WebDAV and FTP nodes in that state are given up by the idle timeout of 0.11.1 (`TestRealSilentNodes`).

## [0.11.1] - 2026-11-19

### Fixed
* **A node that dies with its connections open no longer holds a write for minutes.** A server that vanishes with no reset (a power cut, a cable pulled; shown with an S3 server stopped under open connections) answers nothing, and rclone waits five minutes for a byte and tries ten times: a part of an upload, or a write in one call, did not end for many minutes (a test of 9 minutes had not seen the refusal yet). The calls to nodes now give up after `BLOBGATE_NODE_TIMEOUT_SECONDS` (default 30) with no byte moving and are tried `BLOBGATE_NODE_RETRIES` times (default 3), so the write is refused with `503`, or, in a mirror with `min_acks` met by the other nodes, goes on and the copy for the dead node is queued. Found with SeaweedFS stopped in the middle of uploads in parts (`s3node_test.go`).

## [0.11.0] - 2026-11-19

### Changed
* **The ETag of an object made of parts is the one S3 makes**: the MD5 of the MD5s of its parts and `-<number of parts>`. The module now keeps the MD5 of every part (a column `md5` of the table of parts, added at start) and makes the ETag when the upload is completed. A tool that compares the ETag of a multipart object with its own calculation (the AWS CLI, `rclone`, a backup tool) now finds them equal. Objects that are written already keep the older name, and an upload that was going on at the upgrade is completed with it (its parts have no MD5); the ETag of an object does not change after it is written. See docs/s3-reference.md.
* The status page: on a narrow screen (a phone) a node of the details is a small card of labels and values, so that nothing has to be scrolled sideways; the two columns of the public view line up between the stores; the details table is wider on a big screen (checked in the light and the dark theme).

## [0.10.0] - 2026-11-18

### Added
* **ListMultipartUploads** in the S3 door (`aws s3api list-multipart-uploads`, `rclone backend cleanup`, the cleaning of tools that find uploads nobody finished): the uploads in parts of the tenant, by key and upload id, with `prefix`, `delimiter`, markers and `max-uploads`. The table of uploads gains a column (`one_call`, added at start) that tells a write in one call, which is not listed, from an upload in parts (during a rolling upgrade, a write in one call that an older replica makes may show in the list for the seconds it lasts).

### Fixed
* S3 door: **the names of the metadata go out in lower case** (`x-amz-meta-owner`), as S3 sends them: boto3 gave back `Owner` for what was written as `owner`. **A listing of nothing has `KeyCount` 0** (v2): the Java SDK read the missing one as null and failed. Found with the SDKs of Python, JavaScript and Java (`tools/sdk-check/`, docs/s3.md).

### Changed
* Tests only: the S3 door, deduplication, copies by reference and by ranges, and the status page are tested with real servers (SFTPGo's WebDAV, FTP and SFTP, Garage, SeaweedFS, and MinIO where its image is there), in CI with `BLOBGATE_DOCKER_TESTS=1`. No fault was found in the program.

## [0.9.0] - 2026-11-18

### Added
* **UploadPartCopy** in the S3 door (a part of an upload that is a range of an object): `aws s3 cp` and `mv` between keys now work for objects above the CLI's multipart threshold with no setting. The conditions on the source of a copy (`x-amz-copy-source-if-match`, `-if-none-match`, `-if-modified-since`, `-if-unmodified-since`) are checked in CopyObject and UploadPartCopy (they were `501`).

## [0.8.0] - 2026-11-18

### Added
* **A status page** (`/status`, `/status.json` open; `/v1/admin/status` with the admin token) on the main port: every node of every store is `ok`, `lagging`, `dead`, `full`, `slow` or `unknown`, and every store green, yellow or red. The open view has no store name, id, error or figure; a node is named by its optional `label` (`"label"` of a node when a store is registered) or `backend-position`. Every node is probed every `BLOBGATE_PROBE_SECONDS` (10). See docs/status-page.md.

## [0.7.0] - 2026-11-17

### Added
* **CopyObject in the S3 door** (`aws s3 cp s3://a s3://b`, `mv`, `rclone` server-side copy): in one store of sets (`dedup` on) the copy takes a reference on the set, in one transaction, with no byte read or written; any other copy reads the source and writes the destination through a pipe. `x-amz-metadata-directive` `COPY` or `REPLACE`. See docs/s3-reference.md.

### Changed
* The S3 door has always been served over TLS when `BLOBGATE_TLS_CERT_FILE` is set (it is one of the ports); this is now tested (a signed SDK call over https, plain http refused) and written in docs/configuration.md. Nothing in the program changed.

## [0.6.1] - 2026-11-16

### Changed
* **`BLOBGATE_THREADS` is 4 by default** (was 8): at most four blocks are held in memory at once by reads and sealed writes (up to 32 MiB instead of 64 MiB); more reads at one time wait for a place. Set `BLOBGATE_THREADS=8` for the old value.
* `golang.org/x/crypto` v0.59.0 (fixes GO-2026-5932, which the program did not call); the VEX document (`vex/blobgate.openvex.json`) is now empty. A `main` that is protected for everyone and a VEX document are new in the repository.

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
