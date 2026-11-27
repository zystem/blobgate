# Supported versions

The rule is in [SECURITY.md](../SECURITY.md), "What is supported": only the **latest release** is supported, and every security fix is made in a new release. Older releases get no fixes, whatever their number: update.
Since 0.12.0; before it the previous minor release was said to get High and Critical fixes for 30 days, which ends with this change (0.10.0 had been given until 2026-12-19, and 0.11 gets no such window).

| Release | Date | Status |
|---|---|---|
| **0.13.1** | 2026-11-28 | supported: the latest |
| 0.13.0 | 2026-11-27 | not supported: update to 0.13.1 |
| 0.12.0 | 2026-11-26 | not supported: update to 0.13.1 |
| 0.11.6 | 2026-11-21 | not supported: update to 0.13.1 |
| 0.11.5 | 2026-11-20 | not supported: update to 0.13.1 |
| 0.11.4 | 2026-11-20 | not supported: update to 0.13.1 |
| 0.11.3 | 2026-11-20 | not supported: update to 0.13.1 |
| 0.11.2 | 2026-11-20 | not supported: update to 0.13.1 |
| 0.11.1 | 2026-11-19 | not supported: update to 0.13.1 |
| 0.11.0 | 2026-11-19 | not supported: update to 0.13.1 |
| 0.10.0 | 2026-11-18 | not supported: update to 0.13.1 |
| 0.9.0 | 2026-11-18 | not supported (since 2026-11-19) |
| 0.8.0 | 2026-11-18 | not supported: update |
| 0.7.0 | 2026-11-17 | not supported: update |
| 0.6.1 | 2026-11-16 | not supported: update |
| 0.6.0 | 2026-11-14 | not supported: update to 0.6.1 or 0.7.0 |
| 0.5.0 | 2026-11-14 | not supported: update |
| 0.4.0 | 2026-11-13 | not supported: update |
| 0.3.0 | 2026-11-13 | not supported: update |
| 0.2.0 | 2026-11-13 | not supported: update |
| 0.1.0 | 2026-11-13 | not supported: built with Go 1.26.0, which has GO-2026-6859 (net/http): update |

How to read it: while the project is 0.x a minor release may change a setting or a format; [upgrading.md](upgrading.md) lists what each one changed. What is stored (the rows in rqlite, the layout of blocks on the nodes,
the format of a sealed secret, of a sealed block and of a backup) carries a version, and a program refuses a store that a newer one wrote.

The chart has the version of the release it was made with. Releases, their signatures and their bill of materials: [GitHub releases](https://github.com/zystem/blobgate/releases),
[verifying-releases.md](verifying-releases.md).

This page is updated by each release ([release.md](release.md)).
