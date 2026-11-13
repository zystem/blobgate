// Package store holds the named stores and talks to their nodes through rclone (as a library: no config file, no state on a disk).
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/fspath"
)

// NodeSpec says how to reach one node: the rclone backend, its options (no secrets) and the path inside it.
type NodeSpec struct {
	Backend string            `json:"backend"`
	Options map[string]string `json:"options,omitempty"`
	Path    string            `json:"path,omitempty"`
}

// CurrentLayout is the version of how blocks lie on the nodes (the path of a block, the escaping of a key). A store records the one it was written with, and a program refuses a store with a
// newer one: reading it with an older rule would take the blocks for strays.
const CurrentLayout = 1

// Config is what is kept of a store, without its secrets.
type Config struct {
	Layout   int        `json:"layout,omitempty"` // 0 is 1: the stores written before the field was added
	Kind     string     `json:"kind"`             // "mirror" (rclone nodes) or "s3" (one S3 bucket: a short way to write one node)
	Nodes    []NodeSpec `json:"nodes,omitempty"`
	MinAcks  int        `json:"min_acks,omitempty"` // 0: every node must confirm a write
	SpaceURL string     `json:"space_url,omitempty"`
	// SoftDeleteSeconds: a deleted object is kept this long in the trash (hidden, restorable) before its blocks are removed; 0: removed at once
	SoftDeleteSeconds int        `json:"soft_delete_seconds,omitempty"`
	Encrypt           *EncConfig `json:"encrypt,omitempty"`  // seal the blocks written from now on (what is already written stays as it is and is still read)
	Endpoint          string     `json:"endpoint,omitempty"` // s3
	Region            string     `json:"region,omitempty"`
	Bucket            string     `json:"bucket,omitempty"`
	KeyID             string     `json:"access_key_id,omitempty"`
}

// Secrets are the options that must not be written in the clear: per node for a mirror, the secret key for s3.
type Secrets struct {
	Nodes    []map[string]string `json:"nodes,omitempty"`
	S3Secret string              `json:"s3_secret,omitempty"`
	EncKeys  map[string]string   `json:"enc_keys,omitempty"` // key id -> the text the key is made of; every key an object was sealed with must stay, or the object cannot be read
}

// ValidName says whether this may be the name of a store: lowercase letters, digits and -, starting with a letter, at most 32.
func ValidName(n string) bool {
	if len(n) < 1 || len(n) > 32 || n[0] < 'a' || n[0] > 'z' {
		return false
	}
	for i := 0; i < len(n); i++ {
		c := n[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func printable(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '!' || s[i] > '~' {
			return false
		}
	}
	return true
}

// ParseRemote reads a connection string (`:ftp,host=h,port=21:/path`) into a node spec; the options may not hold secrets the caller means to seal.
func ParseRemote(remote string) (NodeSpec, error) {
	p, err := fspath.Parse(remote)
	if err != nil {
		return NodeSpec{}, err
	}
	if p.Name == "" || p.Name[0] != ':' {
		return NodeSpec{}, errors.New("a node is a connection string that starts with :backend, for example :ftp,host=h:/path")
	}
	spec := NodeSpec{Backend: p.Name[1:], Options: map[string]string{}, Path: p.Path}
	for k, v := range p.Config {
		spec.Options[k] = v
	}
	return spec, nil
}

// Check returns "" if these may be the settings of a store, else what is wrong.
func (c Config) Check(s Secrets) string {
	if c.Layout > CurrentLayout {
		return fmt.Sprintf("layout %d is newer than this program knows (%d)", c.Layout, CurrentLayout)
	}
	if c.Encrypt != nil || len(s.EncKeys) > 0 {
		if c.Encrypt != nil && (c.Encrypt.KeyID == 0 || s.EncKeys[strconv.FormatUint(uint64(c.Encrypt.KeyID), 10)] == "") {
			return "encryption.key_id is the id of a key in encryption.keys (ids are numbers from 1)"
		}
		for id, k := range s.EncKeys {
			if n, err := strconv.ParseUint(id, 10, 32); err != nil || n == 0 {
				return fmt.Sprintf("a key id is a number from 1: %q", id)
			}
			if !printable(k, 16, 256) {
				return "a key is 16..256 printable characters"
			}
		}
	}
	switch c.Kind {
	case "s3":
		if c.SoftDeleteSeconds < 0 || c.SoftDeleteSeconds > 366*24*3600 {
			return "soft_delete_seconds is 0 (removed at once) or up to a year"
		}
		if !(strings.HasPrefix(c.Endpoint, "http://") || strings.HasPrefix(c.Endpoint, "https://")) || strings.Count(strings.TrimPrefix(strings.TrimPrefix(c.Endpoint, "http://"), "https://"), "/") > 0 {
			return "endpoint is scheme://host[:port], http or https, with no path"
		}
		if c.Region == "" || len(c.Region) > 64 {
			return "region is 1..64 characters"
		}
		if len(c.Bucket) < 3 || len(c.Bucket) > 63 {
			return "bucket is 3..63 lowercase letters, digits, - and ."
		}
		if !printable(c.KeyID, 1, 128) {
			return "access_key_id is 1..128 printable characters"
		}
		if !printable(s.S3Secret, 1, 256) {
			return "secret_access_key is 1..256 printable characters"
		}
		if c.SpaceURL != "" && !(strings.HasPrefix(c.SpaceURL, "http://") || strings.HasPrefix(c.SpaceURL, "https://")) {
			return "space_url is an http(s) address that says how much room the store has (Garage: http://host:3903/metrics), or empty"
		}
		return ""
	case "mirror":
		if len(c.Nodes) < 1 || len(c.Nodes) > 8 {
			return "nodes is 1..8 nodes"
		}
		for i, n := range c.Nodes {
			if _, err := fs.Find(n.Backend); n.Backend == "" || err != nil {
				return fmt.Sprintf("node %d: no such backend %q (built in: %s)", i, n.Backend, strings.Join(Backends(), ", "))
			}
		}
		if len(s.Nodes) != 0 && len(s.Nodes) != len(c.Nodes) {
			return "secrets must have one entry for every node"
		}
		if c.SoftDeleteSeconds < 0 || c.SoftDeleteSeconds > 366*24*3600 {
			return "soft_delete_seconds is 0 (removed at once) or up to a year"
		}
		if c.MinAcks < 0 || c.MinAcks > len(c.Nodes) {
			return fmt.Sprintf("min_acks is 0 (every node, the default) or 1..%d", len(c.Nodes))
		}
		return ""
	}
	return "kind is mirror (rclone nodes) or s3 (a bucket)"
}

// Backends lists the rclone backends this program was built with.
func Backends() []string {
	var out []string
	for _, r := range fs.Registry {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

// Specs gives the nodes of the store with their secrets, ready to be opened.
func (c Config) Specs(s Secrets) []NodeSpec {
	if c.Kind == "s3" {
		return []NodeSpec{{Backend: "s3", Path: c.Bucket, Options: map[string]string{
			"provider": "Other", "endpoint": c.Endpoint, "region": c.Region, "access_key_id": c.KeyID, "secret_access_key": s.S3Secret,
			"env_auth": "false", "force_path_style": "true", "no_check_bucket": "false", "chunk_size": "8Mi"}}}
	}
	out := make([]NodeSpec, len(c.Nodes))
	for i, n := range c.Nodes {
		o := map[string]string{}
		for k, v := range n.Options {
			o[k] = v
		}
		if i < len(s.Nodes) {
			for k, v := range s.Nodes[i] {
				o[k] = v
			}
		}
		out[i] = NodeSpec{Backend: n.Backend, Options: o, Path: n.Path}
	}
	return out
}

// backendDefaults are options the module sets for a backend when the store does not: where rclone's own default makes the module slow. The WebDAV backend sleeps 10 ms between any two calls to one
// node, which held every operation on a node to about a hundred calls a second (a write of 64 KiB takes several): measured 23 writes and 46 reads of 64 KiB a second on a node that gave
// a thousand. A store that names the option itself keeps its value (a server that throttles).
var backendDefaults = map[string]map[string]string{
	"webdav": {"pacer_min_sleep": "100us"},
}

func quote(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

// ConnString makes the rclone connection string of a node; the options rclone wants obscured (passwords) are obscured here, so that the clear text never has to be given in another form.
func (n NodeSpec) ConnString() (string, error) {
	reg, err := fs.Find(n.Backend)
	if err != nil {
		return "", fmt.Errorf("no such backend %q", n.Backend)
	}
	pw := map[string]bool{}
	for _, o := range reg.Options {
		if o.IsPassword {
			pw[o.Name] = true
		}
	}
	opts := map[string]string{}
	for k, v := range backendDefaults[n.Backend] {
		opts[k] = v
	}
	for k, v := range n.Options {
		opts[k] = v
	}
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(":" + n.Backend)
	for _, k := range keys {
		v := opts[k]
		if pw[k] && v != "" {
			var err error
			if v, err = obscure.Obscure(v); err != nil {
				return "", err
			}
		}
		b.WriteString("," + k + "=" + quote(v))
	}
	b.WriteString(":" + n.Path)
	return b.String(), nil
}

// ID is a name for the node that does not change while its settings do not: what the repair queue uses (never a secret goes into it: the secrets are not hashed).
func (n NodeSpec) ID() string {
	keys := make([]string, 0, len(n.Options))
	for k := range n.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(n.Backend + "|" + n.Path))
	for _, k := range keys {
		if reg, err := fs.Find(n.Backend); err == nil && isSecret(reg, k) {
			continue
		}
		h.Write([]byte("|" + k + "=" + n.Options[k]))
	}
	return n.Backend + "-" + hex.EncodeToString(h.Sum(nil))[:8]
}

func isSecret(reg *fs.RegInfo, name string) bool {
	for _, o := range reg.Options {
		if o.Name == name && (o.IsPassword || o.Sensitive) {
			return true
		}
	}
	return false
}

// Open makes the Fs of a node.
func (n NodeSpec) Open(ctx context.Context) (fs.Fs, error) {
	cs, err := n.ConnString()
	if err != nil {
		return nil, err
	}
	f, err := fs.NewFs(ctx, cs)
	if err != nil && !errors.Is(err, fs.ErrorIsFile) {
		return nil, err
	}
	return f, nil
}
