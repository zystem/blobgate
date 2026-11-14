package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/auth"
	"github.com/zystem/blobgate/internal/db"
	"github.com/zystem/blobgate/internal/maint"
	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/seal"
	"github.com/zystem/blobgate/internal/store"
)

// capture runs f with the standard output (and error) of the process redirected, and returns what was printed and what f returned.
func capture(t *testing.T, f func() int) (string, int) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = w, w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	code := f()
	_ = w.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	return <-done, code
}

func TestTokenCommand(t *testing.T) {
	out, code := capture(t, func() int {
		return tokenCommand([]string{"001", "a-secret-of-at-least-16-chars", "put", "/v1/stores/_default/objects/a/b"})
	})
	tok := strings.TrimSpace(out)
	if code != 0 || !auth.VerifyTarget("a-secret-of-at-least-16-chars", "001", "PUT", "/v1/stores/_default/objects/a/b", tok, time.Now().Unix()) {
		t.Fatalf("%d %q", code, out)
	}
	if out, code := capture(t, func() int { return tokenCommand([]string{"001"}) }); code != 2 || !strings.Contains(out, "usage: blobgate token") {
		t.Fatalf("usage: %d %q", code, out)
	}
}

func TestValidateCommand(t *testing.T) {
	dir := t.TempDir()
	out, code := capture(t, func() int { return validate([]string{":local:" + dir}) })
	if code != 0 || !strings.Contains(out, "the node can be used") || !strings.Contains(out, "ok    write into directories that do not exist") {
		t.Fatalf("%d %s", code, out)
	}
	out, code = capture(t, func() int { return validate([]string{"-json", ":local:" + dir}) })
	var v struct {
		Usable bool `json:"usable"`
		Checks []struct {
			Name string
			OK   bool
		}
	}
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || !v.Usable || len(v.Checks) < 8 {
		t.Fatalf("json: %d %s", code, out)
	}
	// a node that cannot be written
	ro := filepath.Join(dir, "ro")
	_ = os.Mkdir(ro, 0o500)
	t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
	if out, code := capture(t, func() int { return validate([]string{":local:" + ro}) }); code != 1 || !strings.Contains(out, "FAIL") || !strings.Contains(out, "cannot be used") {
		t.Fatalf("a node that cannot be written: %d %s", code, out)
	}
	for name, args := range map[string][]string{"no argument": {}, "unknown flag": {"-x", ":local:/tmp"}, "-secret with no value": {"-secret"}, "secret without =": {"-secret", "nopair", ":local:/tmp"}, "not a connection string": {"plain/path"}} {
		if out, code := capture(t, func() int { return validate(args) }); code != 2 {
			t.Errorf("%s: %d %s", name, code, out)
		}
	}
	if out, code := capture(t, func() int { return validate([]string{":nosuchbackend:/x"}) }); code != 1 && code != 2 {
		t.Errorf("an unknown backend: %d %s", code, out)
	}
}

// The commands that work with the backups of the database, through their entry point, against a real rqlite.
func TestBackupCommands(t *testing.T) {
	rqURL := startRqlite(t)
	dir := t.TempDir()
	t.Setenv("BLOBGATE_RQLITE_URL", rqURL)
	t.Setenv("BLOBGATE_BACKUP_NODES", `[{"remote":":local:`+filepath.Join(dir, "n1")+`"},{"remote":":local:`+filepath.Join(dir, "n2")+`"}]`)
	t.Setenv("BLOBGATE_BACKUP_KEY", "a-key-for-the-command-test")
	c := rq.New(rqURL)
	ctx := context.Background()
	if _, err := c.Exec(ctx, false, rq.S("CREATE TABLE zz (a TEXT)"), rq.S("INSERT INTO zz VALUES ('kept')")); err != nil {
		t.Fatal(err)
	}
	if out, code := capture(t, func() int { return backupCommand("backups", nil) }); code != 0 || !strings.Contains(out, "no backups") {
		t.Fatalf("no backups: %d %s", code, out)
	}
	out, code := capture(t, func() int { return backupCommand("backup", nil) })
	if code != 0 || !strings.Contains(out, "sealed=true") {
		t.Fatalf("backup: %d %s", code, out)
	}
	id := strings.Fields(out)[0]
	if out, code := capture(t, func() int { return backupCommand("backups", []string{"-json"}) }); code != 0 || !strings.Contains(out, id) {
		t.Fatalf("backups -json: %d %s", code, out)
	}
	if out, code := capture(t, func() int { return backupCommand("verify", nil) }); code != 0 || !strings.HasPrefix(out, "ok") {
		t.Fatalf("verify: %d %s", code, out)
	}
	if out, code := capture(t, func() int { return backupCommand("verify", []string{"-all"}) }); code != 0 || !strings.Contains(out, id) {
		t.Fatalf("verify -all: %d %s", code, out)
	}
	if out, code := capture(t, func() int { return backupCommand("verify", []string{"-id", "nosuch"}) }); code != 1 || !strings.Contains(out, "no backup nosuch") {
		t.Fatalf("verify of an id that is not there: %d %s", code, out)
	}
	if out, code := capture(t, func() int { return backupCommand("restore", nil) }); code != 1 || !strings.Contains(out, "not empty") {
		t.Fatalf("restore into a database that is not empty: %d %s", code, out)
	}
	if _, err := c.Exec(ctx, false, rq.S("DELETE FROM zz")); err != nil {
		t.Fatal(err)
	}
	if out, code := capture(t, func() int { return backupCommand("restore", []string{"-force", "-id", id}) }); code != 0 || !strings.HasPrefix(out, "restored") {
		t.Fatalf("restore -force: %d %s", code, out)
	}
	if rows, _ := c.Query(ctx, rq.S("SELECT a FROM zz")); len(rows) != 1 || rq.Str(rows[0][0]) != "kept" {
		t.Fatalf("the data is not back: %v", rows)
	}
	// the settings: a missing url, missing nodes, a bad flag
	t.Setenv("BLOBGATE_RQLITE_URL", "")
	if _, code := capture(t, func() int { return backupCommand("backup", nil) }); code != 2 {
		t.Fatal("no url")
	}
	t.Setenv("BLOBGATE_RQLITE_URL", rqURL)
	t.Setenv("BLOBGATE_BACKUP_NODES", "")
	if _, code := capture(t, func() int { return backupCommand("backup", nil) }); code != 2 {
		t.Fatal("no nodes")
	}
	if _, code := capture(t, func() int { return backupCommand("backup", []string{"-nosuchflag"}) }); code != 2 {
		t.Fatal("a flag that is not there")
	}
}

// At start, an empty database is filled from the newest backup when the module is told to; with no backup it starts empty; the loop of backups writes one under the lease.
func TestRestoreOnEmptyAndBackupLoop(t *testing.T) {
	rqURL := startRqlite(t)
	dir := t.TempDir()
	t.Setenv("BLOBGATE_RQLITE_URL", rqURL)
	t.Setenv("BLOBGATE_BACKUP_NODES", `[{"remote":":local:`+filepath.Join(dir, "n1")+`"}]`)
	t.Setenv("BLOBGATE_BACKUP_KEY", "a-key-for-the-loop-test")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	c := rq.New(rqURL)

	restoreIfEmpty(ctx, rqURL, log) // empty database, no backup: starts empty
	if empty, _ := isEmpty(ctx, c); !empty {
		t.Fatal("something was made by a restore that had nothing to restore")
	}
	if err := db.Migrate(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, false, rq.S("INSERT INTO storage_lease (name, holder, until) VALUES ('marker', 'was-here', 1)")); err != nil {
		t.Fatal(err)
	}
	reg := store.NewRegistry(c, seal.Key("a-seal-key-for-the-loop-test"))
	w := maint.New(maint.Config{HolderID: "loop"}, c, reg)
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { backupLoop(cctx, w, rqURL, 300*time.Millisecond, log); close(done) }()
	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done
	if out, code := capture(t, func() int { return backupCommand("backups", []string{"-json"}) }); code != 0 || !strings.Contains(out, `"blocks"`) {
		t.Fatalf("the loop wrote no backup: %d %s", code, out)
	}
	// the database is lost; the module starts and fills it
	for _, tb := range []string{"storage_stores", "storage_objects", "storage_repair", "storage_parts", "storage_uploads", "storage_lease", "storage_quotas", "storage_sets"} {
		_, _ = c.Exec(ctx, false, rq.S("DROP TABLE "+tb))
	}
	restoreIfEmpty(ctx, rqURL, log)
	rows, err := c.Query(ctx, rq.S("SELECT holder FROM storage_lease WHERE name = 'marker'"))
	if err != nil || len(rows) != 1 || rq.Str(rows[0][0]) != "was-here" {
		t.Fatalf("the row that was in the backup is not back: %v %v", rows, err)
	}
	restoreIfEmpty(ctx, rqURL, log) // not empty any more: nothing is done
}

func isEmpty(ctx context.Context, c *rq.Client) (bool, error) {
	rows, err := c.Query(ctx, rq.S("SELECT count(*) FROM sqlite_master WHERE type='table'"))
	return err == nil && len(rows) == 1 && rq.Int(rows[0][0]) == 0, err
}

func TestEnvInt(t *testing.T) {
	t.Setenv("BLOBGATE_TEST_N", "42")
	if envInt("BLOBGATE_TEST_N", 7) != 42 || envInt("BLOBGATE_TEST_MISSING", 7) != 7 {
		t.Fatal("envInt")
	}
	_ = bytes.MinRead
}
