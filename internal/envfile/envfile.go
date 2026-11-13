// Package envfile reads a setting that may be given as a value in the environment or as a file (`NAME_FILE=/path`): a secret that a platform mounts as a file (a Kubernetes Secret, a Docker secret)
// is then never in the environment of the process, where a listing of it, a crash report or a child process would show it, and it is replaced by changing the file and restarting, with no rebuild.
package envfile

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// legacy names: the settings were called CINIM_STORAGE_<X> (CINIM_RQLITE_URL, CINIM_SHARD, CINIM_STORAGE_SHARDS) while the program was the storage module of one product. The new names are BLOBGATE_<X>
// (BLOBGATE_TENANT, BLOBGATE_TENANTS for the shards, now called tenants); the old ones still work, and a new one wins when both are set.
func legacyNames(name string) []string {
	suffix, ok := strings.CutPrefix(name, "BLOBGATE_")
	if !ok {
		return nil
	}
	switch suffix {
	case "TENANTS":
		return []string{"CINIM_STORAGE_SHARDS"}
	case "TENANT":
		return []string{"CINIM_SHARD"}
	case "RQLITE_URL":
		return []string{"CINIM_RQLITE_URL"}
	}
	return []string{"CINIM_STORAGE_" + suffix}
}

// Lookup returns the value of a setting by its new name, else by its old one.
func Lookup(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	for _, old := range legacyNames(name) {
		if v := os.Getenv(old); v != "" {
			return v
		}
	}
	return ""
}

// Get returns the value of NAME, or the contents of the file named by NAME_FILE (one trailing newline removed). Both set is an error: which of the two is meant is not for the program to guess.
func Get(name string) (string, error) {
	v, f := Lookup(name), Lookup(name+"_FILE")
	switch {
	case v != "" && f != "":
		return "", fmt.Errorf("%s and %s_FILE are both set: set one", name, name)
	case f == "":
		return v, nil
	}
	info, err := os.Stat(f) // #nosec G703 -- the path is the setting, as below
	if err != nil {
		return "", fmt.Errorf("%s_FILE: %w", name, err)
	}
	if info.Mode().Perm()&0o007 != 0 {
		return "", fmt.Errorf("%s_FILE: %s is readable by everyone (mode %o): make it readable by its owner only", name, f, info.Mode().Perm())
	}
	if info.Size() > 1<<20 {
		return "", errors.New(name + "_FILE: the file is over 1 MiB")
	}
	b, err := os.ReadFile(f) // #nosec G304 G703 -- the path is the setting: the file that the operator names is the one to read
	if err != nil {
		return "", fmt.Errorf("%s_FILE: %w", name, err)
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), nil
}
