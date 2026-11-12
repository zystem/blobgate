# Cut a release

How-to page. A release is a tag; the workflow `.github/workflows/release.yml` does the rest.

1. Put the changes of the version in `CHANGELOG.md` under `## [X.Y.Z] - <date>` (the text of that section is the text of the release). Commit and push to `main`; wait for `ci` to be green.
2. Tag and push:

   ```bash
   git tag -a vX.Y.Z -m "blobgate X.Y.Z" && git push origin vX.Y.Z
   ```

   A tag with a suffix (`vX.Y.Z-rc.1`) is a pre-release: the image is tagged with that version only (no `X.Y`, no `latest`) and the release is marked as one.
3. The workflow runs `ci` on the tagged commit, then publishes:
   * the image `ghcr.io/<owner>/blobgate:X.Y.Z` (`X.Y` and `latest` for a version with no suffix), linux/amd64 and linux/arm64, with provenance and an SBOM;
   * the chart `oci://ghcr.io/<owner>/charts/blobgate` version X.Y.Z (appVersion X.Y.Z, so `image.tag` can stay empty);
   * the GitHub release with `blobgate_X.Y.Z_linux_<arch>.tar.gz`, `checksums.txt` and the chart.
4. Check what was published:

   ```bash
   docker run --rm ghcr.io/<owner>/blobgate:X.Y.Z version          # blobgate X.Y.Z
   helm show chart oci://ghcr.io/<owner>/charts/blobgate --version X.Y.Z
   ```

A package that GitHub made for the first time is **private** until its owner makes it public (Packages, the package, Package settings, Change visibility); the workflow cannot do it. A tag is not moved: a release
that is wrong is superseded by the next version.
