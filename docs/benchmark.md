# Speed and memory against the Nim module

Explanation and record. Run 2026-11-13 on one machine (a workstation with other work going on; the figures move by a fifth between runs), one rqlite in Docker on a tmpfs, nodes on `/dev/shm`.
The tool is `cmd/bench` (the same HTTP calls the core makes, signed as the core signs them); the script is `tools/bench/stand.sh`. Each cell is the median of 3 runs.

| Load | Nim module + 2 Caddy nodes | Go module + the same 2 Caddy nodes | Go module + 2 local nodes | Go module + 2 local nodes, sealed |
|---|---|---|---|---|
| PUT 8 MiB, 4 workers | 224 MB/s (27/s) | 966 MB/s (115/s) | 1198 MB/s (143/s) | 716 MB/s (85/s) |
| GET 8 MiB, 4 workers | 1330 MB/s (159/s) | 1122 MB/s (134/s) | 1298 MB/s (155/s) | 1037 MB/s (124/s) |
| PUT 64 KiB, 8 workers | 155/s | 555/s | 748/s | 676/s |
| GET 64 KiB, 8 workers | 1905/s | 1725/s | 3148/s | 3616/s |
| PUT 1 GiB in parts of 8 MiB, 1 worker | 106 MB/s | 324 MB/s | 268 MB/s | 179 MB/s |
| GET 1 GiB in ranges of 8 MiB, 1 worker | 374 MB/s | 299 MB/s | 370 MB/s | 312 MB/s |
| Peak memory of the module process (VmHWM, all loads) | 47 MB | 76 MB | 75 MB | 115 MB |

"The same Caddy nodes" are the `build/caddy-store` of the Nim stand; for the Go module they are plain WebDAV nodes with no `forward_auth` (the Go module does not use it), spoken to through rclone.

## What it says

* **Writes are several times faster** than with the Nim module on the same nodes, and reads are the same within the noise (the 1 GiB read and the small reads are a little lower, 10 to 20 %). Why the Nim module writes slower was not looked for.
* **Memory is not as good: 75 MB against 47 MB** (115 MB sealed). A read holds the whole block (up to 8 MiB) to check its digest before a byte goes out, a sealed write holds two; the number of blocks held is
  bounded (`BLOBGATE_THREADS`, 8) but the size of a block is not made smaller. The module sets `GOGC=25` to keep the heap near what is live (without it the peak of the same run was 102 MB).
  "Not worse than Nim" is **not reached** for memory; the figure the task gave (35 MiB for six blocks at once) was not measured here because the Go module holds a block, not a stream.
* **Sealing costs** about 40 % of the write rate and 20 % of the read rate of 8 MiB blocks, and memory.

## What was found while measuring (and fixed)

* rclone's WebDAV backend sleeps 10 ms between two calls to one node (`pacer_min_sleep`), which held a node to about a hundred calls a second: 23 writes and 46 reads of 64 KiB a second. The module now
  sets 100 µs (a store that names the option keeps its value, for a server that throttles).
* Every write took the node's lock to make the directory it shares, with a round trip or several to make it; the directory is now remembered, and made once.
* A read asked the database twice for the settings of the store and once, at the strong level, for the row of the object: the settings are kept for 2 s (`store.RowTTL`; a change made through another replica shows
  within that time) and the row is read at the weak level (the leader answers from its own state). Small reads went from 2290/s to 6232/s on local nodes.
* A read buffer that grows holds up to twice the block; it is now made of the size the block must have.

## What is not comparable

* The Nim module was measured with a new connection for every request (`-nokeepalive`): with idle connections reused its server answered some requests with an error ("Connection was closed before full request has been
  made"). The Go module was measured with reused connections. This favours the Go module a little on the small loads.
* `docs/store.md` of CiNim gives 195 / 1081 MB/s (8 MiB), 1208 / 9390 per second (64 KiB) and 54 / 372 MB/s (1 GiB) for the Nim module. The 8 MiB figures and the 1 GiB read were reproduced here (224 / 1330, 374);
  the small ones (155 and 1905 per second) and the 1 GiB write (106) were not, so the stand of that document was not this one. The comparison above is between the modules on this stand.
* No network between the module and its nodes, no sealing of the Nim side (it has none), one machine for the load and the module.
