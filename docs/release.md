# Cut a release

How-to page. A release is a tag; the workflow `.github/workflows/release.yml` does the rest.

1. Put the changes of the version in `CHANGELOG.md` under `## [X.Y.Z] - <date>` (the text of that section is the text of the release). Commit and push to `main`; wait for `ci` to be green.
2. Tag and push (the tag is signed with your SSH key: `gpg.format ssh`, `user.signingkey` set to your public key and `tag.gpgSign true`; add the key to GitHub as a **signing key**; check with `git tag -v vX.Y.Z`):

   ```bash
   git tag -s vX.Y.Z -m "blobgate X.Y.Z" && git tag -v vX.Y.Z && git push origin vX.Y.Z
   ```

   A tag with a suffix (`vX.Y.Z-rc.1`) is a pre-release: the image is tagged with that version only (no `X.Y`, no `latest`) and the release is marked as one.
3. The workflow runs `ci` on the tagged commit, then publishes:
   * the image `ghcr.io/<owner>/blobgate:X.Y.Z` (`X.Y` and `latest` for a version with no suffix), linux/amd64 and linux/arm64, with provenance and an SBOM;
   * the chart `oci://ghcr.io/<owner>/charts/blobgate` version X.Y.Z (appVersion X.Y.Z, so `image.tag` can stay empty);
   * the GitHub release with `blobgate_X.Y.Z_linux_<arch>.tar.gz`, `checksums.txt`, the bill of materials (`blobgate.spdx.json`) and the chart, each with a `.sigstore.json` (keyless signature);
   * a build provenance attestation for the image, the chart and the files.
4. Also in the commit before the tag: the VEX document (`vex/blobgate.openvex.json`, made as [security-policies.md](security-policies.md#vex-findings-that-do-not-affect-the-program) says: run `govulncheck -format openvex`, keep the statements that are still true), and `docs/supported-versions.md` (the new release is the latest; the ones before it in its minor are replaced; the previous minor gets its end date, 30 days after a new minor).
5. Check what was published, as a user would ([verifying-releases.md](verifying-releases.md)):

   ```bash
   docker run --rm ghcr.io/<owner>/blobgate:X.Y.Z version          # blobgate X.Y.Z
   helm show chart oci://ghcr.io/<owner>/charts/blobgate --version X.Y.Z
   ```

A package that GitHub made for the first time is **private** until its owner makes it public (Packages, the package, Package settings, Change visibility); the workflow cannot do it. A tag is not moved: a release
that is wrong is superseded by the next version.
