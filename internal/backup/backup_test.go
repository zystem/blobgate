package backup

// Tests with a real rqlite of their own (Docker, image `rqlite/rqlite`): the tests wipe the database, which the other tests of the program must not see. BLOBGATE_DOCKER_TESTS=1 runs them.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zystem/blobgate/internal/store"
)

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startRqlite(t *testing.T) string {
	if os.Getenv("BLOBGATE_DOCKER_TESTS") != "1" {
		t.Skip("BLOBGATE_DOCKER_TESTS=1 runs the tests with a real rqlite")
	}
	name := fmt.Sprintf("blobgate-test-rq-%d", os.Getpid())
	port := freePort(t)
	out, err := exec.Command("docker", "run", "-d", "--name", name, "-p", fmt.Sprintf("127.0.0.1:%d:4001", port), "--tmpfs", "/rqlite/file:rw,size=512m,mode=1777", "rqlite/rqlite", "-fk", "-disco-mode=").CombinedOutput()
	if err != nil {
		t.Skipf("docker could not start rqlite: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	for i := 0; i < 60; i++ {
		time.Sleep(500 * time.Millisecond)
		if resp, err := http.Get(url + "/readyz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return url
			}
		}
	}
	t.Fatal("rqlite did not come up")
	return ""
}

func post(t *testing.T, url, path string, body any) []byte {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || strings.Contains(string(data), `"error"`) {
		t.Fatalf("%s: %d %s", path, resp.StatusCode, data)
	}
	return data
}

func fill(t *testing.T, url string, mb int) string {
	post(t, url, "/db/execute", []any{"CREATE TABLE IF NOT EXISTS big (id INTEGER PRIMARY KEY, data TEXT)", "CREATE TABLE IF NOT EXISTS small (k TEXT, v TEXT)", []any{"INSERT INTO small VALUES (?, ?)", "hello", "world"}})
	h := sha256.New()
	for i := 0; i < mb; i++ {
		raw := make([]byte, 768<<10) // random: it does not compress
		_, _ = rand.Read(raw)
		s := base64.StdEncoding.EncodeToString(raw)
		h.Write([]byte(s))
		post(t, url, "/db/execute", []any{[]any{"INSERT INTO big (data) VALUES (?)", s}})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func digest(t *testing.T, url string) string {
	data := post(t, url, "/db/query?level=strong", []any{"SELECT data FROM big ORDER BY id"})
	var out struct {
		Results []struct {
			Values [][]string `json:"values"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	for _, v := range out.Results[0].Values {
		h.Write([]byte(v[0]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func wipe(t *testing.T, url string) {
	data := post(t, url, "/db/query?level=strong", []any{"SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'"})
	var out struct {
		Results []struct {
			Values [][]string `json:"values"`
		} `json:"results"`
	}
	_ = json.Unmarshal(data, &out)
	for _, r := range out.Results[0].Values {
		post(t, url, "/db/execute", []any{"DROP TABLE " + r[0]})
	}
	if empty, err := IsEmpty(context.Background(), url); err != nil || !empty {
		t.Fatalf("not wiped: %v %v", empty, err)
	}
}

type rig struct {
	t     *testing.T
	url   string
	nodes [2]string
	cfg   Config
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, url: startRqlite(t)}
	dir := t.TempDir()
	r.nodes = [2]string{filepath.Join(dir, "n1"), filepath.Join(dir, "n2")}
	r.cfg = Config{RqliteURL: r.url, Key: "a-key-for-the-backup-test", Keep: 3,
		Nodes: []store.NodeSpec{{Backend: "local", Path: r.nodes[0]}, {Backend: "local", Path: r.nodes[1]}}}
	return r
}

func files(dir string) []string {
	var out []string
	_ = filepath.Walk(dir, func(p string, i os.FileInfo, err error) error {
		if err == nil && !i.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// A database is backed up to two nodes, sealed, in blocks, and put back into an empty one: every row is there, and the module's own tables with them.
func TestBackupAndRestoreIntoAnEmptyDatabase(t *testing.T) {
	r := newRig(t)
	want := fill(t, r.url, 14)
	man, err := Backup(context.Background(), r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Blocks) < 2 || !man.Sealed || man.RawSize < 10<<20 {
		t.Fatalf("%+v", man)
	}
	for _, n := range r.nodes {
		f := files(n)
		if len(f) != len(man.Blocks)+1 {
			t.Fatalf("%s holds %d files, want %d blocks and a manifest: %v", n, len(f), len(man.Blocks), f)
		}
	}
	// the sealed blocks do not show the data
	for _, f := range files(r.nodes[0]) {
		data, _ := os.ReadFile(filepath.Join(r.nodes[0], f))
		if bytes.Contains(data, []byte("SQLite format")) {
			t.Fatalf("%s is not sealed", f)
		}
	}
	if _, err := Restore(context.Background(), r.cfg, "", false); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("a restore into a database that is not empty: %v", err)
	}
	wipe(t, r.url)
	got, err := Restore(context.Background(), r.cfg, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != man.ID {
		t.Fatal("not the newest")
	}
	if d := digest(t, r.url); d != want {
		t.Fatalf("the data is not the same after the restore")
	}
	small := post(t, r.url, "/db/query?level=strong", []any{"SELECT v FROM small WHERE k='hello'"})
	if !strings.Contains(string(small), "world") {
		t.Fatalf("%s", small)
	}
	// with force a database that is not empty is replaced
	post(t, r.url, "/db/execute", []any{"DELETE FROM small"})
	if _, err := Restore(context.Background(), r.cfg, man.ID, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(post(t, r.url, "/db/query?level=strong", []any{"SELECT v FROM small"})), "world") {
		t.Fatal("force did not restore")
	}
}

// A block that is damaged on one node is read from the other; damaged on both, the restore stops before it touches the database.
func TestRestoreChecksEveryBlock(t *testing.T) {
	r := newRig(t)
	fill(t, r.url, 10)
	man, err := Backup(context.Background(), r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var blocks []string
	for _, f := range files(r.nodes[0]) {
		if !strings.HasSuffix(f, ".json") {
			blocks = append(blocks, f)
		}
	}
	damage := func(node, rel string) {
		p := filepath.Join(r.nodes[node[0]-'0'], rel)
		data, _ := os.ReadFile(p)
		data[len(data)/2] ^= 0xff
		_ = os.WriteFile(p, data, 0o644)
	}
	r.nodes = [2]string{r.nodes[0], r.nodes[1]}
	damage("0", blocks[len(blocks)-1])
	if _, err := Verify(context.Background(), r.cfg, ""); err != nil {
		t.Fatalf("a block damaged on one node only: %v", err)
	}
	damage("1", blocks[len(blocks)-1])
	wipe(t, r.url)
	_, err = Restore(context.Background(), r.cfg, "", false)
	if err == nil || !strings.Contains(err.Error(), "database is untouched") {
		t.Fatalf("%v", err)
	}
	if empty, _ := IsEmpty(context.Background(), r.url); !empty {
		t.Fatal("a restore that failed changed the database")
	}
	_ = man
}

// The key: a wrong key is refused with words; a backup that is sealed is not read as plain, nor the opposite.
func TestKey(t *testing.T) {
	r := newRig(t)
	fill(t, r.url, 1)
	if _, err := Backup(context.Background(), r.cfg); err != nil {
		t.Fatal(err)
	}
	bad := r.cfg
	bad.Key = "another-key-for-the-backup"
	if _, err := Verify(context.Background(), bad, ""); err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("a wrong key: %v", err)
	}
	plain := r.cfg
	plain.Key, plain.Plaintext = "", true
	if _, err := Verify(context.Background(), plain, ""); err == nil || !strings.Contains(err.Error(), "opposite") {
		t.Fatalf("%v", err)
	}
	none := r.cfg
	none.Key = ""
	if _, err := Backup(context.Background(), none); err == nil || !strings.Contains(err.Error(), "CINIM_STORAGE_BACKUP_KEY") {
		t.Fatalf("no key: %v", err)
	}
	// a plain backup is read plain
	if _, err := Backup(context.Background(), plain); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), plain, ""); err != nil {
		t.Fatal(err)
	}
}

// Only the newest Keep backups stay; a backup with no manifest (cut off) is not one, and its blocks are removed once they are a day old.
func TestPruneAndCutOffBackups(t *testing.T) {
	r := newRig(t)
	fill(t, r.url, 1)
	t0 := time.Now().Add(-72 * time.Hour)
	var ids []string
	for i := 0; i < 5; i++ {
		c := r.cfg
		at := t0.Add(time.Duration(i) * time.Hour)
		c.Now = func() time.Time { return at }
		m, err := Backup(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	mans, _ := List(context.Background(), r.cfg)
	if len(mans) != 3 || mans[0].ID != ids[4] || mans[2].ID != ids[2] {
		t.Fatalf("kept %d: %v", len(mans), mans)
	}
	for _, n := range r.nodes {
		for _, f := range files(n) {
			if strings.Contains(f, ids[0]) || strings.Contains(f, ids[1]) {
				t.Fatalf("the blocks of a pruned backup are left: %s", f)
			}
		}
	}
	// a backup cut off, three days old: the blocks are there, the manifest is not
	cut := "20260101T000000Z-aaaaaa"
	_ = os.MkdirAll(filepath.Join(r.nodes[0], "_backup", Set, cut), 0o755)
	_ = os.WriteFile(filepath.Join(r.nodes[0], "_backup", Set, cut, "0"), []byte("x"), 0o644)
	m, _ := r.cfg.mirror(context.Background())
	Prune(context.Background(), r.cfg, m)
	if _, err := os.Stat(filepath.Join(r.nodes[0], "_backup", Set, cut)); err == nil {
		t.Fatal("the blocks of a backup that was cut off are left")
	}
	// without the manifest of the newest, the one before it is the newest
	_ = os.Remove(filepath.Join(r.nodes[0], "_backup", Set, ids[4]+".json"))
	_ = os.Remove(filepath.Join(r.nodes[1], "_backup", Set, ids[4]+".json"))
	mans, _ = List(context.Background(), r.cfg)
	if mans[0].ID != ids[3] {
		t.Fatalf("a backup with no manifest was taken for a whole one: %v", mans[0].ID)
	}
}

// Nothing on the nodes is no backup, said plainly.
func TestNoBackup(t *testing.T) {
	r := newRig(t)
	if _, err := Restore(context.Background(), r.cfg, "", false); err == nil || !strings.Contains(err.Error(), "no backup was found") {
		t.Fatalf("%v", err)
	}
}
