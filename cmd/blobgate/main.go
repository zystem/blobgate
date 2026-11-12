// blobgate: the storage module of CiNim. A Deployment with no volume: every setting is in rqlite and in the environment (see docs/configuration.md).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zystem/blobgate/internal/api"
	"github.com/zystem/blobgate/internal/backup"
	"github.com/zystem/blobgate/internal/db"
	"github.com/zystem/blobgate/internal/maint"
	"github.com/zystem/blobgate/internal/metrics"
	"github.com/zystem/blobgate/internal/rq"
	"github.com/zystem/blobgate/internal/seal"
	"github.com/zystem/blobgate/internal/store"
)

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "storage: "+msg)
	os.Exit(2)
}

func envInt(name string, def int64) int64 {
	if v := os.Getenv(name); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			fatal(name + " is not a number")
		}
		return n
	}
	return def
}

func main() {
	// blocks of up to 8 MiB are held while they are read and sealed, so the heap is mostly short-lived buffers: collect when it has grown a quarter, not a whole (GOGC overrides this)
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(25)
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "validate":
			os.Exit(validate(os.Args[2:]))
		case "token":
			os.Exit(tokenCommand(os.Args[2:]))
		case "backup", "backups", "verify", "restore":
			os.Exit(backupCommand(os.Args[1], os.Args[2:]))
		case "-h", "--help", "help":
			fmt.Println(validateUsage + "\n\n" + backupUsage + "\n\n" + tokenUsage)
			return
		}
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	url := os.Getenv("CINIM_RQLITE_URL")
	if url == "" {
		fatal("CINIM_RQLITE_URL is not set")
	}
	secrets := map[string]string{}
	for _, pair := range strings.Split(os.Getenv("CINIM_STORAGE_SHARDS"), ";") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(k) == "" {
			fatal("CINIM_STORAGE_SHARDS is <shard>=<secret>;<shard>=<secret>")
		}
		secrets[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if t := os.Getenv("CINIM_STORAGE_TOKEN"); len(secrets) == 0 && t != "" {
		shard := os.Getenv("CINIM_SHARD")
		if shard == "" {
			shard = "001"
		}
		secrets[shard] = t
	}
	if len(secrets) == 0 {
		fatal("no shard is served: set CINIM_STORAGE_SHARDS or CINIM_STORAGE_TOKEN")
	}
	for shard, sec := range secrets {
		if shard == "" || strings.Trim(shard, "0123456789") != "" {
			fatal("a shard is made of digits only (SHD-001): " + shard)
		}
		if len(sec) < 16 {
			fatal("the secret of shard " + shard + " is shorter than 16 characters")
		}
	}
	admin := os.Getenv("CINIM_STORAGE_ADMIN_TOKEN")
	if len(admin) < 16 {
		fatal("CINIM_STORAGE_ADMIN_TOKEN is shorter than 16 characters")
	}
	sealText := os.Getenv("CINIM_STORAGE_SEAL_KEY")
	if len(sealText) < 16 {
		fatal("CINIM_STORAGE_SEAL_KEY is shorter than 16 characters")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	c := rq.New(url)
	// a database that is new is filled from the newest backup, when the module is told to (CINIM_STORAGE_RESTORE_ON_EMPTY=1), before it makes its own tables
	if os.Getenv("CINIM_STORAGE_RESTORE_ON_EMPTY") == "1" {
		restoreIfEmpty(ctx, url, log)
	}
	// the tables: the database may come up after the module does (two minutes)
	for tries := 0; ; tries++ {
		if err := db.Migrate(ctx, c); err == nil {
			break
		} else if tries >= 60 || ctx.Err() != nil {
			fatal("rqlite does not answer: " + err.Error())
		}
		time.Sleep(2 * time.Second)
	}
	reg := store.NewRegistry(c, seal.Key(sealText))
	metrics.RegisterDB(c, 15*time.Second)
	sec := func(name string, def int64) time.Duration { return time.Duration(envInt(name, def)) * time.Second }
	srv := api.New(api.Config{ShardSecrets: secrets, AdminToken: admin, Reserve: envInt("CINIM_STORAGE_RESERVE_BYTES", 0),
		MaxReads: int(envInt("CINIM_STORAGE_THREADS", 0)), UploadTTL: sec("CINIM_STORAGE_UPLOAD_TTL_SECONDS", 24*3600)}, c, reg, log)
	host, _ := os.Hostname()
	worker := maint.New(maint.Config{HolderID: fmt.Sprintf("%s-%d", host, os.Getpid()), RepairEvery: sec("CINIM_STORAGE_REPAIR_SECONDS", 60), LeaseFor: sec("CINIM_STORAGE_LEASE_SECONDS", 120), CheckEvery: sec("CINIM_STORAGE_CHECK_SECONDS", 600),
		RetryBase: sec("CINIM_STORAGE_REPAIR_RETRY_SECONDS", 30), UploadTTL: sec("CINIM_STORAGE_UPLOAD_TTL_SECONDS", 24*3600), Deep: envInt("CINIM_STORAGE_DEEP_CHECK", 0) == 1,
		DeepBytesPS: envInt("CINIM_STORAGE_DEEP_CHECK_BYTES_PER_SECOND", 0), Log: log}, c, reg)
	var bg sync.WaitGroup
	bg.Add(1)
	go func() { defer bg.Done(); worker.Run(ctx) }()
	if every := sec("CINIM_STORAGE_BACKUP_SECONDS", 0); every > 0 {
		bg.Add(1)
		go func() { defer bg.Done(); backupLoop(ctx, worker, url, every, log) }()
	}

	port := envInt("CINIM_STORAGE_PORT", 18090)
	shimPort := envInt("CINIM_STORAGE_SHIM_PORT", 18091)
	servers := []*http.Server{{Addr: fmt.Sprintf(":%d", port), Handler: srv.Handler(), ReadHeaderTimeout: 30 * time.Second}}
	if shimPort > 0 {
		// its own pool and socket timeouts (30 s), so that step code cannot take what the core's calls need
		servers = append(servers, &http.Server{Addr: fmt.Sprintf(":%d", shimPort), Handler: srv.ShimHandler(), ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second})
	}
	// the metrics on a port of their own, with no token, for a scraper inside the cluster (off by default; /metrics on the main port wants the admin token)
	if mp := envInt("CINIM_STORAGE_METRICS_PORT", 0); mp > 0 {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.Handler())
		servers = append(servers, &http.Server{Addr: fmt.Sprintf(":%d", mp), Handler: mux, ReadHeaderTimeout: 10 * time.Second})
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *http.Server) { errc <- s.ListenAndServe() }(s)
	}
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	fmt.Println("storage: serving shards", strings.Join(keys, ","), "on port", port)
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			fatal(err.Error())
		}
	}
	// the requests that are going on are finished first
	shut, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shut)
	}
	bg.Wait() // the worker lets go of its lease when ctx ends
}

// restoreIfEmpty waits for rqlite (two minutes), and when it has no table fills it from the newest backup. No backup is no reason to stop: the module starts empty.
func restoreIfEmpty(ctx context.Context, url string, log *slog.Logger) {
	cfg, err := backup.FromEnv(url)
	if err != nil {
		fatal("CINIM_STORAGE_RESTORE_ON_EMPTY is set and the backups are not: " + err.Error())
	}
	var empty bool
	for tries := 0; ; tries++ {
		if empty, err = backup.IsEmpty(ctx, url); err == nil {
			break
		}
		if tries >= 60 || ctx.Err() != nil {
			fatal("rqlite does not answer: " + err.Error())
		}
		time.Sleep(2 * time.Second)
	}
	if !empty {
		return
	}
	m, err := backup.RestoreWith(ctx, cfg, backup.Options{Fallback: os.Getenv("CINIM_STORAGE_RESTORE_FALLBACK") == "1"})
	switch {
	case err == nil:
		log.Info("the database was empty and was restored", "backup", m.ID, "created", m.Created, "bytes", m.RawSize)
	case strings.Contains(err.Error(), "no backup was found"):
		log.Info("the database is empty and there is no backup: starting empty")
	default:
		// a backup that cannot be read is not a reason to start on an empty database and make new tables over what the backup would have restored
		fatal("the database is empty and the restore failed: " + err.Error())
	}
}

// backupLoop takes a backup every `every`; one replica at a time (the lease "backup").
func backupLoop(ctx context.Context, w *maint.Worker, url string, every time.Duration, log *slog.Logger) {
	cfg, err := backup.FromEnv(url)
	if err != nil {
		log.Error("CINIM_STORAGE_BACKUP_SECONDS is set and the backups are not", "error", err.Error())
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.Release(context.Background(), "backup")
			return
		case <-t.C:
		}
		if ok, err := w.LeaseFor(ctx, "backup", every+time.Minute); err != nil || !ok {
			continue
		}
		m, err := backup.Backup(ctx, cfg)
		if err != nil {
			log.Error("backup failed", "error", err.Error())
			continue
		}
		metrics.BackupWritten(time.Now())
		log.Info("backup written", "id", m.ID, "bytes", m.RawSize, "blocks", len(m.Blocks))
	}
}
