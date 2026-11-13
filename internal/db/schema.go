// Package db holds the tables of the module (docs/storage-module.md): all `storage_*`, so that the module never touches a table of the core and the core never touches these.
package db

import (
	"context"
	"fmt"

	"github.com/zystem/blobgate/internal/rq"
)

var ddl = []string{
	// the named stores; the secrets (a JSON of the options of each node that rclone wants obscured) are sealed with the module's key, the name of the store as additional data
	`CREATE TABLE IF NOT EXISTS storage_stores (
       name TEXT PRIMARY KEY, kind TEXT NOT NULL, config TEXT NOT NULL, secret TEXT NOT NULL,
       is_default INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
	// every object the module fixed: one row, the generation and the SHA-256 of every block (a JSON list). The row is the commit: blocks of another generation are not part of the object.
	`CREATE TABLE IF NOT EXISTS storage_objects (
       store TEXT NOT NULL, key TEXT NOT NULL, size INTEGER NOT NULL, written_at INTEGER NOT NULL, PRIMARY KEY (store, key))`,
	// what a node of a mirror has to be given (`copy`) or to lose (`delete`, `purge`) for a path; the last operation on a path and a node wins
	`CREATE TABLE IF NOT EXISTS storage_repair (
       store TEXT NOT NULL, key TEXT NOT NULL, node TEXT NOT NULL, op TEXT NOT NULL, since INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
       next_at INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', PRIMARY KEY (store, key, node))`,
	`CREATE INDEX IF NOT EXISTS storage_repair_due ON storage_repair (next_at)`,
	// the blocks of an upload as the module received them: the digest it made of the bytes and their number; the core compares them with what the shim reported before it completes
	`CREATE TABLE IF NOT EXISTS storage_parts (
       store TEXT NOT NULL, key TEXT NOT NULL, upload TEXT NOT NULL, part INTEGER NOT NULL, sha256 TEXT NOT NULL, size INTEGER NOT NULL, written_at INTEGER NOT NULL,
       PRIMARY KEY (store, key, upload, part))`,
	// an upload that has begun and not been completed or aborted: what the sweep of orphan blocks looks at
	`CREATE TABLE IF NOT EXISTS storage_uploads (
       store TEXT NOT NULL, key TEXT NOT NULL, upload TEXT NOT NULL, created_at INTEGER NOT NULL, PRIMARY KEY (store, key, upload))`,
	// a job that only one replica of the module does at a time
	`CREATE TABLE IF NOT EXISTS storage_lease (name TEXT PRIMARY KEY, holder TEXT NOT NULL, until INTEGER NOT NULL)`,
}

// columns added to a table that an earlier version made: (table, column, definition)
var columns = [][3]string{
	{"storage_objects", "generation", "TEXT NOT NULL DEFAULT ''"},
	{"storage_objects", "block_size", "INTEGER NOT NULL DEFAULT 0"},
	{"storage_objects", "blocks", "TEXT NOT NULL DEFAULT '[]'"},
	{"storage_objects", "enc", "INTEGER NOT NULL DEFAULT 0"},        // the id of the key the blocks are sealed with; 0: not sealed
	{"storage_objects", "content_type", "TEXT NOT NULL DEFAULT ''"}, // as the writer named it
	{"storage_objects", "meta", "TEXT NOT NULL DEFAULT '{}'"},       // the metadata the writer gave (a JSON object of text)
	{"storage_uploads", "content_type", "TEXT NOT NULL DEFAULT ''"},
	{"storage_uploads", "meta", "TEXT NOT NULL DEFAULT '{}'"},
	{"storage_parts", "stored_sha256", "TEXT NOT NULL DEFAULT ''"}, // of the block as stored (sealed); sha256 stays that of what the shim sent, which the core compares
	{"storage_parts", "stored_size", "INTEGER NOT NULL DEFAULT 0"},
	{"storage_parts", "enc", "INTEGER NOT NULL DEFAULT 0"},
}

// Migrate makes the tables and adds the columns that are missing; it can be run any number of times.
func Migrate(ctx context.Context, c *rq.Client) error {
	stmts := make([]rq.Stmt, len(ddl))
	for i, s := range ddl {
		stmts[i] = rq.S(s)
	}
	if _, err := c.Exec(ctx, true, stmts...); err != nil {
		return err
	}
	for _, col := range columns {
		rows, err := c.Query(ctx, rq.S(fmt.Sprintf("SELECT count(*) FROM pragma_table_info('%s') WHERE name = ?", col[0]), col[1]))
		if err != nil {
			return err
		}
		if len(rows) == 1 && rq.Int(rows[0][0]) == 0 {
			if _, err := c.Exec(ctx, false, rq.S(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", col[0], col[1], col[2]))); err != nil {
				return err
			}
		}
	}
	return nil
}
