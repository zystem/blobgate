# Back up rqlite and restore it into an empty one

How-to page. The reasons are at the end; the settings are in the table.

The module can keep copies of the whole rqlite database (the module's own tables and the core's) on nodes of its own choice, and put the newest one back into an empty rqlite. The backups do not depend on
the database: the nodes and the key are in the environment, and a backup is read from the nodes alone.

## Settings (environment; a change needs a restart)

| Variable | Default | Meaning |
|---|---|---|
| `CINIM_STORAGE_BACKUP_NODES` | none: no backups | JSON list of nodes: `[{"remote": ":sftp,host=h,user=u:/backups", "secrets": {"pass": "..."}}, ...]`. The same node as in a store (`:check` and `blobgate validate` work on it). One node is a backup with no copy: use two. |
| `CINIM_STORAGE_BACKUP_KEY` | none: refused | At least 16 characters; the sealing key is made of it. **Keep it apart from the database and from the nodes** (a backup holds every secret of the database). |
| `CINIM_STORAGE_BACKUP_PLAINTEXT` | `0` | `1`: no sealing. Not advised. |
| `CINIM_STORAGE_BACKUP_KEEP` | `7` | Backups kept; older ones are removed after each new one. |
| `CINIM_STORAGE_BACKUP_MIN_ACKS` | `0` (every node) | Nodes that must take every block. |
| `CINIM_STORAGE_BACKUP_SECONDS` | `0` (the module does not back up by itself) | Back up every so many seconds; one replica at a time (the lease `backup`). Dangerous when too short: every backup reads the whole database. |
| `CINIM_STORAGE_RESTORE_FALLBACK` | `0` | `1`: with `CINIM_STORAGE_RESTORE_ON_EMPTY`, when the newest backup does not check out take the newest that does (the state is then older). |
| `CINIM_STORAGE_RESTORE_ON_EMPTY` | `0` | `1`: at start, a database with no table at all is filled from the newest backup before the module makes its own tables. |

## Take a backup

```bash
blobgate backup
```

Prints (2026-11-12, a database of 64 KiB, two local nodes):

```
20261112T215317Z-cad17e  2026-11-12T21:53:17Z  65536 bytes in 1 block(s)  sealed=true
```

`blobgate backups` lists the ones on the nodes, newest first, and `blobgate verify` reads the newest whole (every block's digest, the opening, the decompression) and changes nothing. Run `verify` now and then:
a backup that has not been read back is a hope.

## Restore into an empty database

Run it **before the module starts**: the module makes its tables when it starts, and a database with tables is no longer empty.

```bash
blobgate restore
```

Prints `restored  <id>  <time>  <bytes> bytes in <n> block(s)  sealed=true`. The backup is read and checked whole first; one that cannot be read changes nothing. A database that is not empty is refused:

```
restore: the database is not empty: a restore replaces all of it; run it with force when that is meant
```

`-force` replaces all of it; `-id <id>` takes another backup than the newest.

With `CINIM_STORAGE_RESTORE_ON_EMPTY=1` the module does this itself at start (and starts empty, with a line in the log, when there is no backup at all; it stops when there is one that cannot be read, rather
than make new tables over what it would have restored).

## The newest backup is damaged

`blobgate restore` reads and checks the whole backup before it touches the database, so a damaged one changes nothing, and the error names the older ones:

```
restore: the backup does not check out, the database is untouched: 20261112T215317Z-cad17e: block 0: ...; older backups: 20261111T215317Z-91ab03 (restore one with -id, or take the newest that checks out with -fallback; `blobgate verify -all` says which can be read)
```

* `blobgate verify -all` reads every backup and says which can be read (`ok` or `BAD` with the reason).
* `blobgate restore -id 20261111T215317Z-91ab03` takes the one you choose.
* `blobgate restore -fallback` takes the newest that checks out. The database is then **older** than the newest backup says, by as much as the backups that were skipped; look at the line it prints.
* `CINIM_STORAGE_RESTORE_FALLBACK=1` does the same for `CINIM_STORAGE_RESTORE_ON_EMPTY=1` at start. Default `0`: a module that cannot read the newest backup stops, because starting from an older state
  is a decision for a person, not for a restart loop.

## What was run (2026-11-12) and what was not

Run: the commands above against a real rqlite and two `local` nodes; the start of the module on a wiped database with `CINIM_STORAGE_RESTORE_ON_EMPTY=1` (the row written before the wipe was there
afterwards); in the tests (`internal/backup`, with Docker): a database of 14 MiB of random data in several blocks, restored and compared by digest; a block damaged on one node (read from the other) and on both
(the restore stops, the database is untouched); a wrong key; sealed read as plain and the opposite; the removal of old backups and of a backup that was cut off; no backup at all.

Not run: nodes of FTP, WebDAV or SFTP for the backups (they are the same code as the stores', which were); a database of gigabytes; a rqlite of several nodes (the load goes to the address in
`CINIM_RQLITE_URL`; it must reach the leader); the repair of a node that missed a block of a backup (a backup that two nodes took and one lost is read from the other; nothing copies it back yet).

## Why it is made like this

The module keeps its stores in the database, so a backup of the database cannot be kept in a store it describes: with the database empty, the module knows no store. The nodes are therefore in the
environment, and the file that says what a backup is (the manifest) lies next to its blocks and is written last: no manifest, no backup. The listing of the nodes that this needs is the one place the module
lists a node as a part of its work. The backup is the SQLite file that rqlite gives for itself (`GET /db/backup`, one moment of the database), compressed, in blocks of 8 MiB, each sealed with AES-256-GCM
with its number and the id of the backup as additional data, so that a block moved to another place or another backup does not open.
