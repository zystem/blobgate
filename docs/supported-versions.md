# Supported versions

The rule is in [SECURITY.md](../SECURITY.md), "What is supported": the **latest release** gets every security fix; the **previous minor release** gets fixes for High and Critical problems for 30 days after the new
minor release; anything older gets none. A new patch release (0.2.1 after 0.2.0) replaces the one before it: update.

| Release | Date | Status |
|---|---|---|
| **0.5.0** | 2026-11-14 | supported: the latest |
| 0.4.0 | 2026-11-13 | previous minor: High and Critical fixes only, **until 2026-12-14** |
| 0.3.0 | 2026-11-13 | not supported: update |
| 0.2.0 | 2026-11-13 | not supported: update |
| 0.1.0 | 2026-11-13 | not supported: built with Go 1.26.0, which has GO-2026-6859 (net/http): update |

How to read it: while the project is 0.x a minor release may change a setting or a format; [upgrading.md](upgrading.md) lists what each one changed. What is stored (the rows in rqlite, the layout of blocks on the nodes,
the format of a sealed secret, of a sealed block and of a backup) carries a version, and a program refuses a store that a newer one wrote.

The chart has the version of the release it was made with. Releases, their signatures and their bill of materials: [GitHub releases](https://github.com/zystem/blobgate/releases),
[verifying-releases.md](verifying-releases.md).

This page is updated by each release ([release.md](release.md)).
