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

**0.11.5** adds two settings, `BLOBGATE_HELD_BLOCKS` (default 32) and `BLOBGATE_CLIENT_IDLE_SECONDS` (default 120). A client with no byte moving for 120 seconds is now ended, and a call that finds no place for 30 seconds is answered `503 busy` with `Retry-After`; a client that waits longer than 120 seconds between bytes (a very slow upload from a script that stops to think) must raise the second setting. Memory of the process is now bounded by about 32 blocks of 8 MiB for clients (256 MiB), not counting the other buffers; the default limit of the chart (512 MiB) held in a cluster with 120 slow clients ([benchmark.md](benchmark.md), "Slow and quiet clients in a cluster").

**0.11.6** puts a part of a multipart upload through the S3 door under the places of `BLOBGATE_HELD_BLOCKS` too (a call that finds no place is told `503 SlowDown` with `Retry-After`), and finds a local node whose directory cannot be read on the status page. A part whose client leaves is answered `400 IncompleteBody` instead of stopping the handler with a panic. Nothing is to be done.

**0.12.0** fixes defects of safety and makes the module refuse some things that it accepted ([../CHANGELOG.md](../CHANGELOG.md)). Update as soon as you can. From this release on only the latest release is supported: 0.11 and 0.10 get no fixes ([supported-versions.md](supported-versions.md)).

* The schema gains a table (`storage_guard`) and a column (`storage_repair.rev`) at start; nothing is to be done by hand. Finish the rolling upgrade quickly: a replica of an older version does not use the new guard or the revisions, so the races stay open while one is running. After `blobgate backup restore` of a backup taken before 0.12.0, restart the replicas so that the table and the column are made.
* S3 door: a header that decides what a call does (`x-amz-copy-source*`, the metadata and tagging directives, `x-amz-meta-*`, `x-amz-checksum-*`, `x-amz-trailer`, `x-amz-decoded-content-length`, `Content-MD5`) must be in `SignedHeaders`, or the call is `403 AccessDenied`. A client that adds one after signing, or sends it with a presigned URL that did not name it, must sign it. A declared checksum that is wrong, or of an unknown algorithm, is now refused.
* Conditional writes: of two writers under one `If-Match` or `If-None-Match`, one now gets `412`; `If-Match` with a weak tag (`W/"..."`) no longer matches; the completion of an upload honours the conditions.
* Stores that seal: registering a store with a key id written as `01` or `+1`, with another text under an id that the store already has, or without a key that blocks are sealed with, is now `400`. A program that registers its stores at every start must send ids as `1` and keep the texts.
* A write is `503` when a copy that a node missed cannot be written to the repair queue; the new metric `blobgate_repair_enqueue_errors_total` counts these.
* Slow readers, or slow writers, can take at most three quarters of `BLOBGATE_HELD_BLOCKS` (24 of 32).

Run on 2026-11-26 with this code: the tests of the repository, the AWS CLI, boto3, rclone and s3cmd. Not run: the upgrade of a running 0.11.6, the other SDKs and `mc`.

**0.13.0** makes quotas hard limits and says less to callers about failures ([../CHANGELOG.md](../CHANGELOG.md)). Look at these before you update:

* **Quotas count the parts of uploads that have not ended.** An abandoned upload holds its room until it is aborted or swept (`BLOBGATE_UPLOAD_TTL_SECONDS`, a day). A tenant near its limit may need a larger one, or a shorter time. A part over the limit is now `507` when it is sent.
* **With more than one tenant, one takes at most three quarters of `BLOBGATE_HELD_BLOCKS`** (24 of 32), on each replica. `BLOBGATE_HELD_BLOCKS_PER_TENANT` (chart `config.heldBlocksPerTenant`) changes it; a value of `BLOBGATE_HELD_BLOCKS` or more is no limit.
* S3 door: an access key that is no tenant is answered `SignatureDoesNotMatch`, not `InvalidAccessKeyId`. The detail of a failure of the database or of a node, and of `507 store_full`, no longer names nodes, hosts or paths: it ends with `(ref <id>)`, and the log line with that reference has the text. A script that matched on the old words has to change.
* The schema gains a column (`storage_guard.quota`) and indexes by generation at start; nothing is to be done by hand, but making the indexes on a large inventory takes a while. During a rolling upgrade an older replica still treats quotas as soft. After `blobgate backup restore` of an older backup, restart the replicas.

Run on 2026-11-27 on one machine: 0.12.0 and this code side by side on one database and two local nodes (each reads what the other wrote, an upload begun on 0.12.0 is completed by the new one). Not run: an upgrade in a cluster, remote backends, the S3 clients other than the AWS CLI in that check, the Ceph tests.

**0.13.1** fixes a defect that loses data: an abort of an upload that had been completed removed the object's blocks ([../CHANGELOG.md](../CHANGELOG.md)). Update every replica as soon as you can; a replica of an older version still does it. Nothing stored or set has to change. An object that was lost this way is not brought back by the update.

**0.14.0** fixes a defect that loses data: sending a part of an upload again could damage the part that had been accepted ([../CHANGELOG.md](../CHANGELOG.md)). **This is not a rolling upgrade**: stop every replica of an older version before one of 0.14.0 writes. The layout of a store is now 2 and the name of a block of a part carries the attempt that wrote it, which an older program cannot read; do not go back to an older version after 0.14.0 has written. Objects written by earlier releases are read as before, and an upload that was going on can be completed. An object that was damaged by the defect is not repaired by the update. There is no schema change.

The chart updates with `RollingUpdate`, so `helm upgrade` alone runs both versions side by side. Stop the replicas first (`<release>` is the name of the release; the deployment is `<release>-blobgate`, or `<release>` when that name holds `blobgate`), then upgrade, which sets the number of replicas again:

```bash
kubectl -n <namespace> scale deployment/<release>-blobgate --replicas=0
```

```bash
helm upgrade <release> oci://ghcr.io/zystem/charts/blobgate --version 0.14.0 -n <namespace> --reuse-values
```

These two commands were not run. If both versions have run side by side for a time: the older replica answers errors for objects that the new one wrote in parts; by reading the code nothing is removed by it, which was not tested.

Run on 2026-11-28 on one machine: 0.13.1, then this code on the same database and two local nodes (reads what was written, takes a part again and completes an upload begun on 0.13.1). Not run: an upgrade in a cluster, remote backends, the Ceph tests.
