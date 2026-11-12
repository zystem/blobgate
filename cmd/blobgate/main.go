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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zystem/blobgate/internal/api"
	"github.com/zystem/blobgate/internal/db"
	"github.com/zystem/blobgate/internal/maint"
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
	sec := func(name string, def int64) time.Duration { return time.Duration(envInt(name, def)) * time.Second }
	srv := api.New(api.Config{ShardSecrets: secrets, AdminToken: admin, Reserve: envInt("CINIM_STORAGE_RESERVE_BYTES", 0),
		MaxReads: int(envInt("CINIM_STORAGE_THREADS", 0)), UploadTTL: sec("CINIM_STORAGE_UPLOAD_TTL_SECONDS", 24*3600)}, c, reg, log)
	host, _ := os.Hostname()
	worker := maint.New(maint.Config{HolderID: fmt.Sprintf("%s-%d", host, os.Getpid()), RepairEvery: sec("CINIM_STORAGE_REPAIR_SECONDS", 60), CheckEvery: sec("CINIM_STORAGE_CHECK_SECONDS", 600),
		RetryBase: sec("CINIM_STORAGE_REPAIR_RETRY_SECONDS", 30), UploadTTL: sec("CINIM_STORAGE_UPLOAD_TTL_SECONDS", 24*3600), Deep: envInt("CINIM_STORAGE_DEEP_CHECK", 0) == 1,
		DeepBytesPS: envInt("CINIM_STORAGE_DEEP_CHECK_BYTES_PER_SECOND", 0), Log: log}, c, reg)
	var bg sync.WaitGroup
	bg.Add(1)
	go func() { defer bg.Done(); worker.Run(ctx) }()

	port := envInt("CINIM_STORAGE_PORT", 18090)
	shimPort := envInt("CINIM_STORAGE_SHIM_PORT", 18091)
	servers := []*http.Server{{Addr: fmt.Sprintf(":%d", port), Handler: srv.Handler(), ReadHeaderTimeout: 30 * time.Second}}
	if shimPort > 0 {
		// its own pool and socket timeouts (30 s), so that step code cannot take what the core's calls need
		servers = append(servers, &http.Server{Addr: fmt.Sprintf(":%d", shimPort), Handler: srv.ShimHandler(), ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second})
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
