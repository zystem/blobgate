# Status of the Go module (2026-11-12)

Built and tested (`go test -race ./...` with `BLOBGATE_RQLITE_URL`; without it the API tests skip): the tokens (`internal/auth`, vectors from the Nim code), the sealing (`internal/seal`), the key layout
(`internal/layout`), the rqlite client and tables (`internal/rq`, `internal/db`), stores and mirrors over rclone (`internal/store`), the core API, the port for shims and the admin API (`internal/api`),
the repair queue, the check of the inventory (size, or deep: the digest), the sweep of the blocks of uploads that never ended and the lease of the replica that does them (`internal/maint`), the program
(`cmd/blobgate`; SIGTERM lets go of the lease, checked by hand). Run against `local` nodes, and (`BLOBGATE_DOCKER_TESTS=1`, Docker, the image `drakkan/sftpgo`) against real WebDAV, FTP and SFTP servers: a mirror of WebDAV + FTP, of WebDAV + FTP + SFTP, a node that goes away and comes back, a node that loses its disk, names that a node could trip on (space, trailing dot, unicode, a 250-byte segment, `:*?`, case), 24 writers under one parent directory; and a real Garage (S3) through rclone, with the room taken from its metrics. Passed on 2026-11-12.

A block that does not fit: when a node says how much room it has (`about`, or Garage's `space_url`) the module refuses beforehand with 507 and the figures; when it cannot (FTP, most WebDAV), the write
is tried and, if too few nodes take the block because a node reports no room (ENOSPC, quota, FTP 552, WebDAV 507), the answer is the same 507 `store_full` with a sentence that names the node and says what to do
(`store.ErrNoSpace`, test `TestNodeOutOfSpaceIsAnInformativeRefusal`). A node that is full while the others are enough (`min_acks`) is given the block later by the repair pass.

The routes of the server and `openapi.yaml` are checked to be the same set (`TestOpenAPIMatchesRoutes`).

The cleanup of a replaced or deleted generation is written down in the transaction that changes the row, and a process that dies at any step leaves nothing that nobody knows of (docs/data-lifecycle.md, `TestCrashAtEveryStep`, `TestConcurrentReplacementsLeaveOneGeneration`).

Also built: `blobgate validate` (what a node can do, before it is put in a store), the version of the layout in the settings of a store (a program refuses a newer one), `:reconcile` (a check of the nodes against the database from outside, dry by default), backup and restore of rqlite (docs/backup.md).

Also built: sealing of the blocks of a store on the fly (docs/encryption.md); a restore that names the older backups when the newest is damaged, `verify -all`, `restore -id`, `-fallback`.

Measured against the Nim module on the same stand (docs/benchmark.md): writes several times faster, reads the same, memory 75 MB against 47 MB (not as good).

Also built: Prometheus metrics (docs/metrics.md), the image (`Dockerfile`: static, user 65532, read-only root; run for real with the try-it of the README), the README and the reference pages, a test that a setting, a metric, a command or an error code without a line in the docs fails the build.

Run as a process with SIGKILL (`cmd/blobgate/process_test.go`): a writer killed in the middle of a block, and the replica that holds the lease killed with another one running (2026-11-13).

Not built yet: the Nim process tests against this binary (they start Caddy nodes, which this module does not use); tests with servers other than SFTPGo and Garage (the error words of a full disk are matched by text and are known only from the documentation of the protocols); speed and memory;
the pipeline (CiNim's `.ci/pipeline.lua`) and the chart; the end of the pass on SIGTERM waits for the pass in progress (a long pass delays the stop).

Known limits: a read holds one block (at most 8 MiB) in memory for each read going on (`BLOBGATE_THREADS`, default 4, bounds them); node names with a difference of case only meet on a node that folds case; a node that is merely
unreachable during the check gets a `copy` task like one that lost its disk, and the task is put off with a growing pause.
