# Roadmap (November 2026 to November 2027)

What the project intends to do and not do in the next year. It is a plan of two maintainers, not a promise; items move when a user's need or a security problem asks for it. Done work is in [CHANGELOG.md](../CHANGELOG.md).

## Intends to do

1. **Use in production by CiNim and fix what it shows**: the module has been run against real SFTP, WebDAV, FTP and Garage servers in tests; a cluster and a long run (a soak of days) will show what the tests did not.
2. **Memory of a read**: a read holds a whole block (75 MB measured against 47 MB of the module it replaces, [docs/benchmark.md](benchmark.md)); find out whether streaming with a check at the end of the block is acceptable
   to the callers, and if it is, stream.
3. **More kinds of node under test**: S3 services other than Garage, FTP servers that lack `SIZE` or `REST`, a server that folds the case of names, a server that is slow; and `kill -9` against each kind.
4. **Supply chain**: keep the releases signed (keyless, with build provenance and a bill of materials), the build repeatable bit for bit, the dependencies current (Dependabot), CI green; keep the VEX document (`vex/blobgate.openvex.json`) current: a finding that does
   not affect the program is a statement in it.
5. **Quality**: keep statement coverage at 80% or more and measured in CI; a fuzz target for every new parser; a regression test for every fixed bug.
6. **Documentation**: keep the reference, the how-to pages and the explanations in step with the code; each claim says what was run.
7. **Badges**: the OpenSSF Best Practices badge at "passing" and "silver".
8. **An organisation with two owners** that holds the repository and the packages, so that the settings do not rest on one account. It changes the address of the repository and of the image, so it is done with notice.

## Considers (no date)

- Deduplication below the object (the same block in different objects, blocks cut by content): the whole-object kind (0.5.0) and the check before the write (`?link`, 0.6.0) are built, and the owner has said that
  below the object is not wanted.
- In the S3 door: CopyObject (by `?link` when the bytes are kept), ListMultipartUploads, an ETag that is the MD5 of a multipart object too.
- Packing small blocks into larger files for nodes that are slow with many small files (FTP).
- A bundled node (Garage) in the chart.

## Does not intend to do

- To be a full S3 server: the S3 door (0.6.0) answers the calls that tools make for objects and uploads; versions, ACLs, policies, lifecycle, notifications and virtual-hosted addresses are not intended.
- Multipart parts of any size in the S3 door: a part is at most 8 MiB and every part but the last is as long as the first, to keep the memory of a part bounded (the owner's decision, 2026-11-14); a tool that sends 16 MiB parts is told to set 8 MiB.
- Repository formats (Maven, npm, OCI): a separate service on top of this one, if ever.
- A web interface.
