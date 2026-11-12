# Changelog

All notable changes. The format is that of [Keep a Changelog](https://keepachangelog.com), the versions are [Semantic Versions](https://semver.org). The API of the module is the one of
`internal/api/openapi.yaml`; a change of it that breaks a caller is a change of the major version (of the minor one while the major is 0).

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

[0.1.0]: https://github.com/zystem/blobgate/releases/tag/v0.1.0
