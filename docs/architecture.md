# Architecture

The high-level design. For what happens to a block and what a crash leaves, see [data-lifecycle.md](data-lifecycle.md); for the API, [../internal/api/openapi.yaml](../internal/api/openapi.yaml).

## The parts

```
 CiNim core ──calls (token)──▶ ┌────────────────────┐ ──rclone──▶ node 1
 step shims ──grants─────────▶ │      blobgate      │ ──rclone──▶ node 2     every block on every node of its store
 operator ──admin token─────▶  │  (replicas, stateless) │ ──rclone──▶ node n
                                └─────────┬──────────┘
                                          │ records, lease
                                          ▼
                                       rqlite
```

blobgate is a stateless service: any number of replicas, one rqlite, nodes that rclone can talk to. It writes nothing to a disk. One replica at a time (the one that holds the lease in rqlite) does the background work.

## The packages

| Package | What it does |
|---|---|
| `cmd/blobgate` | the program: the service, `validate`, `token`, `backup`, `restore`, `version` |
| `internal/api` | the HTTP side: the core's calls, the port for shims, the admin API, metrics, the log |
| `internal/store` | the stores: settings, the nodes opened through rclone, the mirror (write to all, read the first block that checks out), sealing of blocks, free space, the check of a node |
| `internal/maint` | what one replica does: the repair queue, the check of the inventory, the sweep of uploads that never ended, `reconcile`, the lease |
| `internal/backup` | backup of the whole rqlite to nodes of its own, and restore into an empty one |
| `internal/auth`, `internal/seal`, `internal/envfile`, `internal/tlsutil` | the tokens and grants, the sealing of secrets, secrets as files, the TLS configuration |
| `internal/layout` | how a key becomes a path on a node, and the keys of the repair queue |
| `internal/rq`, `internal/db`, `internal/space`, `internal/metrics` | the rqlite client, the tables, the words for a full store, the metrics |

## The data model

An **object** is a row of `storage_objects` (the store, the key with the tenant in front, the size, the generation, the size of a block, the SHA-256 of every block as stored, the key of sealing) and the **blocks** it names:
files `<escaped key>~<generation>/<n>` on every node of the store. The row is the commit. `storage_uploads` and `storage_parts` hold what an upload that has begun has written; `storage_repair` holds what a node has to be
given or to lose; `storage_lease` holds who does the background work; `storage_stores` holds the stores (their secrets sealed).

## A write, a read

A write of a block reads the body once, writes it to all nodes at the same time without holding it (a sealed store holds one block to seal it), and compares the digest with the one the writer named. The object is fixed by one
transaction that also writes down the removal of what it replaces. A read takes the row, reads each block from the first node that has it with the digest the row names, and sends the bytes asked for.

## Trust boundaries

The core and the operator are trusted (each with its own secret). A step's shim is semi-trusted (it holds a grant for one upload or one object). The nodes are not trusted with integrity (every block is checked) and, with
sealing, not with confidentiality. rqlite is trusted with the records and with nothing secret in the clear. See [threat-model.md](threat-model.md).
