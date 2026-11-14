# Configuration reference

Reference page. Every setting is an environment variable; a change needs a restart of the program. "Required" means the program stops at start without it.

## Names of the settings

The settings are `BLOBGATE_<NAME>`. The program was first the storage module of CiNim, whose settings were `CINIM_STORAGE_<NAME>` (`CINIM_RQLITE_URL`, `CINIM_SHARD` and `CINIM_STORAGE_SHARDS` for what are now
`BLOBGATE_RQLITE_URL`, `BLOBGATE_TENANT` and `BLOBGATE_TENANTS`); **the old names still work** (the file forms too: `CINIM_STORAGE_ADMIN_TOKEN_FILE`), and the new one wins when both are set. Nothing has to be renamed on the
day of an update. A *tenant* is what the first version called a *shard*: the header of a call is `X-Blobgate-Tenant` (`X-Cinim-Shard` still works), and the tokens and grants of the first version (`v1`, `g1`) are still
accepted; `blobgate token` makes `v2`.

## Service

| Variable | Default | Meaning |
|---|---|---|
| `BLOBGATE_RQLITE_URL` | required | The rqlite that holds the records (`http://host:4001`). The program waits for it up to two minutes at start. |
| `BLOBGATE_TENANTS` | required, or `BLOBGATE_TOKEN` | `001=<secret>;002=<secret>`: the tenants served and the secret each signs its calls with (at least 16 characters). A tenant is 1..64 of lowercase letters, digits, `-` and `_` (a name as `acme`, or a number as `001`). |
| `BLOBGATE_TOKEN`, `BLOBGATE_TENANT` | none; tenant `default` (`001` when only the old name `CINIM_STORAGE_TOKEN` is set) | One tenant in short: the secret and the tenant. Used only when `BLOBGATE_TENANTS` is empty. |
| `BLOBGATE_ADMIN_TOKEN` | required | The token of the admin API and `/metrics` (at least 16 characters). |
| `BLOBGATE_SEAL_KEY` | required | The key the secrets of the stores are sealed under in the database (at least 16 characters). **Keep it apart from the database**; losing it makes the stored secrets unreadable. |
| `BLOBGATE_TLS_CERT_FILE`, `BLOBGATE_TLS_KEY_FILE` | none: plain HTTP | Both set: every port (the main one, the one for shims, the metrics one) serves TLS 1.2 or later with this certificate and key (PEM); the files are read again when they change, so a renewed certificate needs no restart. Either one alone stops the program. **Dangerous** to leave off when the port can be reached from outside the cluster: the tokens are signed per call but the bodies are not hidden. |
| `BLOBGATE_PORT` | `18090` | The port of the core's calls, the admin API, `/healthz`, `/readyz`, `/metrics`, `/v1/openapi.yaml`. |
| `BLOBGATE_SHIM_PORT` | `18091` | The port for step shims (plain HTTP, grants only; socket timeouts 30 s). `0`: none. |
| `BLOBGATE_METRICS_PORT` | `0` (off) | A port that serves `/metrics` with no token, for a scraper inside the cluster. **Dangerous** when it can be reached from outside. |
| `BLOBGATE_THREADS` | `8` | Reads and sealed writes of a block at once (each holds a block, up to 8 MiB, in memory). |
| `BLOBGATE_FULL_HINT` | a general sentence | What a refusal for want of room tells the person to do, after the figures. A product that uses the module puts its own words here (the name of its retention settings). |
| `BLOBGATE_MAX_OBJECT_BYTES` | `0` (no limit besides the room) | The most bytes one object may hold; a larger write is refused with 413 `too_large` (at once when its `Content-Length` says so, else when the body passes it). |
| `BLOBGATE_RESERVE_BYTES` | `268435456` (256 MiB) | Room kept free in a store: a write that would leave less is refused with 507 `store_full`. `0` is the default, not "none". |
| `GOGC` | `25` | The garbage collector of the Go runtime; the program sets 25 unless it is set. |

### Secrets as files

Every setting that holds a secret can be given as a file instead of a value: `BLOBGATE_TENANTS_FILE`, `BLOBGATE_TOKEN_FILE`, `BLOBGATE_ADMIN_TOKEN_FILE`, `BLOBGATE_SEAL_KEY_FILE`,
`BLOBGATE_BACKUP_NODES_FILE` and `BLOBGATE_BACKUP_KEY_FILE` hold the path of a file whose contents are the value (one trailing newline is dropped). A Kubernetes Secret or a Docker secret mounted as a file
then never appears in the environment of the process. The file must not be readable by everyone (mode 0600 or 0400), or the program stops with words; a value and its file both set also stop it. A secret is
replaced by changing the file and restarting; nothing is rebuilt.

## Repair and cleaning (one replica at a time, under a lease)

| Variable | Default | Meaning |
|---|---|---|
| `BLOBGATE_REPAIR_SECONDS` | `60` | How often the repair queue and the sweep are worked. |
| `BLOBGATE_REPAIR_RETRY_SECONDS` | `30` | The first pause after a repair that failed; it doubles up to an hour. |
| `BLOBGATE_CHECK_SECONDS` | `600` | How often a slice (200 objects) of the inventory is checked against the nodes. |
| `BLOBGATE_DEEP_CHECK` | `0` | `1`: the check reads the blocks and compares their SHA-256, not only their size. Reads a lot. |
| `BLOBGATE_DEEP_CHECK_BYTES_PER_SECOND` | `0` (no limit) | The most the deep check reads in a second. |
| `BLOBGATE_LEASE_SECONDS` | `120` | How long the replica that does the repair holds its lease without renewing it. When it dies, another takes over after this time. Shorter is quicker to take over and more sensitive to a pause of the database. It must be longer than `BLOBGATE_REPAIR_SECONDS`. |
| `BLOBGATE_UPLOAD_TTL_SECONDS` | `86400` | An upload that has not ended after this is swept: its blocks are removed. |

## Backups of the database (docs/backup.md)

`BLOBGATE_BACKUP_NODES`, `BLOBGATE_BACKUP_KEY`, `BLOBGATE_BACKUP_PLAINTEXT`, `BLOBGATE_BACKUP_KEEP`, `BLOBGATE_BACKUP_MIN_ACKS`, `BLOBGATE_BACKUP_SECONDS`,
`BLOBGATE_RESTORE_ON_EMPTY`, `BLOBGATE_RESTORE_FALLBACK`: see the table in [backup.md](backup.md), with defaults and what is dangerous.

## Commands

`blobgate` with no arguments is the service. The others read the same environment and print what they did.

| Command | What it does |
|---|---|
| `blobgate validate [-json] [-secret option=value]... '<connection string>'` | Asks one node for what the module needs of it and says what it cannot do. Needs no database. Exit 1: the node cannot be used. |
| `blobgate token <tenant> <secret> <METHOD> <target>` | Prints the token of a call of the core, for curl. |
| `blobgate backup` | Takes a backup of rqlite now (docs/backup.md). |
| `blobgate backups [-json]` | Lists the backups on the nodes, newest first. |
| `blobgate verify [-id ID \| -all]` | Reads a backup (or every one) whole and checks it; changes nothing. |
| `blobgate restore [-id ID] [-fallback] [-force]` | Puts a backup into an empty rqlite (with `-force`, into one that is not). |
| `blobgate version` | The version of the program (`dev` for a build that is not a release). |
| `blobgate help` | The usage of all of them. |

## Admin calls (`Authorization: Bearer $BLOBGATE_ADMIN_TOKEN`)

`GET /v1/admin/stores`; `PUT /v1/admin/stores/{name}` (try, then keep; `POST` does the same); `DELETE /v1/admin/stores/{name}[?force=1]`; `POST /v1/admin/stores/{name}:check`, `:default`,
`:reconcile[?apply&min_age=<seconds>]`. The body and the answers are in `internal/api/openapi.yaml`.

A store is `{"kind": "mirror", "nodes": [{"remote": ":ftp,host=h,user=u:/path", "secrets": {"pass": "..."}}], "min_acks": 0, "encryption": {...}}` or, for one S3 bucket,
`{"kind": "s3", "endpoint", "region", "bucket", "access_key_id", "secret_access_key", "space_url"}`.

* `min_acks`: nodes that must confirm a write; `0` (the default) is every node. A node that missed it gets the block from the repair pass.
* `secrets` of a node: options of the rclone backend that are passwords or keys; they are sealed in the database and never shown.
* `dedup`: `""` (the default, off), `"tenant"` or `"store"`: objects with the same bytes share one set of blocks, inside one tenant or the whole store (docs/deduplication.md). Read by a replica within two seconds; what is written stays as it is.
* `soft_delete_seconds`: `0` (the default) deletes at once. Above `0` (at most one year) a deleted object goes to the trash: it is hidden from reads and from the list, its blocks stay, and `POST .../objects/<key>?restore`
  brings it back as it was; a new write of the key replaces it; `DELETE ...?purge` removes it at once; the worker removes it when the time is over. A change is read by the replicas within two seconds.
* Quotas: `PUT /v1/admin/quotas/<store>/<tenant>` with `{"max_bytes": n, "max_objects": n}` (`0`, the default, is no limit; applied at once, by every replica). A write that would pass the limit is
  refused with 507 `quota_exceeded` before anything is written (a body of unknown length: when it has come, and nothing is left); a replacement counts once; the trash does not count; what is held when the limit is set is not touched.
  `GET /v1/stores/<store>/usage` (the tenant) and `GET /v1/admin/stores/<name>:usage` (the operator) say what is held.
* Listing: `GET /v1/stores/<store>/objects?prefix=&limit=&after=&delimiter=&trash=` (the keys of the tenant only; see the API description).
* `space_url` (s3): where the store says how much room it has (Garage: `http://host:3903/metrics`). A store that does not say is not asked, and a full one is found when a write fails (507 with words).
* The WebDAV backend of rclone is given `pacer_min_sleep=100us` unless the store sets it.
