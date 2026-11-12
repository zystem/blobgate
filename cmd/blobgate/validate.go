package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zystem/blobgate/internal/store"
)

const validateUsage = `usage: blobgate validate [-json] [-secret option=value]... '<rclone connection string>'

Asks one node, step by step, for what the module needs of it (write into directories that do not exist, replace a file, many writers at once, odd names, removal, free space) and says what it
could not do. It leaves nothing behind and needs no database. Example:

  blobgate validate -secret pass=... ':ftp,host=files.example.com,user=blob:/blobs'

A password is given with -secret (rclone options that hold passwords are obscured for you); it does not go into the connection string. Exit code 0: the node can be used (the checks that are not
required may still warn); 1: it cannot; 2: wrong arguments.`

// validate is the subcommand that tests a node before it is put in a store.
func validate(args []string) int {
	var asJSON bool
	secrets := map[string]string{}
	var remote string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-json":
			asJSON = true
		case a == "-secret" && i+1 < len(args):
			i++
			k, v, ok := strings.Cut(args[i], "=")
			if !ok || k == "" {
				fmt.Fprintln(os.Stderr, validateUsage)
				return 2
			}
			secrets[k] = v
		case strings.HasPrefix(a, "-"):
			fmt.Fprintln(os.Stderr, validateUsage)
			return 2
		default:
			remote = a
		}
	}
	if remote == "" {
		fmt.Fprintln(os.Stderr, validateUsage)
		return 2
	}
	spec, err := store.ParseRemote(remote)
	if err != nil {
		fmt.Fprintln(os.Stderr, "validate:", err)
		return 2
	}
	for k, v := range secrets {
		spec.Options[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	f, err := spec.Open(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "validate: the node could not be opened:", err)
		return 1
	}
	checks := store.Validate(ctx, store.NewNode(spec, f))
	usable := store.Usable(checks)
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"usable": usable, "checks": checks})
	} else {
		for _, c := range checks {
			mark := "ok  "
			switch {
			case !c.OK && c.Required:
				mark = "FAIL"
			case !c.OK:
				mark = "warn"
			}
			line := fmt.Sprintf("%s  %s", mark, c.Name)
			if c.Detail != "" && (!c.OK || c.Name == "free space (about)") {
				line += ": " + c.Detail
			}
			fmt.Println(line)
		}
		if usable {
			fmt.Println("the node can be used")
		} else {
			fmt.Println("the node cannot be used: fix what failed above")
		}
	}
	if usable {
		return 0
	}
	return 1
}
