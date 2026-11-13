package envfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, mode os.FileMode, content string) string {
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGet(t *testing.T) {
	t.Setenv("X_NAME", "")
	t.Setenv("X_NAME_FILE", "")
	if v, err := Get("X_NAME"); v != "" || err != nil {
		t.Fatalf("nothing set: %q %v", v, err)
	}
	t.Setenv("X_NAME", "from-env")
	if v, _ := Get("X_NAME"); v != "from-env" {
		t.Fatal("the environment")
	}
	t.Setenv("X_NAME", "")
	t.Setenv("X_NAME_FILE", write(t, 0o600, "from-file\n"))
	if v, err := Get("X_NAME"); v != "from-file" || err != nil {
		t.Fatalf("the file: %q %v", v, err)
	}
	t.Setenv("X_NAME_FILE", write(t, 0o400, "a\r\n"))
	if v, _ := Get("X_NAME"); v != "a" {
		t.Fatal("a CRLF end")
	}
	t.Setenv("X_NAME", "both")
	if _, err := Get("X_NAME"); err == nil || !strings.Contains(err.Error(), "both set") {
		t.Fatalf("both: %v", err)
	}
	t.Setenv("X_NAME", "")
	t.Setenv("X_NAME_FILE", write(t, 0o644, "open"))
	if _, err := Get("X_NAME"); err == nil || !strings.Contains(err.Error(), "readable by everyone") {
		t.Fatalf("a file that everyone can read: %v", err)
	}
	t.Setenv("X_NAME_FILE", "/no/such/file")
	if _, err := Get("X_NAME"); err == nil {
		t.Fatal("a file that is not there")
	}
	t.Setenv("X_NAME_FILE", write(t, 0o600, strings.Repeat("x", 1<<20+1)))
	if _, err := Get("X_NAME"); err == nil {
		t.Fatal("a file over 1 MiB")
	}
}

// The settings keep their old names: CINIM_STORAGE_<X> still works for BLOBGATE_<X>, the shards are the tenants, and the new name wins.
func TestLegacyNames(t *testing.T) {
	for new, old := range map[string]string{"BLOBGATE_PORT": "CINIM_STORAGE_PORT", "BLOBGATE_TENANTS": "CINIM_STORAGE_SHARDS", "BLOBGATE_TENANT": "CINIM_SHARD", "BLOBGATE_RQLITE_URL": "CINIM_RQLITE_URL", "BLOBGATE_SEAL_KEY": "CINIM_STORAGE_SEAL_KEY"} {
		t.Setenv(new, "")
		t.Setenv(old, "old-"+old)
		if got := Lookup(new); got != "old-"+old {
			t.Errorf("%s: %q", new, got)
		}
		t.Setenv(new, "new")
		if got := Lookup(new); got != "new" {
			t.Errorf("the new name does not win for %s: %q", new, got)
		}
	}
	if Lookup("OTHER_NAME") != "" || len(legacyNames("OTHER")) != 0 {
		t.Fatal("a name that is not BLOBGATE_ has no legacy")
	}
	// a secret as a file under its old name
	t.Setenv("BLOBGATE_ADMIN_TOKEN", "")
	t.Setenv("CINIM_STORAGE_ADMIN_TOKEN", "")
	t.Setenv("CINIM_STORAGE_ADMIN_TOKEN_FILE", write(t, 0o600, "from-an-old-file\n"))
	if v, err := Get("BLOBGATE_ADMIN_TOKEN"); err != nil || v != "from-an-old-file" {
		t.Fatalf("%q %v", v, err)
	}
}
