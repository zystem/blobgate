package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/zystem/blobgate/internal/envfile"
	"os"
	"time"

	"github.com/zystem/blobgate/internal/backup"
)

const backupUsage = `usage:
  blobgate backup                 take a backup of rqlite now and write it to the backup nodes
  blobgate backups [-json]        list the backups on the nodes, newest first
  blobgate verify [-id ID | -all] read a backup whole and check it (the newest by default; -all: every one, and
                                  which can be read); changes nothing
  blobgate restore [-id ID] [-fallback] [-force]
                                  put a backup (the newest by default) into rqlite; refuses a database that is
                                  not empty unless -force. If the newest does not check out, the error names the
                                  older ones: pick one with -id, or let -fallback take the newest that checks out.
                                  Run it before the module starts: the module makes its tables when it starts, and
                                  then the database is no longer empty.

The settings are in the environment: BLOBGATE_RQLITE_URL, BLOBGATE_BACKUP_NODES, BLOBGATE_BACKUP_KEY
(docs/backup.md). The backups do not depend on the database, so a restore needs nothing but these.`

// backupCommand runs the commands that work with the backups of the database; it returns the exit code.
func backupCommand(name string, args []string) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	id := fs.String("id", "", "the id of a backup (default: the newest)")
	force := fs.Bool("force", false, "restore into a database that is not empty")
	fallback := fs.Bool("fallback", false, "restore: when the newest backup does not check out, take the newest older one that does")
	all := fs.Bool("all", false, "verify: every backup")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, backupUsage) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	url := envfile.Lookup("BLOBGATE_RQLITE_URL")
	if url == "" {
		fmt.Fprintln(os.Stderr, name+": BLOBGATE_RQLITE_URL is not set")
		return 2
	}
	cfg, err := backup.FromEnv(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, name+":", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	say := func(m backup.Manifest) {
		fmt.Printf("%s  %s  %d bytes in %d block(s)  sealed=%v\n", m.ID, m.Created.Format(time.RFC3339), m.RawSize, len(m.Blocks), m.Sealed)
	}
	switch name {
	case "backup":
		m, err := backup.Backup(ctx, cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "backup:", err)
			return 1
		}
		say(m)
	case "backups":
		ms, err := backup.List(ctx, cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "backups:", err)
			return 1
		}
		if *asJSON {
			_ = json.NewEncoder(os.Stdout).Encode(ms)
			return 0
		}
		for _, m := range ms {
			say(m)
		}
		if len(ms) == 0 {
			fmt.Println("no backups")
		}
	case "verify":
		if *all {
			rs, err := backup.VerifyAll(ctx, cfg)
			if err != nil {
				fmt.Fprintln(os.Stderr, "verify:", err)
				return 1
			}
			bad := 0
			for _, r := range rs {
				if r.Err != nil {
					bad++
					fmt.Print("BAD ")
				} else {
					fmt.Print("ok  ")
				}
				say(r.Manifest)
				if r.Err != nil {
					fmt.Println("      ", r.Err)
				}
			}
			if len(rs) == 0 {
				fmt.Println("no backups")
			}
			if bad > 0 {
				return 1
			}
			return 0
		}
		m, err := backup.Verify(ctx, cfg, *id)
		if err != nil {
			fmt.Fprintln(os.Stderr, "verify:", err)
			return 1
		}
		fmt.Print("ok  ")
		say(m)
	case "restore":
		m, err := backup.RestoreWith(ctx, cfg, backup.Options{ID: *id, Force: *force, Fallback: *fallback})
		if err != nil {
			fmt.Fprintln(os.Stderr, "restore:", err)
			return 1
		}
		fmt.Print("restored  ")
		say(m)
	}
	return 0
}
