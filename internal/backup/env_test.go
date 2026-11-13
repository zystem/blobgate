package backup

import (
	"context"
	"strings"
	"testing"
)

func TestFromEnv(t *testing.T) {
	t.Setenv("BLOBGATE_BACKUP_NODES", "")
	if Configured() {
		t.Fatal("configured with no nodes")
	}
	if _, err := FromEnv("http://r"); err == nil {
		t.Fatal("no nodes accepted")
	}
	t.Setenv("BLOBGATE_BACKUP_NODES", `[{"remote":":sftp,host=h,user=u:/b","secrets":{"pass":"x"}},{"backend":"local","path":"/tmp/x","options":{"copy_links":"false"}}]`)
	t.Setenv("BLOBGATE_BACKUP_KEY", "a-key-of-sixteen-chars")
	t.Setenv("BLOBGATE_BACKUP_KEEP", "3")
	t.Setenv("BLOBGATE_BACKUP_MIN_ACKS", "1")
	c, err := FromEnv("http://r")
	if err != nil || !Configured() || len(c.Nodes) != 2 || c.Nodes[0].Backend != "sftp" || c.Nodes[0].Options["host"] != "h" || c.Secrets.Nodes[0]["pass"] != "x" || c.Nodes[1].Path != "/tmp/x" || c.Keep != 3 || c.MinAcks != 1 || c.Plaintext {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("BLOBGATE_BACKUP_PLAINTEXT", "1")
	if c, _ := FromEnv("http://r"); !c.Plaintext {
		t.Fatal("plaintext")
	}
	for name, env := range map[string]map[string]string{
		"not json":          {"BLOBGATE_BACKUP_NODES": "{"},
		"a short key":       {"BLOBGATE_BACKUP_KEY": "short"},
		"keep not a number": {"BLOBGATE_BACKUP_KEEP": "many"},
		"negative acks":     {"BLOBGATE_BACKUP_MIN_ACKS": "-1"},
		"bad remote":        {"BLOBGATE_BACKUP_NODES": `[{"remote":"plain/path"}]`},
	} {
		t.Setenv("BLOBGATE_BACKUP_NODES", `[{"remote":":local:/tmp/x"}]`)
		t.Setenv("BLOBGATE_BACKUP_KEEP", "")
		t.Setenv("BLOBGATE_BACKUP_MIN_ACKS", "")
		t.Setenv("BLOBGATE_BACKUP_KEY", "a-key-of-sixteen-chars")
		for k, v := range env {
			t.Setenv(k, v)
		}
		if _, err := FromEnv("http://r"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_ = strings.TrimSpace
}

func TestMirrorRefusals(t *testing.T) {
	if _, err := (Config{}).mirror(context.Background()); err == nil || !strings.Contains(err.Error(), "BLOBGATE_BACKUP_NODES") {
		t.Fatalf("no nodes: %v", err)
	}
}
