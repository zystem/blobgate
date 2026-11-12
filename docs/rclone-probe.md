# What rclone does on real nodes (probe, 2026-11-12)

Explanation page: facts that the design of blobgate rests on, with what was run and what was not.

**Run:** rclone v1.76.0 as a Go library (`fs.NewFs` with a connection string, no config file), against `local`, and against FTP, WebDAV (`vendor=other`) and SFTP served by one
SFTPGo container (`drakkan/sftpgo:latest`, the image on the workstation on 2026-11-12, default settings, one user with all rights). Blocks of 8 MiB. A probe program, not kept.

| Question | local | FTP | WebDAV | SFTP |
|---|---|---|---|---|
| Put into a directory that does not exist (no mkdir) | works | works | works | works |
| Range read (`fs.RangeOption`) | works | works | works | works |
| Overwrite of an existing file | works | works | works | works |
| Delete a directory tree (`operations.Purge`) | works | works | works | works |
| Delete of a file that is not there | error | `object not found` | `404` | error |
| `About` (free space) | yes | **no** | answers, all fields empty | yes |
| Hashes the node gives | many | none | none | none (hashcheck off) |
| Source fails in the middle of a put (a clean error) | nothing left | nothing left | nothing left | nothing left |
| **The writer is killed with `kill -9` in the middle of a put** | not run | **partial file under the final name** | **partial file under the final name** | **partial file under the final name** |

Findings that change the design:

1. **A killed writer leaves a partial file under the final name** (3 MiB of 8 MiB on all three network nodes). A file that exists is not a file that is complete. So an object is fixed by the row in
   rqlite and every block is checked against its SHA-256 on read, as designed; a block that fails the check counts as missing on that node. This also means a block must **never be overwritten in
   place**: a replacement is written under a new generation and the row switches only when the blocks are in. The idea of keeping a one-block object under its plain key is dropped by this:
   an in-place replacement killed in the middle would destroy the old copy on every node written at that moment. A one-block object lies under its generation path like any other.
2. Free space cannot be taken from `About` alone: FTP has none, WebDAV (this server) answers empty. Unknown space refuses nothing (docs/storage-module.md, "Room in the store"); a node that does not
   say is listed as such by `:check`.
3. No node gives a hash that can be trusted, so the module hashes every block itself (reads, repair, deep check).
4. Directories need not be made first; `Purge` of a generation directory is how a generation is removed.
5. A binary with the backends s3, webdav, ftp, sftp and local is 39 MB (a probe, not stripped).

**Not run:** S3 (Garage) through rclone; an FTP server without `SIZE` or `REST`; a node that goes away in the middle of a block (the server side); a case-insensitive node; names near the limits
(255-byte segment, 260-byte path, characters a Windows server refuses); speed and memory; a server that gives a partial file only to the reader (a half-written file seen by a concurrent read).
