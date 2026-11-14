# Security policies of the project

What the project does about its dependencies, its code, its secrets and its releases. The automatic part is in `.github/workflows/`.

## Dependencies

* Listed in a computer-readable way: `go.mod` and `go.sum` (every module with its version and its hash); the image's base is named in the `Dockerfile` and the actions in the workflows, pinned by version or commit.
* **Monitored**: Dependabot (`.github/dependabot.yml`) proposes updates weekly for the Go modules, the actions and the Docker base; `govulncheck` runs on every change and fails on a known vulnerability that the code
  can reach; dependency review checks the pull requests; Trivy scans the image weekly and on every change. Dependabot security updates are on.
* **Fixed or verified**: a vulnerability that the code can reach is fixed by updating the module, in a release; one that it cannot reach is written down (the reason) in the changelog or a note. A fix for a High or
  Critical problem in a dependency is released within 30 days of the report of the advisory (SECURITY.md).
* **Easy to update**: Go modules are updated with one command and the tests say whether it worked; rclone is pinned in `go.mod` and its tests are run again at every update (its library API is not stable).

## Code

* `gofmt`, `go vet`, `staticcheck` (all checks) and `gosec` (rules for common weaknesses) run on every change and fail it; the exceptions are in the code with their reasons.
* The tests run with the race detector, with the real servers (SFTP, WebDAV, FTP, S3) and with the process killed with SIGKILL; the statement coverage is kept at 80% or more by a gate in CI; the fuzz targets run weekly.
* New functionality comes with tests, a fixed bug with a regression test (CONTRIBUTING.md).
* No new cryptographic primitive of our own; the algorithms are the approved ones of the standard library: AES-256-GCM, HMAC-SHA-256, SHA-256, TLS 1.2 or later.

## Secrets

* The repository holds no secret: GitHub secret scanning with push protection is on; the test secrets are fixed fake values. The CI has no long-lived secret (the signing is keyless; the token of a run is made by
  GitHub for that run).
* A deployment's secrets (the tenant secrets, the admin token, the seal key, the key of the backups, the passwords of the nodes) are the operator's, can be files, and are never put in a log, a metric or an answer.

## Releases

* Made only by the release workflow, from a signed tag; the files, the image and the chart are signed keyless and carry build provenance and a bill of materials; the build is repeatable bit for bit
  ([verifying-releases.md](verifying-releases.md)).

## VEX: findings that do not affect the program

`govulncheck` runs on every change and fails the build for a vulnerability whose code the program **calls**. A vulnerability in a module that is required but whose vulnerable code is **not called** does not fail it; such findings are
accounted for in an [OpenVEX](https://openvex.dev) document, [`vex/blobgate.openvex.json`](../vex/blobgate.openvex.json), with the status `not_affected` and the reason (`vulnerable_code_not_present`). It is made by

```bash
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 -format openvex ./...
```

and is brought up to date when a release is made ([release.md](release.md), step 4): a finding that is gone (the module was updated) is taken out, a new one is added with the words that say why it does not affect the program. A finding that does
affect it is not a VEX statement: it is fixed under the times of [SECURITY.md](../SECURITY.md). As of 2026-11-14 the document has one statement: GO-2026-5932 (`golang.org/x/crypto/openpgp`, which the program does not call).
