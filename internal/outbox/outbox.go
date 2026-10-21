// Package outbox is the transactional outbox: rows written in the same Postgres
// transaction as business data, published to Kafka by the relay role.
package outbox

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	bookingv1 "railway/gen/booking/v1"
	"railway/internal/platform"
	"railway/internal/platform/kafka"
	"railway/internal/platform/metrics"
)

// Add inserts a row inside tx.
func Add(ctx context.Context, tx pgx.Tx, topic, key string, payload []byte) error {
	_, err := tx.Exec(ctx, `INSERT INTO outbox (topic, msg_key, payload) VALUES ($1, $2, $3)`, topic, key, payload)
	return err
}

// AddCommand queues a booking-requests command keyed by journey_id.
func AddCommand(ctx context.Context, tx pgx.Tx, cmd *bookingv1.Command) error {
	if cmd.RequestId == "" {
		cmd.RequestId = platform.NewRequestID()
	}
	b, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	return Add(ctx, tx, kafka.TopicBookingRequests, strconv.FormatInt(cmd.JourneyId, 10), b)
}

// Relay publishes unsent rows in id order: lock a batch (SKIP LOCKED so several
// relays can run), produce all with acks=all, mark them sent. A crash between
// produce and mark re-sends; consumers are idempotent (Confirm by payment_id).
type Relay struct {
	DB  *pgxpool.Pool
	Cl  *kgo.Client
	Log *slog.Logger
}

func (r *Relay) Run(ctx context.Context) error {
	idle := 50 * time.Millisecond
	for ctx.Err() == nil {
		n, err := r.once(ctx)
		if err != nil {
			r.Log.Warn("relay batch failed", "err", err)
			sleep(ctx, time.Second)
			continue
		}
		if n == 0 {
			r.reportAge(ctx)
			sleep(ctx, idle)
		}
	}
	return ctx.Err()
}

func (r *Relay) once(ctx context.Context) (int, error) {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id, topic, msg_key, payload FROM outbox WHERE sent_at IS NULL
	                             ORDER BY id LIMIT 500 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return 0, err
	}
	var ids []int64
	var recs []*kgo.Record
	for rows.Next() {
		var id int64
		var topic, key string
		var payload []byte
		if err := rows.Scan(&id, &topic, &key, &payload); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		recs = append(recs, &kgo.Record{Topic: topic, Key: []byte(key), Value: payload})
	}
	rows.Close()
	if len(recs) == 0 {
		return 0, nil
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = r.Cl.ProduceSync(pctx, recs...).FirstErr()
	cancel()
	if err != nil {
		return 0, err // Kafka down: the outbox grows, nothing is lost (W10)
	}
	if _, err := tx.Exec(ctx, `UPDATE outbox SET sent_at = now() WHERE id = ANY($1)`, ids); err != nil {
		return 0, err
	}
	return len(ids), tx.Commit(ctx)
}

func (r *Relay) reportAge(ctx context.Context) {
	var age float64
	_ = r.DB.QueryRow(ctx, `SELECT COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0) FROM outbox WHERE sent_at IS NULL`).Scan(&age)
	metrics.OutboxOldestSeconds.Set(age)
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
