package backup

import (
	"strings"
	"testing"
)

func TestFromEnv(t *testing.T) {
	t.Setenv("CINIM_STORAGE_BACKUP_NODES", "")
	if Configured() {
		t.Fatal("configured with no nodes")
	}
	if _, err := FromEnv("http://r"); err == nil {
		t.Fatal("no nodes accepted")
	}
	t.Setenv("CINIM_STORAGE_BACKUP_NODES", `[{"remote":":sftp,host=h,user=u:/b","secrets":{"pass":"x"}},{"backend":"local","path":"/tmp/x","options":{"copy_links":"false"}}]`)
	t.Setenv("CINIM_STORAGE_BACKUP_KEY", "a-key-of-sixteen-chars")
	t.Setenv("CINIM_STORAGE_BACKUP_KEEP", "3")
	t.Setenv("CINIM_STORAGE_BACKUP_MIN_ACKS", "1")
	c, err := FromEnv("http://r")
	if err != nil || !Configured() || len(c.Nodes) != 2 || c.Nodes[0].Backend != "sftp" || c.Nodes[0].Options["host"] != "h" || c.Secrets.Nodes[0]["pass"] != "x" || c.Nodes[1].Path != "/tmp/x" || c.Keep != 3 || c.MinAcks != 1 || c.Plaintext {
		t.Fatalf("%+v %v", c, err)
	}
	t.Setenv("CINIM_STORAGE_BACKUP_PLAINTEXT", "1")
	if c, _ := FromEnv("http://r"); !c.Plaintext {
		t.Fatal("plaintext")
	}
	for name, env := range map[string]map[string]string{
		"not json":          {"CINIM_STORAGE_BACKUP_NODES": "{"},
		"a short key":       {"CINIM_STORAGE_BACKUP_KEY": "short"},
		"keep not a number": {"CINIM_STORAGE_BACKUP_KEEP": "many"},
		"negative acks":     {"CINIM_STORAGE_BACKUP_MIN_ACKS": "-1"},
		"bad remote":        {"CINIM_STORAGE_BACKUP_NODES": `[{"remote":"plain/path"}]`},
	} {
		t.Setenv("CINIM_STORAGE_BACKUP_NODES", `[{"remote":":local:/tmp/x"}]`)
		t.Setenv("CINIM_STORAGE_BACKUP_KEEP", "")
		t.Setenv("CINIM_STORAGE_BACKUP_MIN_ACKS", "")
		t.Setenv("CINIM_STORAGE_BACKUP_KEY", "a-key-of-sixteen-chars")
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
	if _, err := (Config{}).mirror(nil); err == nil || !strings.Contains(err.Error(), "CINIM_STORAGE_BACKUP_NODES") {
		t.Fatalf("no nodes: %v", err)
	}
}
