# Upgrading

How-to page. Every release says in `CHANGELOG.md` what it changes; this page is the way to do an upgrade and what to check.

1. Read the changelog from your version to the new one. While the major version is 0, a minor release may change a setting, an environment variable or a stored format; the changelog says which, and what to do.
2. Take a backup of the database first ([backup.md](backup.md)): `blobgate backup`, then `blobgate verify`.
3. Upgrade one replica and see `/readyz` answer; then the others. The module migrates its tables itself at start (`CREATE TABLE IF NOT EXISTS`, and columns added when they are missing); a table is never dropped or
   rewritten by an upgrade.
4. Check `blobgate_repair_oldest_age_seconds` and `blobgate_node_errors_total` ([metrics.md](metrics.md)) for a few minutes.

What is kept across releases:

* the rows of rqlite: a newer program reads what an older one wrote (the migration adds the columns); an older program reading what a newer one wrote was not tested, which is one more reason not to downgrade;
* the layout of blocks on the nodes (`store.CurrentLayout`): a store records the layout it was written with, and a program refuses a store written with a newer one (`500 newer_layout`, with the words to update);
* the format of a sealed secret (`v2.`), of a sealed block (a version byte) and of a backup (`version` in its manifest).

A downgrade is not supported: a newer release may have added what an older one does not read (restore the backup taken before the upgrade, with the older program, if a downgrade is needed).

**0.1.0** is the first release; there is nothing to upgrade from. The Nim module that this one replaces in CiNim used other tables for objects (`storage_objects` without generations); the stores start empty, as decided
for CiNim (D-60), and nothing is read from the old layout.
