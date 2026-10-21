// Command allocator is the stateful inventory owner: it owns booking-requests
// partitions through the "allocator" consumer group and is the only writer of
// seat inventory (§6).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"railway/internal/allocator"
	"railway/internal/catalog"
	"railway/internal/platform"
	"railway/internal/platform/config"
	"railway/internal/platform/pg"
	"railway/internal/platform/redisx"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := platform.NewLogger(cfg.LogLevel).With("role", "allocator", "pod", cfg.PodName)
	db, err := pg.Connect(ctx, cfg.PGURL)
	if err != nil {
		return err
	}
	defer db.Close()
	rdb := redisx.New(cfg.RedisAddr)
	cat := catalog.New(db, log)
	if err := cat.Load(ctx); err != nil {
		return fmt.Errorf("load catalog: %w", err)
	}
	go cat.Run(ctx, 5*time.Minute)

	a := allocator.New(cfg, log, cat, db, rdb)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !a.Ready() {
			http.Error(w, "kafka session not ready", http.StatusServiceUnavailable)
			return
		}
		if err := redisx.Ping(r.Context(), rdb); err != nil {
			http.Error(w, "redis: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready"))
	})
	mux.Handle("GET /metrics", promhttp.Handler())
	srv := &http.Server{Addr: cfg.InternalAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("internal server", "err", err)
		}
	}()
	defer srv.Close()

	log.Info("allocator starting", "brokers", cfg.KafkaBrokers)
	return a.Run(ctx)
}
