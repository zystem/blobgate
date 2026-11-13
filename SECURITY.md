# Security policy

blobgate keeps the data of build runs (artifacts, caches, and with `blobgate backup` the whole database of the tenant), so a report of a weakness is welcome and is read first.

## Reporting

- Use GitHub's private vulnerability reporting for this repository (Security, Report a vulnerability), or write to the maintainer at the address on the commits (a noreply address is used there; ask for a private
  channel in a first message that holds no details).
- Do not put details of an unfixed problem into a public issue.
- Say what you saw, on which version or commit, and how to see it again; a failing test is the best report.

## The process, and the timeframe

- **Acknowledgement** within 7 days of a report; an assessment (confirmed, not reproducible, not a vulnerability) within 14 days.
- **Fix**: a confirmed problem is fixed and tested first (a regression test goes in with the fix); for High and Critical severity the target is a release within 30 days, for the rest the next release. The reporter
  is told when the fix is ready.
- **Disclosure**: after the fix is released, or after 90 days from the report if no fix is possible, whichever is first (earlier by agreement). The problem is then described in the changelog and in a GitHub security
  advisory, **with the people who reported it credited by name** unless they ask not to be. A fix is released with its tests and a note of which versions are affected.
- **Contact**: the private vulnerability reporting of this repository is the security contact. GitHub shows those reports to the owner of a personal repository only, so there is a second way when the owner does
  not answer within 7 days: open a **public issue that holds no details** ("security contact request") or mention `@Juliesergienko` in it; the second maintainer then arranges a private channel with you (a private
  email or a call), takes the report and fixes it as described here.

## What is supported

- **Scope**: the latest release (every fix of a security problem is made there) and the head of `main`. An earlier release does not get a fix: update to the latest.
- **Duration**: a release is supported until the next release is made; the previous minor release (for example 0.2.x when 0.3.0 is out) is supported for 30 days after the new one, for fixes of High and Critical
  problems only. The project is young (first released in November 2026) and has no long-term-support line. The list is in [docs/supported-versions.md](docs/supported-versions.md).
- **End of support** is said in the release notes of the release that replaces the one that ends.
- How a release is verified: [docs/verifying-releases.md](docs/verifying-releases.md). The policies for dependencies, analysis and secrets: [docs/security-policies.md](docs/security-policies.md). How the project is run:
  [GOVERNANCE.md](GOVERNANCE.md).

## What helps a reviewer

- [docs/security-requirements.md](docs/security-requirements.md) (what a user can and cannot expect), [docs/threat-model.md](docs/threat-model.md), [docs/assurance-case.md](docs/assurance-case.md),
  [docs/architecture.md](docs/architecture.md).
- Tests are the specification: `go test -race ./...`; the fuzz targets run with `go test -fuzz`; the crash tests kill the program with SIGKILL (`cmd/blobgate/process_test.go`).
- Scanners that the project runs: `govulncheck`, `staticcheck`, `gosec`, Trivy on the image, GitHub secret scanning with push protection, Dependabot, dependency review, OpenSSF Scorecard.
