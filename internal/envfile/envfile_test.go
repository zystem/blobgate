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
