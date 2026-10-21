// Package scheduler sends Warm, Open and Close at the right times (W1, W15)
// and creates the monthly booking/ticket partitions. It is leader-elected with
// pg_try_advisory_lock(42) and scans scheduler_job every 500 ms for due rows,
// marking them sent after producing.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	bookingv1 "railway/gen/booking/v1"
	"railway/internal/platform"
	"railway/internal/platform/pg"
)

const LeaderLock = 42

type Producer interface {
	Produce(ctx context.Context, cmd *bookingv1.Command) error
}

type Scheduler struct {
	DB       *pgxpool.Pool
	Producer Producer
	Log      *slog.Logger
	Every    time.Duration

	lastPartitions time.Time
}

func (s *Scheduler) Run(ctx context.Context) error {
	return pg.RunAsLeader(ctx, s.DB, LeaderLock, s.Log, s.loop)
}

func (s *Scheduler) loop(ctx context.Context) error {
	if s.Every == 0 {
		s.Every = 500 * time.Millisecond
	}
	t := time.NewTicker(s.Every)
	defer t.Stop()
	for {
		if err := s.tick(ctx); err != nil {
			s.Log.Warn("scheduler tick", "err", err)
		}
		if time.Since(s.lastPartitions) > 6*time.Hour {
			s.partitions(ctx)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// partitions keeps booking/ticket partitions (with the no-overlap constraint)
// for the current month and the next five.
func (s *Scheduler) partitions(ctx context.Context) {
	_, err := s.DB.Exec(ctx, `SELECT create_month_partitions((date_trunc('month', now()) + make_interval(months => m))::date)
	                           FROM generate_series(0, 5) m`)
	if err != nil {
		s.Log.Warn("create partitions", "err", err)
		return
	}
	s.lastPartitions = time.Now()
}

type job struct {
	journeyID int64
	kind      string
	dueAt     time.Time
}

func (s *Scheduler) tick(ctx context.Context) error {
	rows, err := s.DB.Query(ctx, `SELECT journey_id, kind, due_at FROM scheduler_job
	        WHERE sent_at IS NULL AND due_at <= now() ORDER BY due_at LIMIT 100`)
	if err != nil {
		return err
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.journeyID, &j.kind, &j.dueAt); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	for _, j := range jobs {
		cmd, err := Command(j.journeyID, j.kind, nil)
		if err != nil {
			s.Log.Error("bad scheduler job", "journey", j.journeyID, "kind", j.kind, "err", err)
			continue
		}
		if err := s.Producer.Produce(ctx, cmd); err != nil {
			return err // Kafka down: retried on the next tick
		}
		if _, err := s.DB.Exec(ctx, `UPDATE scheduler_job SET sent_at = now() WHERE journey_id = $1 AND kind = $2 AND due_at = $3`,
			j.journeyID, j.kind, j.dueAt); err != nil {
			return err
		}
		s.Log.Info("scheduler sent", "journey", j.journeyID, "kind", j.kind, "due_at", j.dueAt, "late", time.Since(j.dueAt))
	}
	return nil
}

// Command builds the system command for a job kind; the internal endpoints
// use it too. An OPEN payload {"pools":[...]} names pools; empty = all due.
func Command(journeyID int64, kind string, payload []byte) (*bookingv1.Command, error) {
	cmd := &bookingv1.Command{RequestId: platform.NewRequestID(), JourneyId: journeyID}
	switch kind {
	case "WARM":
		cmd.Body = &bookingv1.Command_Warm{Warm: &bookingv1.Warm{}}
	case "OPEN":
		var p struct {
			Pools []struct {
				ClassCode string `json:"class_code"`
				Quota     string `json:"quota"`
			} `json:"pools"`
		}
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &p); err != nil {
				return nil, err
			}
		}
		o := &bookingv1.Open{}
		for _, pl := range p.Pools {
			o.Pools = append(o.Pools, &bookingv1.Pool{ClassCode: pl.ClassCode, Quota: pl.Quota})
		}
		cmd.Body = &bookingv1.Command_Open{Open: o}
	case "CLOSE":
		cmd.Body = &bookingv1.Command_Close{Close: &bookingv1.Close{}}
	default:
		return nil, fmt.Errorf("unknown job kind %q", kind)
	}
	return cmd, nil
}
