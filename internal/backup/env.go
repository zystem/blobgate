package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/zystem/blobgate/internal/envfile"
	"github.com/zystem/blobgate/internal/store"
)

type nodeEnv struct {
	Remote  string            `json:"remote"`
	Backend string            `json:"backend"`
	Options map[string]string `json:"options"`
	Path    string            `json:"path"`
	Secrets map[string]string `json:"secrets"`
}

// Configured says whether the environment names backup nodes at all.
func Configured() bool {
	v, err := envfile.Get("CINIM_STORAGE_BACKUP_NODES")
	return err != nil || v != ""
}

// FromEnv reads the settings of the backups from the environment:
//
//	CINIM_STORAGE_BACKUP_NODES      a JSON list of nodes: [{"remote": ":sftp,host=h,user=u:/backups", "secrets": {"pass": "..."}}, ...] (the same node as in a store)
//	CINIM_STORAGE_BACKUP_KEY        the text the sealing key is made of (at least 16 characters; keep it apart from the database and from the nodes)
//	CINIM_STORAGE_BACKUP_PLAINTEXT  1: no sealing (not advised: a backup holds every secret of the database)
//	CINIM_STORAGE_BACKUP_KEEP       how many backups are kept (7)
//	CINIM_STORAGE_BACKUP_MIN_ACKS   nodes that must take every block (0: all)
func FromEnv(rqliteURL string) (Config, error) {
	raw, err := envfile.Get("CINIM_STORAGE_BACKUP_NODES")
	if err != nil {
		return Config{}, err
	}
	key, err := envfile.Get("CINIM_STORAGE_BACKUP_KEY")
	if err != nil {
		return Config{}, err
	}
	if raw == "" {
		return Config{}, errors.New("CINIM_STORAGE_BACKUP_NODES is not set")
	}
	var nodes []nodeEnv
	if err := json.Unmarshal([]byte(raw), &nodes); err != nil {
		return Config{}, fmt.Errorf("CINIM_STORAGE_BACKUP_NODES is not a JSON list of nodes: %w", err)
	}
	c := Config{RqliteURL: rqliteURL, Key: key, Plaintext: os.Getenv("CINIM_STORAGE_BACKUP_PLAINTEXT") == "1"}
	for _, n := range nodes {
		spec := store.NodeSpec{Backend: n.Backend, Options: n.Options, Path: n.Path}
		if n.Remote != "" {
			var err error
			if spec, err = store.ParseRemote(n.Remote); err != nil {
				return Config{}, err
			}
			for k, v := range n.Options {
				spec.Options[k] = v
			}
		}
		c.Nodes = append(c.Nodes, spec)
		c.Secrets.Nodes = append(c.Secrets.Nodes, n.Secrets)
	}
	if c.Key != "" && len(c.Key) < 16 {
		return Config{}, errors.New("CINIM_STORAGE_BACKUP_KEY is shorter than 16 characters")
	}
	for name, dst := range map[string]*int{"CINIM_STORAGE_BACKUP_KEEP": &c.Keep, "CINIM_STORAGE_BACKUP_MIN_ACKS": &c.MinAcks} {
		if v := os.Getenv(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return Config{}, fmt.Errorf("%s is not a number", name)
			}
			*dst = n
		}
	}
	return c, nil
}
