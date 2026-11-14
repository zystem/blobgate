# What happens to a block, and what a crash leaves

Explanation page. The test that keeps it true is `TestCrashAtEveryStep` (`internal/api/crash_test.go`): the server is stopped at each point below, as a process that is killed there, and after the
passes of the next replica the nodes must hold exactly the blocks of the objects in the database, with nothing left in `storage_repair`, `storage_uploads` or `storage_parts`.

## The rule

An object is **one row** in `storage_objects` (the generation and the SHA-256 of every block). A file on a node is a block of the object only if the row says so. A block is written under the path of a
generation (`<key>~<generation>/<n>`) and never over another one. The row is switched to the new generation in one transaction, and **the same transaction writes down, for every node, the removal of the
generation it replaces** (a `purge` task, read from the row in the transaction itself, so that writers of one key at the same moment each write down the generation they replaced). The removal is then
tried at once, and the task is taken out of the queue by the node that did it. Nothing is ever only in memory.

## Where a process can die

| Step | What is left | Who cleans it |
|---|---|---|
| a PUT or an upload has begun (`storage_uploads` row written) | the row | the sweep, after `BLOBGATE_UPLOAD_TTL_SECONDS` (24 h) |
| blocks are being written | partial files under the generation path, on some nodes | the sweep (the upload row is still there) |
| a block is written, its record in `storage_parts` is not | the block | the sweep |
| all blocks written, the transaction not yet | the blocks; the old object is untouched and is the object | the sweep |
| the transaction is done, the old generation is not yet removed | the old blocks, with a `purge` task for every node | the repair pass |
| a delete: the transaction is done, the blocks are not yet removed | the blocks, with a `purge` task for every node | the repair pass |
| a soft delete: the row is marked as deleted, the blocks are not touched | the object in the trash, restorable (`delete:after-trash`) | the worker, when `soft_delete_seconds` is over: one transaction writes the `purge` tasks and removes the row, only if the object is still the same and still in the trash (a restore that came first wins) |
| a write in a store with deduplication that found its bytes in the store: the transaction is done | the blocks it wrote, with a `purge` task for every node | the repair pass (docs/deduplication.md) |
| the last object of a set of blocks is deleted | the set with `refs = 0` | the worker's pass: one transaction removes the row and writes down the removal of the blocks |
| an abort: the blocks are removed, the rows are not | the rows | the sweep |

A node that cannot take a write while the others do (with `min_acks` below the number of nodes) gets a `copy` task; the repair pass reads the block from a node that has it, checks its SHA-256 against
the database and writes it.

Besides the stops at named points, the program as a process is killed with SIGKILL in the middle of a block of 8 MiB, of a new object and of the replacement of one (`TestKillNineInTheMiddleOfAWrite`,
`cmd/blobgate/process_test.go`, with a real rqlite): after the restart the object that was there is as it was, the one that was not is not, and the blocks the dead writer left (partial files, seen on the
nodes before the restart) are removed by the sweep. With two replicas, the one that holds the lease is killed and the other takes the lease when it runs out and does the repair that was waiting
(`TestTheReplicaThatTakesOver`, `BLOBGATE_LEASE_SECONDS`).

## What a read trusts

Nothing on a node. A file that exists is not a file that is complete (a killed writer leaves a partial file under the final name on FTP, WebDAV and SFTP: docs/rclone-probe.md). A read takes a whole
block, compares its SHA-256 with the one in the row, and goes to the next node when it does not match.

## Why it is written like this

Two repository managers lost track of blocks in this gap (the record changed, the cleanup not yet done): a "compact" task that never reclaimed the space because its index of deletions fell out of step
with the files (Nexus), and files left by interrupted writes (restic over SFTP, rclone chunker). Listing a node to find strays is not relied on: it is slow, and on some servers it is not consistent.
