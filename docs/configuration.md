# Configuration reference

Reference page. Every setting is an environment variable; a change needs a restart of the program. "Required" means the program stops at start without it.

## Service

| Variable | Default | Meaning |
|---|---|---|
| `CINIM_RQLITE_URL` | required | The rqlite that holds the records (`http://host:4001`). The program waits for it up to two minutes at start. |
| `CINIM_STORAGE_SHARDS` | required, or `CINIM_STORAGE_TOKEN` | `001=<secret>;002=<secret>`: the shards served and the secret each signs its calls with (at least 16 characters). A shard is digits only. |
| `CINIM_STORAGE_TOKEN`, `CINIM_SHARD` | none; shard `001` | One shard in short: the secret and the shard. Used only when `CINIM_STORAGE_SHARDS` is empty. |
| `CINIM_STORAGE_ADMIN_TOKEN` | required | The token of the admin API and `/metrics` (at least 16 characters). |
| `CINIM_STORAGE_SEAL_KEY` | required | The key the secrets of the stores are sealed under in the database (at least 16 characters). **Keep it apart from the database**; losing it makes the stored secrets unreadable. |
| `CINIM_STORAGE_PORT` | `18090` | The port of the core's calls, the admin API, `/healthz`, `/readyz`, `/metrics`, `/v1/openapi.yaml`. |
| `CINIM_STORAGE_SHIM_PORT` | `18091` | The port for step shims (plain HTTP, grants only; socket timeouts 30 s). `0`: none. |
| `CINIM_STORAGE_METRICS_PORT` | `0` (off) | A port that serves `/metrics` with no token, for a scraper inside the cluster. **Dangerous** when it can be reached from outside. |
| `CINIM_STORAGE_THREADS` | `8` | Reads and sealed writes of a block at once (each holds a block, up to 8 MiB, in memory). |
| `CINIM_STORAGE_RESERVE_BYTES` | `268435456` (256 MiB) | Room kept free in a store: a write that would leave less is refused with 507 `store_full`. `0` is the default, not "none". |
| `GOGC` | `25` | The garbage collector of the Go runtime; the program sets 25 unless it is set. |

## Repair and cleaning (one replica at a time, under a lease)

| Variable | Default | Meaning |
|---|---|---|
| `CINIM_STORAGE_REPAIR_SECONDS` | `60` | How often the repair queue and the sweep are worked. |
| `CINIM_STORAGE_REPAIR_RETRY_SECONDS` | `30` | The first pause after a repair that failed; it doubles up to an hour. |
| `CINIM_STORAGE_CHECK_SECONDS` | `600` | How often a slice (200 objects) of the inventory is checked against the nodes. |
| `CINIM_STORAGE_DEEP_CHECK` | `0` | `1`: the check reads the blocks and compares their SHA-256, not only their size. Reads a lot. |
| `CINIM_STORAGE_DEEP_CHECK_BYTES_PER_SECOND` | `0` (no limit) | The most the deep check reads in a second. |
| `CINIM_STORAGE_UPLOAD_TTL_SECONDS` | `86400` | An upload that has not ended after this is swept: its blocks are removed. |

## Backups of the database (docs/backup.md)

`CINIM_STORAGE_BACKUP_NODES`, `CINIM_STORAGE_BACKUP_KEY`, `CINIM_STORAGE_BACKUP_PLAINTEXT`, `CINIM_STORAGE_BACKUP_KEEP`, `CINIM_STORAGE_BACKUP_MIN_ACKS`, `CINIM_STORAGE_BACKUP_SECONDS`,
`CINIM_STORAGE_RESTORE_ON_EMPTY`, `CINIM_STORAGE_RESTORE_FALLBACK`: see the table in [backup.md](backup.md), with defaults and what is dangerous.

## Commands

`blobgate` with no arguments is the service. The others read the same environment and print what they did.

| Command | What it does |
|---|---|
| `blobgate validate [-json] [-secret option=value]... '<connection string>'` | Asks one node for what the module needs of it and says what it cannot do. Needs no database. Exit 1: the node cannot be used. |
| `blobgate token <shard> <secret> <METHOD> <target>` | Prints the token of a call of the core, for curl. |
| `blobgate backup` | Takes a backup of rqlite now (docs/backup.md). |
| `blobgate backups [-json]` | Lists the backups on the nodes, newest first. |
| `blobgate verify [-id ID \| -all]` | Reads a backup (or every one) whole and checks it; changes nothing. |
| `blobgate restore [-id ID] [-fallback] [-force]` | Puts a backup into an empty rqlite (with `-force`, into one that is not). |
| `blobgate help` | The usage of all of them. |

## Admin calls (`Authorization: Bearer $CINIM_STORAGE_ADMIN_TOKEN`)

`GET /v1/admin/stores`; `PUT /v1/admin/stores/{name}` (try, then keep; `POST` does the same); `DELETE /v1/admin/stores/{name}[?force=1]`; `POST /v1/admin/stores/{name}:check`, `:default`,
`:reconcile[?apply&min_age=<seconds>]`. The body and the answers are in `internal/api/openapi.yaml`.

A store is `{"kind": "mirror", "nodes": [{"remote": ":ftp,host=h,user=u:/path", "secrets": {"pass": "..."}}], "min_acks": 0, "encryption": {...}}` or, for one S3 bucket,
`{"kind": "s3", "endpoint", "region", "bucket", "access_key_id", "secret_access_key", "space_url"}`.

* `min_acks`: nodes that must confirm a write; `0` (the default) is every node. A node that missed it gets the block from the repair pass.
* `secrets` of a node: options of the rclone backend that are passwords or keys; they are sealed in the database and never shown.
* `space_url` (s3): where the store says how much room it has (Garage: `http://host:3903/metrics`). A store that does not say is not asked, and a full one is found when a write fails (507 with words).
* The WebDAV backend of rclone is given `pacer_min_sleep=100us` unless the store sets it.
