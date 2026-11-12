# The documents

Every page is of one kind (Diátaxis). The README at the root is the way in.

| Page | Kind | What is in it |
|---|---|---|
| [../README.md](../README.md) | tutorial | the try-it, with the output of every command |
| [backup.md](backup.md) | how-to | back up rqlite, restore into an empty one, choose an older backup |
| [encryption.md](encryption.md) | how-to and reference | seal the blocks of a store; the keys; what it costs |
| [configuration.md](configuration.md) | reference | every environment variable and command, with default and whether a restart is needed |
| [metrics.md](metrics.md) | reference | every metric |
| [../internal/api/openapi.yaml](../internal/api/openapi.yaml) | reference | the HTTP API (served at `/v1/openapi.yaml`; a test keeps it equal to the routes) |
| [release.md](release.md) | how-to | cut a release: what the tag does, what to check after |
| [data-lifecycle.md](data-lifecycle.md) | explanation | what happens to a block, and what a crash leaves |
| [rclone-probe.md](rclone-probe.md) | explanation | what rclone does on real nodes, with what was run |
| [benchmark.md](benchmark.md) | explanation and record | speed and memory against the Nim module |
| [status.md](status.md) | record | what is built, what is not, what was run |

A setting, a metric or an error code that the program has and a page does not mention fails `go test ./cmd/blobgate` (`docs_test.go`).
