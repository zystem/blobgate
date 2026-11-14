# Roadmap (November 2026 to November 2027)

What the project intends to do and not do in the next year. It is a plan of two maintainers, not a promise; items move when a user's need or a security problem asks for it. Done work is in [CHANGELOG.md](../CHANGELOG.md).

## Intends to do

1. **Use in production by CiNim and fix what it shows**: the module has been run against real SFTP, WebDAV, FTP and Garage servers in tests; a cluster and a long run (a soak of days) will show what the tests did not.
2. **Memory of a read**: a read holds a whole block (75 MB measured against 47 MB of the module it replaces, [docs/benchmark.md](benchmark.md)); find out whether streaming with a check at the end of the block is acceptable
   to the callers, and if it is, stream.
3. **More kinds of node under test**: S3 services other than Garage, FTP servers that lack `SIZE` or `REST`, a server that folds the case of names, a server that is slow; and `kill -9` against each kind.
4. **Supply chain**: keep the releases signed (keyless, with build provenance and a bill of materials), the build repeatable bit for bit, the dependencies current (Dependabot), CI green; add a VEX statement when a finding
   does not affect the program.
5. **Quality**: keep statement coverage at 80% or more and measured in CI; a fuzz target for every new parser; a regression test for every fixed bug.
6. **Documentation**: keep the reference, the how-to pages and the explanations in step with the code; each claim says what was run.
7. **Badges**: the OpenSSF Best Practices badge at "passing" and "silver".
8. **An organisation with two owners** that holds the repository and the packages, so that the settings do not rest on one account. It changes the address of the repository and of the image, so it is done with notice.

## Considers (no date)

- Deduplication below the object (the same block in different objects, blocks cut by content), and a check before the write so that a copy is not sent at all: the whole-object kind is built (0.5.0, docs/deduplication.md).
- Packing small blocks into larger files for nodes that are slow with many small files (FTP).
- A bundled node (Garage) in the chart.

## Does not intend to do

- To be a general S3 server (the listing of a tenant's keys is built, 0.4.0; the S3 wire protocol is not).
- Repository formats (Maven, npm, OCI): a separate service on top of this one, if ever.
- A web interface.
