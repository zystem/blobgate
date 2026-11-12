# Status of the Go module (2026-11-12)

Built and tested (`go test -race ./...` with `CINIM_RQLITE_URL`; without it the API tests skip): the tokens (`internal/auth`, vectors from the Nim code), the sealing (`internal/seal`), the key layout
(`internal/layout`), the rqlite client and tables (`internal/rq`, `internal/db`), stores and mirrors over rclone (`internal/store`), the core API, the port for shims and the admin API (`internal/api`),
the repair queue, the check of the inventory (size, or deep: the digest), the sweep of the blocks of uploads that never ended and the lease of the replica that does them (`internal/maint`), the program
(`cmd/blobgate`; SIGTERM lets go of the lease, checked by hand). Run against `local` nodes, and (`BLOBGATE_DOCKER_TESTS=1`, Docker, the image `drakkan/sftpgo`) against real WebDAV, FTP and SFTP servers: a mirror of WebDAV + FTP, of WebDAV + FTP + SFTP, a node that goes away and comes back, a node that loses its disk, names that a node could trip on (space, trailing dot, unicode, a 250-byte segment, `:*?`, case), 24 writers under one parent directory. Passed on 2026-11-12.

A block that does not fit: when a node says how much room it has (`about`, or Garage's `space_url`) the module refuses beforehand with 507 and the figures; when it cannot (FTP, most WebDAV), the write
is tried and, if too few nodes take the block because a node reports no room (ENOSPC, quota, FTP 552, WebDAV 507), the answer is the same 507 `store_full` with a sentence that names the node and says what to do
(`store.ErrNoSpace`, test `TestNodeOutOfSpaceIsAnInformativeRefusal`). A node that is full while the others are enough (`min_acks`) is given the block later by the repair pass.

Not built yet: Prometheus metrics; a test that every route of `openapi.yaml` exists; the Nim process tests against this binary (they start Caddy nodes, which this module does not use); tests with S3 (Garage) nodes and with servers other than SFTPGo (the error words of a full disk are matched by text and are known only from the documentation of the protocols); speed and memory;
a Dockerfile and the pipeline; the end of the pass on SIGTERM waits for the pass in progress (a long pass delays the stop).

Known limits: a read holds one block (at most 8 MiB) in memory for each read going on (`CINIM_STORAGE_THREADS`, default 8, bounds them); two writers of one key at the same moment may leave the blocks of
one generation behind (not swept: the upload row of the loser is removed by its own commit); node names with a difference of case only meet on a node that folds case; a node that is merely
unreachable during the check gets a `copy` task like one that lost its disk, and the task is put off with a growing pause.
