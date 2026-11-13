// Package envfile reads a setting that may be given as a value in the environment or as a file (`NAME_FILE=/path`): a secret that a platform mounts as a file (a Kubernetes Secret, a Docker secret)
// is then never in the environment of the process, where a listing of it, a crash report or a child process would show it, and it is replaced by changing the file and restarting, with no rebuild.
package envfile

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Get returns the value of NAME, or the contents of the file named by NAME_FILE (one trailing newline removed). Both set is an error: which of the two is meant is not for the program to guess.
func Get(name string) (string, error) {
	v, f := os.Getenv(name), os.Getenv(name+"_FILE")
	switch {
	case v != "" && f != "":
		return "", fmt.Errorf("%s and %s_FILE are both set: set one", name, name)
	case f == "":
		return v, nil
	}
	info, err := os.Stat(f)
	if err != nil {
		return "", fmt.Errorf("%s_FILE: %w", name, err)
	}
	if info.Mode().Perm()&0o007 != 0 {
		return "", fmt.Errorf("%s_FILE: %s is readable by everyone (mode %o): make it readable by its owner only", name, f, info.Mode().Perm())
	}
	if info.Size() > 1<<20 {
		return "", errors.New(name + "_FILE: the file is over 1 MiB")
	}
	b, err := os.ReadFile(f) // #nosec G304 -- the path is the setting: the file that the operator names is the one to read
	if err != nil {
		return "", fmt.Errorf("%s_FILE: %w", name, err)
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), nil
}
