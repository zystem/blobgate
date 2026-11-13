// Package backup keeps copies of the rqlite database (the whole of it: the module's own tables and the core's) in a set of nodes that the database does not describe, and puts one back into an empty
// rqlite. The module keeps its stores in the database, so a backup cannot depend on them: the nodes of the backup are given in the environment, and a backup is read without any record of the
// database (a manifest next to its blocks says what it is, and is written last: no manifest, no backup).
//
// A backup is the SQLite file that rqlite gives (`GET /db/backup`, the state at one moment), compressed, cut into blocks of 8 MiB, every block sealed with AES-256-GCM (the key is in the environment
// too, and must be kept apart from the database) and written to every node under `_backup/<set>/<id>/<n>`; then `_backup/<set>/<id>.json`. See docs/backup.md.
package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"

	"github.com/zystem/blobgate/internal/store"
)

const (
	// Set is the name of what is backed up; the paths of the backups are under it.
	Set = "rqlite"
	// BlockSize is how much of the compressed file a block holds before it is sealed.
	BlockSize = 8 << 20
	root      = "_backup"
	// DefaultKeep is how many backups are kept.
	DefaultKeep = 7
)

// Config is what is needed to write and read backups.
type Config struct {
	Nodes     []store.NodeSpec
	Secrets   store.Secrets
	MinAcks   int    // nodes that must take every block; 0: all
	Key       string // the text the sealing key is made of; "" only with Plaintext
	Plaintext bool   // no sealing (a backup then holds every secret of the database in the clear)
	Keep      int    // backups kept (DefaultKeep)
	RqliteURL string
	Now       func() time.Time
}

// Block is one block of a backup as it lies on the nodes.
type Block struct {
	SHA256 string `json:"sha256"` // of the block as stored (sealed)
	Size   int64  `json:"size"`
}

// Manifest says what a backup is. It is written after the last block.
type Manifest struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	Set       string    `json:"set"`
	Created   time.Time `json:"created"`
	RawSize   int64     `json:"raw_size"` // the SQLite file, before compression
	Sealed    bool      `json:"sealed"`
	BlockSize int       `json:"block_size"`
	Blocks    []Block   `json:"blocks"`
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Config) keep() int {
	if c.Keep <= 0 {
		return DefaultKeep
	}
	return c.Keep
}

func (c Config) mirror(ctx context.Context) (*store.Mirror, error) {
	if len(c.Nodes) == 0 {
		return nil, errors.New("no backup nodes are set (CINIM_STORAGE_BACKUP_NODES)")
	}
	if c.Key == "" && !c.Plaintext {
		return nil, errors.New("no key for the backups is set (CINIM_STORAGE_BACKUP_KEY); a backup holds every secret of the database, so it is sealed, unless CINIM_STORAGE_BACKUP_PLAINTEXT=1")
	}
	cfg := store.Config{Kind: "mirror", Nodes: c.Nodes, MinAcks: c.MinAcks}
	if why := cfg.Check(c.Secrets); why != "" {
		return nil, errors.New("the backup nodes: " + why)
	}
	return store.Open(ctx, "backup", cfg, c.Secrets)
}

func (c Config) aead() (cipher.AEAD, error) {
	k := sha256.Sum256([]byte("blobgate/backup/v1|" + c.Key))
	b, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func blockPath(id string, n int) string { return fmt.Sprintf("%s/%s/%s/%d", root, Set, id, n) }
func dirPath(id string) string          { return fmt.Sprintf("%s/%s/%s", root, Set, id) }
func manifestPath(id string) string     { return fmt.Sprintf("%s/%s/%s.json", root, Set, id) }
func setDir() string                    { return root + "/" + Set }

func aad(id string, n int) []byte { return []byte(fmt.Sprintf("%s|%d", id, n)) }

func (c Config) seal(ad cipher.AEAD, id string, n int, plain []byte) []byte {
	if c.Plaintext {
		return plain
	}
	nonce := make([]byte, ad.NonceSize())
	_, _ = rand.Read(nonce)
	return ad.Seal(nonce, nonce, plain, aad(id, n))
}

func (c Config) open(ad cipher.AEAD, id string, n int, data []byte) ([]byte, error) {
	if c.Plaintext {
		return data, nil
	}
	if len(data) < ad.NonceSize()+ad.Overhead() {
		return nil, errors.New("a block is too short")
	}
	p, err := ad.Open(nil, data[:ad.NonceSize()], data[ad.NonceSize():], aad(id, n))
	if err != nil {
		return nil, errors.New("a block does not open: the key is not the one it was sealed with, or the block was changed")
	}
	return p, nil
}

// newID makes the id of a backup: the time to the second, then the milliseconds (so that two backups of one second sort in the order they were made), then three random bytes.
func newID(now time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%03d%s", now.UTC().Format("20060102T150405Z"), now.UTC().Nanosecond()/1e6, hex.EncodeToString(b))
}

func idTime(id string) (time.Time, bool) {
	if len(id) < 16 {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102T150405Z", id[:16])
	return t, err == nil
}

func httpDo(ctx context.Context, method, url string, body io.Reader, length int64, ctype string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if length >= 0 {
		req.ContentLength = length
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	return (&http.Client{}).Do(req)
}

// Backup takes the database as it is now and writes it to the nodes. It returns the manifest, which exists on the nodes only when the call succeeds.
func Backup(ctx context.Context, c Config) (Manifest, error) {
	m, err := c.mirror(ctx)
	if err != nil {
		return Manifest{}, err
	}
	var ad cipher.AEAD
	if !c.Plaintext {
		if ad, err = c.aead(); err != nil {
			return Manifest{}, err
		}
	}
	resp, err := httpDo(ctx, http.MethodGet, strings.TrimRight(c.RqliteURL, "/")+"/db/backup", nil, -1, "")
	if err != nil {
		return Manifest{}, fmt.Errorf("rqlite: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Manifest{}, fmt.Errorf("rqlite: HTTP %d on /db/backup", resp.StatusCode)
	}
	now := c.now()
	man := Manifest{Version: 1, ID: newID(now), Set: Set, Created: now.UTC(), Sealed: !c.Plaintext, BlockSize: BlockSize}
	// the file is compressed as it is read, and cut into blocks as it comes out: memory is one block
	pr, pw := io.Pipe()
	var raw int64
	go func() {
		gw := gzip.NewWriter(pw)
		n, err := io.Copy(gw, resp.Body)
		raw = n
		if err == nil {
			err = gw.Close()
		}
		pw.CloseWithError(err)
	}()
	buf := make([]byte, BlockSize)
	for n := 0; ; n++ {
		k, rerr := io.ReadFull(pr, buf)
		if k > 0 {
			sealed := c.seal(ad, man.ID, n, buf[:k])
			res, err := m.Put(ctx, blockPath(man.ID, n), int64(len(sealed)), bytes.NewReader(sealed))
			if err != nil {
				pr.CloseWithError(err)
				cleanup(ctx, m, man.ID)
				return Manifest{}, fmt.Errorf("block %d: %w", n, err)
			}
			man.Blocks = append(man.Blocks, Block{SHA256: res.SHA256, Size: int64(len(sealed))})
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			cleanup(ctx, m, man.ID)
			return Manifest{}, rerr
		}
	}
	man.RawSize = raw
	if len(man.Blocks) == 0 || raw == 0 {
		cleanup(ctx, m, man.ID)
		return Manifest{}, errors.New("rqlite gave an empty backup")
	}
	mj, _ := json.MarshalIndent(man, "", "  ")
	if _, err := m.Put(ctx, manifestPath(man.ID), int64(len(mj)), bytes.NewReader(mj)); err != nil {
		cleanup(ctx, m, man.ID)
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	Prune(ctx, c, m)
	return man, nil
}

func cleanup(ctx context.Context, m *store.Mirror, id string) {
	ctx = context.WithoutCancel(ctx)
	m.DeleteDir(ctx, dirPath(id))
}

// List returns the backups found on the nodes, newest first. A manifest that is on some nodes only is listed once; one that cannot be read is left out.
func List(ctx context.Context, c Config) ([]Manifest, error) {
	m, err := c.mirror(ctx)
	if err != nil {
		return nil, err
	}
	return list(ctx, m)
}

func list(ctx context.Context, m *store.Mirror) ([]Manifest, error) {
	seen := map[string]Manifest{}
	var firstErr error
	reached := 0
	for _, n := range m.Nodes {
		entries, err := n.F.List(ctx, setDir())
		if errors.Is(err, fs.ErrorDirNotFound) {
			reached++
			continue
		}
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("node %s: %w", n.Label(), err)
			}
			continue
		}
		reached++
		for _, e := range entries {
			o, ok := e.(fs.Object)
			if !ok || !strings.HasSuffix(o.Remote(), ".json") {
				continue
			}
			id := strings.TrimSuffix(path.Base(o.Remote()), ".json")
			if _, dup := seen[id]; dup {
				continue
			}
			rc, err := o.Open(ctx)
			if err != nil {
				continue
			}
			data, _ := io.ReadAll(io.LimitReader(rc, 64<<20))
			rc.Close()
			var man Manifest
			if json.Unmarshal(data, &man) == nil && man.ID == id && man.Version == 1 && len(man.Blocks) > 0 {
				seen[id] = man
			}
		}
	}
	if reached == 0 && firstErr != nil {
		return nil, firstErr
	}
	out := make([]Manifest, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// Prune removes the backups beyond the newest Keep (the manifest first, so that a backup that is half removed is not taken for a whole one), and the directories of blocks that have no manifest
// and are more than a day old (a backup that was cut off).
func Prune(ctx context.Context, c Config, m *store.Mirror) {
	ctx = context.WithoutCancel(ctx)
	mans, err := list(ctx, m)
	if err != nil {
		return
	}
	have := map[string]bool{}
	for i, man := range mans {
		have[man.ID] = true
		if i >= c.keep() {
			for _, n := range m.Nodes {
				_ = n.RemoveBlock(ctx, manifestPath(man.ID))
			}
			m.DeleteDir(ctx, dirPath(man.ID))
		}
	}
	for _, n := range m.Nodes {
		entries, err := n.F.List(ctx, setDir())
		if err != nil {
			continue
		}
		for _, e := range entries {
			if _, ok := e.(fs.Directory); !ok {
				continue
			}
			id := path.Base(e.Remote())
			if t, ok := idTime(id); ok && !have[id] && c.now().Sub(t) > 24*time.Hour {
				_ = n.RemoveDir(ctx, dirPath(id))
			}
		}
	}
}

func pick(mans []Manifest, id string) (Manifest, error) {
	if len(mans) == 0 {
		return Manifest{}, errors.New("no backup was found on the nodes")
	}
	if id == "" || id == "latest" {
		return mans[0], nil
	}
	for _, m := range mans {
		if m.ID == id {
			return m, nil
		}
	}
	return Manifest{}, fmt.Errorf("no backup %s on the nodes", id)
}

// readBlock reads block n of a backup from the first node that has it with the digest the manifest names, and opens it.
func (c Config) readBlock(ctx context.Context, m *store.Mirror, ad cipher.AEAD, man Manifest, n int) ([]byte, error) {
	data, err := m.ReadBlock(ctx, blockPath(man.ID, n), man.Blocks[n].SHA256, man.Blocks[n].Size)
	if err != nil {
		return nil, fmt.Errorf("block %d: %w", n, err)
	}
	plain, err := c.open(ad, man.ID, n, data)
	if err != nil {
		return nil, fmt.Errorf("block %d: %w", n, err)
	}
	return plain, nil
}

// Verify reads every block of a backup ("" or "latest": the newest), checks its digest, opens it and decompresses the whole, and returns the manifest. Nothing is changed.
func Verify(ctx context.Context, c Config, id string) (Manifest, error) {
	m, err := c.mirror(ctx)
	if err != nil {
		return Manifest{}, err
	}
	mans, err := list(ctx, m)
	if err != nil {
		return Manifest{}, err
	}
	man, err := pick(mans, id)
	if err != nil {
		return Manifest{}, err
	}
	return verifyManifest(ctx, c, m, man)
}

// reader gives the SQLite file of a backup, block by block, as the blocks are read.
func (c Config) reader(ctx context.Context, m *store.Mirror, man Manifest) (io.Reader, error) {
	var ad cipher.AEAD
	var err error
	if !c.Plaintext {
		if ad, err = c.aead(); err != nil {
			return nil, err
		}
	}
	if man.Sealed == c.Plaintext {
		return nil, fmt.Errorf("the backup is %s and the settings say the opposite (CINIM_STORAGE_BACKUP_PLAINTEXT)", map[bool]string{true: "sealed", false: "not sealed"}[man.Sealed])
	}
	pr, pw := io.Pipe()
	go func() {
		for n := range man.Blocks {
			plain, err := c.readBlock(ctx, m, ad, man, n)
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(plain); err != nil {
				return
			}
		}
		pw.Close()
	}()
	gz, err := gzip.NewReader(pr)
	if err != nil {
		pr.CloseWithError(err)
		return nil, err
	}
	return gz, nil
}

// IsEmpty says whether the database has no table at all (a new one).
func IsEmpty(ctx context.Context, url string) (bool, error) {
	resp, err := httpDo(ctx, http.MethodPost, strings.TrimRight(url, "/")+"/db/query?level=strong", strings.NewReader(`["SELECT count(*) FROM sqlite_master WHERE type='table'"]`), -1, "application/json")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var out struct {
		Results []struct {
			Values [][]float64 `json:"values"`
			Error  string      `json:"error"`
		} `json:"results"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	if out.Error != "" || len(out.Results) != 1 || out.Results[0].Error != "" || len(out.Results[0].Values) != 1 {
		return false, fmt.Errorf("rqlite: %s", out.Error)
	}
	return out.Results[0].Values[0][0] == 0, nil
}

// Result is what VerifyAll says of one backup.
type Result struct {
	Manifest Manifest
	Err      error // nil: it checks out
}

// VerifyAll checks every backup on the nodes, newest first, and says which of them can be read. It is what to run to know which one to restore when the newest is damaged.
func VerifyAll(ctx context.Context, c Config) ([]Result, error) {
	m, err := c.mirror(ctx)
	if err != nil {
		return nil, err
	}
	mans, err := list(ctx, m)
	if err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(mans))
	for _, man := range mans {
		_, verr := verifyManifest(ctx, c, m, man)
		out = append(out, Result{Manifest: man, Err: verr})
	}
	return out, nil
}

func verifyManifest(ctx context.Context, c Config, m *store.Mirror, man Manifest) (Manifest, error) {
	r, err := c.reader(ctx, m, man)
	if err != nil {
		return man, err
	}
	// at most what the manifest says, plus one byte to see that there is more: a block that decompresses to more is damaged, and is not read to the end
	n, err := io.CopyN(io.Discard, r, man.RawSize+1)
	if err != nil && err != io.EOF {
		return man, err
	}
	if n != man.RawSize {
		return man, fmt.Errorf("the backup holds %d bytes, the manifest says %d", n, man.RawSize)
	}
	return man, nil
}

// Options says how a restore is done.
type Options struct {
	ID       string // "" or "latest": the newest
	Force    bool   // into a database that is not empty
	Fallback bool   // when the backup chosen does not check out, take the newest older one that does (without an ID only). The state of the database is then older than the newest backup says: say so.
}

// Restore puts a backup ("" or "latest": the newest) into the database. It refuses a database that is not empty unless force. The backup is read and checked whole before the database is
// touched, so that a backup that cannot be read changes nothing. When the newest does not check out the error names the older ones, so that one can be chosen with an ID (or with Fallback).
func Restore(ctx context.Context, c Config, id string, force bool) (Manifest, error) {
	return RestoreWith(ctx, c, Options{ID: id, Force: force})
}

// RestoreWith is Restore with the options of a restore.
func RestoreWith(ctx context.Context, c Config, o Options) (Manifest, error) {
	m, err := c.mirror(ctx)
	if err != nil {
		return Manifest{}, err
	}
	mans, err := list(ctx, m)
	if err != nil {
		return Manifest{}, err
	}
	man, err := pick(mans, o.ID)
	if err != nil {
		return Manifest{}, err
	}
	if !o.Force {
		empty, err := IsEmpty(ctx, c.RqliteURL)
		if err != nil {
			return man, fmt.Errorf("rqlite: %w", err)
		}
		if !empty {
			return man, errors.New("the database is not empty: a restore replaces all of it; run it with force when that is meant")
		}
	}
	candidates := []Manifest{man}
	if o.Fallback && (o.ID == "" || o.ID == "latest") {
		candidates = mans
	}
	var failed []string
	for _, cand := range candidates {
		if _, err := verifyManifest(ctx, c, m, cand); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", cand.ID, err))
			continue
		}
		return c.load(ctx, m, cand)
	}
	older := ""
	for _, x := range mans {
		if x.ID != man.ID {
			older += " " + x.ID
		}
	}
	hint := ""
	if !o.Fallback && older != "" {
		hint = fmt.Sprintf("; older backups:%s (restore one with -id, or take the newest that checks out with -fallback; `blobgate verify -all` says which can be read)", older)
	}
	return man, fmt.Errorf("the backup does not check out, the database is untouched: %s%s", strings.Join(failed, "; "), hint)
}

// load puts a backup that has been checked into the database.
func (c Config) load(ctx context.Context, m *store.Mirror, man Manifest) (Manifest, error) {
	r, err := c.reader(ctx, m, man)
	if err != nil {
		return man, err
	}
	resp, err := httpDo(ctx, http.MethodPost, strings.TrimRight(c.RqliteURL, "/")+"/db/load", r, man.RawSize, "application/octet-stream")
	if err != nil {
		return man, fmt.Errorf("rqlite: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != 200 {
		return man, fmt.Errorf("rqlite refused the load: HTTP %d %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	return man, nil
}
