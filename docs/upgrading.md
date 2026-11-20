# Upgrading

How-to page. Every release says in `CHANGELOG.md` what it changes; this page is the way to do an upgrade and what to check.

1. Read the changelog from your version to the new one. While the major version is 0, a minor release may change a setting, an environment variable or a stored format; the changelog says which, and what to do.
2. Take a backup of the database first ([backup.md](backup.md)): `blobgate backup`, then `blobgate verify`.
3. Upgrade one replica and see `/readyz` answer; then the others. The module migrates its tables itself at start (`CREATE TABLE IF NOT EXISTS`, and columns added when they are missing); a table is never dropped or
   rewritten by an upgrade.
4. Check `blobgate_repair_oldest_age_seconds` and `blobgate_node_errors_total` ([metrics.md](metrics.md)) for a few minutes.

What is kept across releases:

* the rows of rqlite: a newer program reads what an older one wrote (the migration adds the columns); an older program reading what a newer one wrote was not tested, which is one more reason not to downgrade;
* the layout of blocks on the nodes (`store.CurrentLayout`): a store records the layout it was written with, and a program refuses a store written with a newer one (`500 newer_layout`, with the words to update);
* the format of a sealed secret (`v2.`), of a sealed block (a version byte) and of a backup (`version` in its manifest).

A downgrade is not supported: a newer release may have added what an older one does not read (restore the backup taken before the upgrade, with the older program, if a downgrade is needed).

**0.6.0** changes the `ETag` of every object once (the MD5 of the bytes for an object written in one call, `hex-N` for the others; [../CHANGELOG.md](../CHANGELOG.md)): a client that keeps ETags (a cache, a sync tool) fetches
once more. The schema gains columns at start; nothing is to be done by hand.

**0.1.0** is the first release; there is nothing to upgrade from. The Nim module that this one replaces in CiNim used other tables for objects (`storage_objects` without generations); the stores start empty, as decided
for CiNim (D-60), and nothing is read from the old layout.

**0.6.1** lowers the default of `BLOBGATE_THREADS` from 8 to 4 (at most four blocks held in memory at once by reads and sealed writes; more reads wait for a place). Set `BLOBGATE_THREADS=8` to keep the old value.

**0.7.0** adds CopyObject to the S3 door and changes nothing that is stored or set: there is nothing to do by hand.

**0.8.0** adds the status page (`/status`, `/status.json` open and `/v1/admin/status` with the admin token) on the main port, and `BLOBGATE_PROBE_SECONDS` (10): every node of every store is asked every 10 seconds whether it answers, which is one listing of its root a node every 10 seconds. The open view tells the state of each node (by its optional `label`, or `backend-position`) and of each store, and nothing else; see [status-page.md](status-page.md) and the threat model before you open the main port to a network that you do not trust. Set `BLOBGATE_PROBE_SECONDS=0` to stop the probes (every node then shows `unknown`). Nothing stored or set has to change.

**0.9.0** adds UploadPartCopy to the S3 door and checks the `x-amz-copy-source-if-*` conditions of a copy (they were `501`): `aws s3 cp` and `mv` between keys work for objects above the CLI's multipart threshold with no setting. Nothing stored or set has to change.

**0.10.0** adds ListMultipartUploads to the S3 door; the table of uploads gains a column (`one_call`) that the program adds at start, by itself. Two answers of the S3 door change to what S3 does: the names of the metadata go out in lower case (`x-amz-meta-owner`), and a listing of nothing says `KeyCount` 0. A client that read the names of the metadata with their capitals (it was `X-Amz-Meta-Owner`) must read them without: HTTP names are not case sensitive, and the SDKs do not look at the case. During a rolling upgrade a write in one call that an older replica makes may show in the list of uploads for the seconds it lasts.


**0.11.0** makes the `ETag` of an object made of parts the one S3 makes (the MD5 of the MD5s of the parts, a dash, the number of parts). The table of parts gains a column `md5` at start; nothing is to be done by hand. Objects written before keep their ETag, and an upload that is going on at the upgrade is completed with the older name; a client that keeps ETags sees no change for what it already holds.

**0.11.1** gives the calls to nodes an idle timeout (`BLOBGATE_NODE_TIMEOUT_SECONDS`, 30 s) and 3 retries (`BLOBGATE_NODE_RETRIES`); rclone's own values were 5 minutes and 10. Nothing is to be done; set them higher if a node is very slow to start moving bytes.

**0.11.2** opens a store whose node cannot be reached at load (the SFTP backend connects when it is made) and opens the node again every 10 seconds; nothing is to be done.

**0.11.3** gives every node its own short queue of bytes and leaves a node that failed alone for 10 seconds (the others are tried first). During those 10 seconds a write that needs every node (`min_acks` equal to the number of nodes) is refused. A link slower than about 150 KB/s needs a larger `BLOBGATE_NODE_TIMEOUT_SECONDS` (see docs/configuration.md). Nothing else is to be done.

**0.11.4** makes a read that walks round a damaged or missing copy of a block queue the repair for that node (the metric `blobgate_node_errors_total` gets `op="damaged"`). Nothing is to be done; expect a few repair tasks after the upgrade if a node holds a damaged copy.

**0.11.5** adds two settings, `BLOBGATE_HELD_BLOCKS` (default 32) and `BLOBGATE_CLIENT_IDLE_SECONDS` (default 120). A client with no byte moving for 120 seconds is now ended, and a call that finds no place for 30 seconds is answered `503 busy` with `Retry-After`; a client that waits longer than 120 seconds between bytes (a very slow upload from a script that stops to think) must raise the second setting. Memory of the process is now bounded by about 32 blocks of 8 MiB for clients (256 MiB), not counting the other buffers; the default limit of the chart (512 MiB) was not tried in a cluster.
