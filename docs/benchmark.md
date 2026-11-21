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
  bounded (`BLOBGATE_THREADS`, 4 since 0.6.1; the table above was measured with 8; see "The limit of threads" below) but the size of a block is not made smaller. The module sets `GOGC=25` to keep the heap near what is live (without it the peak of the same run was 102 MB).
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

## Deduplication, 2026-11-14

The cost of `dedup: "tenant"` was measured with the same tool: [deduplication.md](deduplication.md), "What it costs".

## The limit of threads, 2026-11-16

`BLOBGATE_THREADS` went from 8 to 4 in 0.6.1. Measured with `tools/bench/stand.sh go-local 25` (two local nodes, `GOGC=25`, one run of each, the same machine as above), 8 MiB objects, `-workers` clients at once:

| Load | threads 4 | threads 8 |
|---|---|---|
| 4 clients, GET, peak memory | 70 MB | 69 MB |
| 4 clients, sealed, peak memory | 119 MB | 105 MB |
| 16 clients, GET 8 MiB | 2059 MB/s, peak 128 MB | 2409 MB/s, peak 162 MB |
| 16 clients, sealed, PUT 8 MiB | 472 MB/s | 665 MB/s |
| 16 clients, sealed, GET 8 MiB | 1296 MB/s, peak 132 MB | 1598 MB/s, peak 213 MB |

* With 4 clients the limit is never reached: the memory and the speed are the same (the differences are the noise of one run).
* With 16 clients the peak falls by a fifth (plain) to a third (sealed), and the rate of 8 MiB objects by 15 to 30 %: the reads and sealed writes beyond the fourth wait for a place.
* One run of each, so a difference of a few per cent is not a finding. The Nim comparison above was not made again.

### On a small machine, 2026-11-18

The same load on another host: a virtual machine with 2 cores and 1.7 GB, AlmaLinux 10, the nodes on its disk (not `/dev/shm`), rqlite as a process on the same host, one run of each, `GOGC=25`, 32 clients, 96 objects of 8 MiB
(`cmd/bench -workers 32 -large 96 -small 500`). Peak memory of the module process (`VmHWM`):

| Threads | Read | Sealed, read | Sealed, after the writes | Rate of the read (sealed) |
|---|---|---|---|---|
| 4 | 104 MB | 111 MB | 95 MB | 253 MB/s (175) |
| 8 | 157 MB | 161 MB | 138 MB | 237 MB/s (170) |
| 16 | 260 MB | 268 MB | 227 MB | 193 MB/s (183) |

* The peak grows with the number of threads by about 13 MB for each (a block of 8 MiB and its buffer): 4 to 8 threads add 53 MB, 8 to 16 add 103 MB. On the laptop above, 4 and 8 threads gave 128 and 162 MB with 16 clients.
* On 2 cores and a disk the rate of the read does **not** grow with the threads (it falls a little): the cores and the disk are busy, and more threads only hold more blocks. For such a machine 4 is enough.
* The rates of the writes are scattered (80 to 98 MB/s plain), and one run (4 threads, sealed) wrote at 9.8 MB/s against about 70 to 90 in the others; this is taken for the disk, not for the limit, and nothing is concluded from the writes.
* One run of each, two cores: the rates are not comparable with the table at the top, only the memory is.

## Slow and quiet clients in a cluster

Why: before 0.11.5 a client that took one byte of an object and stopped held a block of 8 MiB, with no bound (48 of them 385 MiB in a test inside the process). The limit `BLOBGATE_HELD_BLOCKS` (32 blocks, 256 MiB) and the idle time `BLOBGATE_CLIENT_IDLE_SECONDS` (120) are tested in the process (`TestMemoryOfSlowClients`, `TestMemoryOfQuietPartUploads`); this is the same in a cluster, with the limit of the chart, as `kubectl top pod` shows it (the memory of the whole container, which the kernel kills at the limit).

**Run:** the chart `blobgate` 0.11.5 (2026-11-21) and 0.11.6 (2026-11-22), one replica, `resources.limits.memory` 512 Mi (the default of the chart), two local nodes on an `emptyDir`, an rqlite pod, a Talos cluster of six nodes at home, namespace made for the test and deleted after. The clients run in a pod of the same cluster (a client on the other side of `kubectl port-forward` hid the problem: the connection and `kubectl` took 4 to 23 MB of every object, so the module held little), with a receive buffer of 4 KiB, and hold their calls for 90 seconds. `kubectl top` is read every 15 seconds (it lags: Go does not give memory back at once).

| Clients (all at once) | Answers | Memory of the pod | Restarts |
|---|---|---|---|
| 0.11.5: 60 readers that took 1 byte of 64 MiB and stopped | 32 `200`, 28 `503 busy` | 272 MiB (was 41) | 0 |
| 0.11.5: 60 writers of no known length that sent 3 MiB and stopped | 32 taken, 28 `503 busy` | 118 MiB | 0 |
| 0.11.6: 40 readers, then 40 such writers, then 40 parts through the S3 door (6 MiB of 8 sent, then quiet) | 32 readers `200`, 8 `503`; all 40 writers and all 40 parts `503` | 284 MiB at the peak, 270 MiB at the end | 0 |
| 0.11.6: 40 parts alone | 32 held, 8 `503` | 278 MiB, 24 MiB after they were gone | 0 |
| 0.11.6: 20 readers and 20 parts held for 150 s (more than the idle time) | 12 parts ended by the module after 120 s (`400`), 8 `503`, the 20 readers `200` | 278 MiB | 0 |

What it shows:
* The limit holds in a cluster: 32 blocks cost about 270 to 285 MiB, the process was not killed at 512 MiB in any run, and 60 such clients used to be enough to end it.
* **A client that comes when every place is taken waits 30 seconds and is told `503 busy`** (`503 SlowDown` at the S3 door) with `Retry-After`: an ordinary read of a small object, sent in the middle of the third run, was answered so after 30 s. That is the price of the bound, and the reason for sizing `BLOBGATE_HELD_BLOCKS` and the memory limit above the clients that are really slow at the same time.
* **The places are not shared out between kinds of call.** The readers that came first took all 32, and the writers and parts that came after got `503`. A flood of slow readers can keep writers out for as long as it holds the places (at most `BLOBGATE_CLIENT_IDLE_SECONDS` for a client that sends nothing).
* The idle time ends a part that sends nothing for 120 seconds (12 `400`, `IncompleteBody`); the readers of that run were closed by the client after 150 s, so the module's cut of a quiet reader is shown by the test in the process, not here.

**Not run:** several replicas under this load (each has its own places); a slow but moving client (the idle time counts a piece of 256 KiB); the memory of the process as RSS by the kernel (`kubectl top` is the working set of the container); a link with real loss; the limit of the chart with `BLOBGATE_HELD_BLOCKS` raised.

