// Package pg opens Postgres pools (through PgBouncer in production) and
// provides advisory-lock leader election.
package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens a pool. Statement caching is disabled so the pool works behind
// PgBouncer in transaction mode.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	cfg.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	for i := 0; ; i++ {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = pool.Ping(pctx)
		cancel()
		if err == nil || i >= 30 || ctx.Err() != nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	return pool, nil
}

// Tx runs fn in a transaction, committing on nil and rolling back otherwise.
func Tx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

// IsUniqueViolation reports a unique-constraint violation (SQLSTATE 23505).
func IsUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// ConstraintName returns the violated constraint, if any.
func ConstraintName(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.ConstraintName
	}
	return ""
}

// RunAsLeader blocks until it holds pg_try_advisory_lock(key) on a dedicated
// connection, then runs fn with a context that is cancelled if the lock
// connection is lost. It retries until ctx ends.
func RunAsLeader(ctx context.Context, pool *pgxpool.Pool, key int64, log *slog.Logger, fn func(ctx context.Context) error) error {
	for ctx.Err() == nil {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			sleep(ctx, 2*time.Second)
			continue
		}
		var got bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&got); err != nil || !got {
			conn.Release()
			sleep(ctx, 2*time.Second)
			continue
		}
		log.Info("acquired leadership", "lock", key)
		lctx, cancel := context.WithCancel(ctx)
		go func() { // watchdog: losing the session loses the lock
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-lctx.Done():
					return
				case <-t.C:
					if err := conn.Ping(lctx); err != nil {
						log.Warn("leader connection lost", "err", err)
						cancel()
						return
					}
				}
			}
		}()
		err = fn(lctx)
		cancel()
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key)
		conn.Release()
		if err != nil && ctx.Err() == nil {
			log.Error("leader task failed", "err", err)
			sleep(ctx, time.Second)
		}
	}
	return ctx.Err()
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
