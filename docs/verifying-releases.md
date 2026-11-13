# Verifying a release

How-to page. A release has a binary for linux/amd64 and linux/arm64, an image, a chart, a bill of materials and signatures. Nothing here needs a key of the project: the signatures are **keyless**, bound to the identity of
the release workflow in this repository, and recorded in the public Sigstore log. There is no private key of the project to lose or to publish, and no key to fetch: the public side is the identity named below.

The identity to expect: `https://github.com/zystem/blobgate/.github/workflows/release.yml@refs/tags/vX.Y.Z`, issued by `https://token.actions.githubusercontent.com`.

## The binary

Download `blobgate_X.Y.Z_linux_<arch>.tar.gz`, `checksums.txt` and their `.sigstore.json` from the [release](https://github.com/zystem/blobgate/releases), then:

```bash
sha256sum -c checksums.txt --ignore-missing
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/zystem/blobgate/.github/workflows/release.yml@refs/tags/vX.Y.Z" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
gh attestation verify blobgate_X.Y.Z_linux_amd64.tar.gz -R zystem/blobgate
```

The first line checks the files against the list, the second that the list was signed by the workflow, the third the build provenance (which commit, which workflow).

## The image and the chart

```bash
cosign verify ghcr.io/zystem/blobgate:X.Y.Z \
  --certificate-identity "https://github.com/zystem/blobgate/.github/workflows/release.yml@refs/tags/vX.Y.Z" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/zystem/blobgate:X.Y.Z -R zystem/blobgate
```

The chart is `oci://ghcr.io/zystem/charts/blobgate`; verify it the same way with `cosign verify ghcr.io/zystem/charts/blobgate:X.Y.Z`.

## The tag

Tags are signed with the SSH key of the maintainer who made them, and show "Verified" on GitHub. To check one locally, put the signing keys in an allowed-signers file (the public keys of the maintainers are on their
GitHub accounts: `https://github.com/<login>.keys` for the keys, and the signing keys under Settings) and run `git -c gpg.ssh.allowedSignersFile=FILE tag -v vX.Y.Z`. The maintainers are listed in
[GOVERNANCE.md](../GOVERNANCE.md).

## The build is repeatable

The same source with the same Go version gives the same bytes: `CGO_ENABLED=0 go build -trimpath -buildmode=pie -ldflags="-s -w -X main.version=X.Y.Z" ./cmd/blobgate` twice gives one SHA-256, and CI checks it on every change
(`reproducible` in `ci.yml`). Build it yourself with the Go version in `go.mod` and compare with the binary in the release.

## What was run

The repeatable build and the checks of the workflows: run in CI. The commands above: run against the release X.Y.Z that follows the one with the signatures (the first release, 0.1.0, was made before the signatures were
added and has the checksums only).
