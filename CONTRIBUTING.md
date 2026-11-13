# Contributing

- **Bugs and ideas**: open an issue. **Security problems**: not in an issue; see [SECURITY.md](SECURITY.md).
- **Build and test**: `go build ./...`, `go vet ./...`, `go test -race ./...`. Most of the tests need an rqlite (`BLOBGATE_RQLITE_URL`; `docker run -d -p 4001:4001 rqlite/rqlite -fk -disco-mode=`), and the tests with the
  real servers (SFTPGo, Garage, a rqlite of their own, the program killed with SIGKILL) need Docker and `BLOBGATE_DOCKER_TESTS=1`. Without them those tests skip, and say so.
- **Quick start for a developer**: `git clone`, start an rqlite as above, `BLOBGATE_RQLITE_URL=http://127.0.0.1:4001 go test ./...` (about a minute, Docker not needed), then with `BLOBGATE_DOCKER_TESTS=1` (a few minutes).
- **Coding standard**: [Effective Go](https://go.dev/doc/effective_go) and the [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments), with `gofmt` formatting. It is enforced automatically: CI fails on unformatted
  code (`gofmt -l`), on `go vet`, on `staticcheck` (all its checks) and on `gosec` findings, and a change that fails them is not merged.
- **Style**: comments say why, not what. Names of cloud services appear only where a protocol needs them; our identifiers are neutral. Errors that a person sees say what is wrong and what to do.
- **Tests**: this is the policy, and it is not optional: **a change of behaviour comes with a test that fails without it; major new functionality comes with tests in the automatic suite; a fixed bug comes with a regression
  test; a parser of input comes with a fuzz test.** A change without them is asked to add them before it is merged. The statement coverage of the suite is kept at 80% or more, and CI fails below it.
- **Documents**: [docs/writing-docs.md](docs/writing-docs.md) (one kind of page, a command with its output, what was not run). A new setting, metric, command or error code has its line in the docs in the same change;
  `go test ./cmd/blobgate` fails when it has none.
- **Cryptography**: no primitive of our own; the standard library and `golang.org/x/crypto`, and say in the change which algorithm it is ([docs/security-policies.md](docs/security-policies.md)).
- **Commits**: a short subject saying what changed and why; `CHANGELOG.md` is kept for user-visible changes.
- **Conduct**: [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md). **Governance and roles**: [GOVERNANCE.md](GOVERNANCE.md). **Roadmap**: [docs/roadmap.md](docs/roadmap.md).
- **Sign-off (DCO)**: every commit carries a `Signed-off-by: Name <email>` line (`git commit -s`), which says that you have the right to submit the work under the project's license, as in [DCO.md](DCO.md). The `DCO`
  workflow checks every commit of a pull request (bots are exempt) and a commit without the line fails it.
- **License**: by contributing you agree that your work is under the MIT license of the project.

## When and how the tests run

- **By you, before a pull request**: `go vet ./...` and `go test -race ./...` (with `BLOBGATE_RQLITE_URL`, and with `BLOBGATE_DOCKER_TESTS=1` when you touch what the real servers cover); the fuzz targets with
  `go test -run '^$' -fuzz '^FuzzName$' -fuzztime 60s ./package`; the chart with `sh tools/test/chart.sh`.
- **Automatically**: on every push to `main` and every pull request, `.github/workflows/ci.yml` runs `gofmt`, `go vet`, the tests with the real servers under `-race`, the coverage gate, `govulncheck`, `staticcheck`,
  `gosec`, the chart check, the image for two platforms and a check that the build is repeatable bit for bit; `fuzz.yml` runs the fuzz targets weekly; `trivy.yml` scans the image; `scorecard.yml` and
  `dependency-review.yml` watch the supply chain; `release.yml` runs on a tag.
