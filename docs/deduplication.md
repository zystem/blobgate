# Deduplication: the same bytes are kept once

How-to and explanation. A store can keep one copy of the blocks of objects that have the same bytes: the layers of images, the tarballs of dependencies, a cache that was saved twice. It is **off by default**
and costs nothing for a store that does not turn it on.

## Turn it on

Register the store again with `dedup` (the admin call replaces the settings of the store, so everything else is sent again):

```bash
curl -X PUT "$STORAGE/v1/admin/stores/main" -H "Authorization: Bearer $ADMIN" -d '{
  "kind": "mirror", "default": true, "dedup": "tenant",
  "nodes": [{"remote": ":local:/data/n1"}, {"remote": ":local:/data/n2"}]}'
```

It answers `{"name":"main","checked":true}` (not run as a command in this form: `internal/api/dedup_test.go` does the same through the API).

| `dedup` | Meaning |
|---|---|
| `""` (the default) | off: every object has blocks of its own |
| `"tenant"` | objects of **one tenant** with the same bytes share their blocks |
| `"store"` | objects of **every tenant of the store** with the same bytes share their blocks |

A change takes effect for the objects written from then on (a replica reads the setting within two seconds). What is written already stays as it is and is still read; turning `dedup` off does not break an object
that shares blocks.

## What is shared, and what is not

* **A whole object.** Two objects share their blocks when every block of them is the same, in the same order, with the same block size. That is a layer that was pushed twice, a file copied to another key, a
  cache saved by two runs. **An object that differs in one byte shares nothing**, and neither does a file that begins with other bytes (blocks are cut at 8 MiB from the start; they are not cut by content).
* **A PUT writes its bytes before it finds they are a copy.** A write puts its blocks on the nodes as it always did, and the transaction that fixes the object then finds the set of blocks that is there and
  removes the blocks it had just written. The store keeps one copy; that write is not faster for it. To send nothing at all, ask first (the next section).
* **The listing, the usage and the quota are of what the tenant holds**, not of what the store keeps: two copies of 1 GiB are 2 GiB of usage. The operator sees both in
  `GET /v1/admin/stores/{name}:usage` (`dedup`: `sets`, `stored_bytes`, `objects`, `logical_bytes`) and in `blobgate_dedup_*`.
* **The version (`ETag`) of two objects with the same bytes is the same**: it is the MD5 of the bytes for an object written in one call, and a name made of the digests of the blocks for one written in parts.

## Send nothing when the store has the bytes (`?link`)

A client that knows the SHA-256 and the size of what it is about to send can ask the store to make the key point to bytes it keeps already, and send nothing. It works in a store whose `dedup` is `"tenant"` and finds
only sets of the same tenant (with `"store"` a digest would be a key to the bytes of other tenants, so the call is refused: `409 dedup_not_tenant`).

```bash
curl -X POST "$STORAGE/v1/stores/main/objects/acme/layers/app.tar?link" -H "Authorization: Bearer $TOKEN" -H "X-Blobgate-Tenant: acme" \
  -d '{"sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "size": 4}'
```

It answers `{"size":4,"linked":true}` when the bytes were there, and `404 {"error":"no_such_content", ...}` when they were not: then send them with PUT. (Not run in this form; `TestLinkTakesTheBytesThatAreThere` does the same.)
The old object of the key, if there is one, is replaced in the same transaction, and if the set was removed between the lookup and the transaction the transaction fails whole and the old object stays. Only sets made by a
PUT in one call have a digest: **a set made by a multipart upload, or by 0.5.0, is not found** (send the bytes once; the set then has it). The metadata headers, `If-Match` and the quota are as in a PUT. The Go client does the
asking and the sending in one call: [go-client.md](go-client.md).

## How it is kept true

A set of blocks is a row of `storage_sets` (its id is a digest of the scope, the key, the block size and the digests of the blocks) and a directory of blocks on every node. `refs` is the number of objects that
point to it. Every change of `refs` is made **in the transaction of the object row that takes or gives the reference** (a write, a replacement, a delete, the removal of the trash), reading the old row inside
that transaction; so a crash leaves either both or neither, and two writers of the same key at once cannot count twice. A set with no reference is removed by the worker (`blobgate_sets_removed_total`): the
removal is written down in the transaction that deletes its row, and only if `refs` is still 0 there, so a write that takes the set again at the same moment decides the matter in the database, never in a
timing. There is **no waiting period** and no scan of the store to find blocks that nobody uses: the new writer never relies on blocks it did not write itself; it finds or makes the set in its own transaction.

| Step | What is left | Who cleans it |
|---|---|---|
| a write found the bytes in the store; the transaction is done, its own blocks are not yet removed | the blocks it wrote, with a `purge` task for every node | the repair pass |
| a delete (or the end of the trash): the reference is given back | a set with `refs = 0` | the worker's pass, which removes the row and writes down the removal of the blocks in one transaction |
| the worker removed the row and died | the blocks, with a `purge` task | the repair pass |
| a node lacks a block of a set | the other node has it | the check makes a `copy` task for it, as for any block |

An object in the trash keeps its reference until the trash is emptied.

## Encryption

A store that seals its blocks must seal the same bytes to the same sealed bytes, or nothing would be found equal. With `dedup` the blocks are sealed **convergently**: the key and the nonce of AES-256-GCM are made
(HMAC-SHA256) from the key of the store, the scope and the digest of the plain block, so the same bytes give the same sealed bytes within one scope, and another scope gives other bytes. What it means:

* An attacker with a node sees sealed blocks, and can tell that two objects of the same scope have the same bytes (they point to one directory). He cannot read them, and cannot test a file he has against them
  without the key of the store.
* A tenant who can write can learn that bytes **are** in the store already by the time a write takes or by the usage the operator shows, **if the scope is the whole store** (`"store"`). With `"tenant"` he
  learns it only about his own bytes. This is why `"tenant"` is the setting to start with, and `"store"` is for stores in which all the tenants trust one another (one organisation's mirror).
* Objects sealed before `dedup` was turned on are sealed the old way (random nonce, tied to the place) and are never shared.

## What it costs

Run 2026-11-14 on one machine (the same that [benchmark.md](benchmark.md) names, with other work going on; two or three runs each unless a row says one, the figures move by a fifth), `cmd/bench`, two local nodes on `/dev/shm`, the loads write the same
bytes under many keys, which is the case that `dedup` is for:

| Load | `dedup` off | `dedup: "tenant"` |
|---|---|---|
| PUT 8 MiB, 4 workers | 875 to 895 MB/s | 753 to 772 MB/s (about 14 % less) |
| PUT 8 MiB, 4 workers, sealed (one run each) | 543 MB/s | 531 MB/s |
| PUT 64 KiB, 8 workers | 810 to 950 /s | 490 to 530 /s (a transaction of more statements: about 40 % less) |
| GET 8 MiB, 4 workers | 1540 to 1680 MB/s | 1557 to 1586 MB/s (the same) |
| GET 64 KiB, 8 workers | 2600 to 2960 /s | 4325 /s (one run, after the read of an object was made one query; every object reads the same few files, which the system keeps in memory) |
| disk used on one node for 64 objects of 8 MiB and 2000 of 64 KiB, all of the same bytes (one run each) | 637 MB | 9 MB |

## What was run, and what was not

Run (`internal/api/dedup_test.go`, `link_test.go`, local nodes, a real rqlite, `-race`): three objects of the same two-block bytes keep two blocks and read back whole and by range; the version is the same; the usage is of the
tenant; blocks stay while one object points to them and go with the worker's pass after the last; a replace and a rewrite of the same bytes; the scope of a tenant and of a store; sealed sets (the nodes hold
no plain bytes, a node that lost its disk is repaired through the check and read alone); a multipart upload that takes the set of a plain write; the trash holds the reference, and a restore keeps the object; a
process killed before and after the transaction; 24 writers and deleters of the same bytes at once, after which `refs` is the number of rows and nothing is left once all is deleted; a link of bytes that are there, of
bytes that are not, of another tenant's, over an object, of a set the worker removes in the middle of it, and the refusals by scope. **The whole program killed with SIGKILL** four times in a row while 12 writers
and deleters work on the same two bodies (`TestKillNineDuringDedupWrites`, 2026-11-14, a rqlite in Docker): after each restart `refs` was the number of rows, no object pointed to a missing set, the queues emptied, the blocks
on both nodes were those of the sets, and when all was deleted nothing was left.

Found by running the AWS CLI against the S3 door (2026-11-14): the listing of an object that points to a set named another version than a read did, so `aws s3 sync` refused the download; fixed
(`TestListNamesTheVersionOfASet`) before 0.6.0, and it was a fault of 0.5.0.

Not run: a store of real FTP, WebDAV or S3 nodes with `dedup` (the code does not differ between nodes); the memory of a store with `dedup` (the sealing path holds a block in memory, as without it); a
store with millions of sets (the worker's query is indexed on `refs`, and was not measured).
