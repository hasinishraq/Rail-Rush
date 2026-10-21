// Package projector consumes inventory-events and maintains the booking and
// ticket projection in Postgres, the purchase counters (mirrored to Redis
// ulimit:*), and the ticket notifications. Offsets are stored in
// projector_offset in the same transaction as the rows they produced, so the
// projection is exactly-once and resumes from its stored offset.
//
// ticket carries the EXCLUDE no-overlap constraint: a violation means a bug
// upstream, so the partition stops and pages (§5.6, §21).
package projector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	bookingv1 "railway/gen/booking/v1"
	"railway/internal/catalog"
	"railway/internal/fares"
	"railway/internal/inventory"
	"railway/internal/otp"
	"railway/internal/platform/kafka"
	"railway/internal/platform/metrics"
	"railway/internal/wire"
)

const consumerName = "projector"

type Projector struct {
	Brokers []string
	DB      *pgxpool.Pool
	RDB     redis.UniversalClient
	Cat     *catalog.Catalog
	Log     *slog.Logger
	cl      *kgo.Client
}

func (p *Projector) Run(ctx context.Context) error {
	var err error
	p.cl, err = kgo.NewClient(
		kgo.SeedBrokers(p.Brokers...),
		kgo.ConsumerGroup(consumerName),
		kgo.ConsumeTopics(kafka.TopicInventoryEvents),
		kgo.FetchIsolationLevel(kgo.ReadCommitted()),
		kgo.DisableAutoCommit(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.OnPartitionsAssigned(p.onAssigned),
	)
	if err != nil {
		return err
	}
	defer p.cl.Close()
	for ctx.Err() == nil {
		fetches := p.cl.PollFetches(ctx)
		if ctx.Err() != nil {
			break
		}
		fetches.EachError(func(_ string, part int32, err error) { p.Log.Warn("projector fetch", "partition", part, "err", err) })
		fetches.EachPartition(func(fp kgo.FetchTopicPartition) {
			if len(fp.Records) == 0 {
				return
			}
			for attempt := 1; ; attempt++ {
				err := p.project(ctx, fp.Partition, fp.Records)
				if err == nil || ctx.Err() != nil {
					break
				}
				var pe *pgconn.PgError
				if errors.As(err, &pe) && pe.Code == "23P01" {
					metrics.ProjectorExcludeViolations.Inc()
					p.Log.Error("PAGE no-overlap constraint rejected a ticket; partition stopped", "partition", fp.Partition, "err", err)
				} else {
					p.Log.Warn("projection batch failed; retrying", "partition", fp.Partition, "attempt", attempt, "err", err)
				}
				select {
				case <-ctx.Done():
				case <-time.After(time.Duration(min(attempt, 10)) * time.Second):
				}
			}
		})
	}
	return ctx.Err()
}

// onAssigned resumes each partition from the offset stored in Postgres.
func (p *Projector) onAssigned(ctx context.Context, cl *kgo.Client, m map[string][]int32) {
	parts := m[kafka.TopicInventoryEvents]
	offs := map[int32]kgo.EpochOffset{}
	rows, err := p.DB.Query(ctx, `SELECT part, last_offset FROM projector_offset WHERE consumer = $1 AND part = ANY($2)`, consumerName, parts)
	if err != nil {
		p.Log.Error("load projector offsets", "err", err)
		return
	}
	for rows.Next() {
		var part int32
		var off int64
		if rows.Scan(&part, &off) == nil {
			offs[part] = kgo.EpochOffset{Epoch: -1, Offset: off + 1}
		}
	}
	rows.Close()
	if len(offs) > 0 {
		cl.SetOffsets(map[string]map[int32]kgo.EpochOffset{kafka.TopicInventoryEvents: offs})
	}
}

// ulimit is a purchase-counter change to mirror to Redis after the commit.
type ulimit struct {
	user    int64
	runDate string
	seats   int
}

func (p *Projector) project(ctx context.Context, part int32, recs []*kgo.Record) error {
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var stored int64 = -1
	err = tx.QueryRow(ctx, `SELECT last_offset FROM projector_offset WHERE consumer = $1 AND part = $2 FOR UPDATE`, consumerName, part).Scan(&stored)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	last := stored
	var limits []ulimit
	for _, r := range recs {
		if r.Offset <= stored {
			continue // already projected
		}
		var ev bookingv1.Event
		if err := proto.Unmarshal(r.Value, &ev); err != nil {
			p.Log.Error("projector: bad event skipped", "offset", r.Offset, "err", err)
			last = r.Offset
			continue
		}
		u, err := p.apply(ctx, tx, &ev, r.Offset)
		if err != nil {
			return fmt.Errorf("event %s (%s) at %d: %w", ev.EventId, inventory.Kind(&ev), r.Offset, err)
		}
		limits = append(limits, u...)
		metrics.ProjectorEvents.WithLabelValues(inventory.Kind(&ev)).Inc()
		last = r.Offset
	}
	if last == stored {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO projector_offset (consumer, part, last_offset) VALUES ($1, $2, $3)
	        ON CONFLICT (consumer, part) DO UPDATE SET last_offset = EXCLUDED.last_offset`, consumerName, part, last); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if len(limits) > 0 && p.RDB != nil {
		pipe := p.RDB.Pipeline()
		for _, u := range limits {
			if d, err := time.Parse("2006-01-02", u.runDate); err == nil {
				pipe.Set(ctx, wire.ULimitKey(u.user, u.runDate), u.seats, max(time.Until(d.AddDate(0, 0, 2)), time.Hour))
			}
		}
		if _, err := pipe.Exec(ctx); err != nil {
			p.Log.Warn("ulimit mirror failed", "err", err)
		}
	}
	return nil
}

func gender(g bookingv1.Gender) string {
	switch g {
	case bookingv1.Gender_MALE, bookingv1.Gender_FEMALE:
		return g.String()
	}
	return "OTHER"
}

func paxType(t bookingv1.PassengerType) string {
	if t == bookingv1.PassengerType_CHILD {
		return "CHILD"
	}
	return "ADULT"
}

func coachOf(l *catalog.Layout, seatID string) string {
	if l != nil {
		if s, ok := l.SeatByID(seatID); ok {
			return s.Coach
		}
	}
	return ""
}

func (p *Projector) apply(ctx context.Context, tx pgx.Tx, ev *bookingv1.Event, off int64) ([]ulimit, error) {
	at := ev.GetAt().AsTime()
	j, err := p.Cat.Journey(ctx, ev.JourneyId)
	if err != nil {
		return nil, err
	}
	layout, _ := p.Cat.Layout(j.LayoutID)
	rd := j.RunDate

	switch b := ev.Body.(type) {
	case *bookingv1.Event_SeatHeld:
		sh := b.SeatHeld
		idem := ev.IdemKey
		if idem == "" {
			idem = "pnr:" + sh.Pnr
		}
		tag, err := tx.Exec(ctx, `INSERT INTO booking (run_date, pnr, journey_id, user_id, from_idx, to_idx, class_code, quota,
		        status, hold_expires_at, amount_minor, currency, idem_key, created_at, updated_at, last_event_offset)
		        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'PENDING_PAYMENT',$9,$10,$11,$12,$13,$13,$14)
		        ON CONFLICT DO NOTHING`,
			rd, sh.Pnr, ev.JourneyId, sh.UserId, sh.FromIdx, sh.ToIdx, sh.ClassCode, sh.Quota,
			sh.ExpiresAt.AsTime(), sh.AmountMinor, sh.Currency, idem, at, off)
		if err != nil || tag.RowsAffected() == 0 {
			return nil, err
		}
		adult := fares.Money{}
		if t, err := p.Cat.Train(j.TrainNo); err == nil {
			adult = p.Cat.Fare(j, t, sh.ClassCode, int(sh.FromIdx), int(sh.ToIdx))
		}
		for i, s := range sh.Seats {
			px := &bookingv1.Passenger{}
			if i < len(sh.Passengers) {
				px = sh.Passengers[i]
			}
			if _, err := tx.Exec(ctx, `INSERT INTO ticket (run_date, pnr, seq, journey_id, user_id, seat_idx, seat_id, coach_code,
			        from_idx, to_idx, passenger_name, gender, passenger_type, fare_minor, status, created_at, updated_at)
			        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'HELD',$15,$15)`,
				rd, sh.Pnr, i+1, ev.JourneyId, sh.UserId, s.SeatIdx, s.SeatId, coachOf(layout, s.SeatId),
				sh.FromIdx, sh.ToIdx, px.Name, gender(px.Gender), paxType(px.Type),
				fares.PassengerFare(adult.AmountMinor, px.Type == bookingv1.PassengerType_CHILD), at); err != nil {
				return nil, err
			}
		}
		return nil, nil

	case *bookingv1.Event_Confirmed:
		c := b.Confirmed
		var user int64
		var status string
		err := tx.QueryRow(ctx, `SELECT user_id, status FROM booking WHERE run_date = $1 AND pnr = $2 FOR UPDATE`, rd, c.Pnr).Scan(&user, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil || (status != "PENDING_PAYMENT" && status != "EXPIRED") {
			return nil, err
		}
		// Late confirmation may land on other seats: move the tickets first,
		// then issue them; the no-overlap constraint checks the result.
		for i, s := range c.Seats {
			if _, err := tx.Exec(ctx, `UPDATE ticket SET seat_idx = $4, seat_id = $5, coach_code = $6, status = 'ISSUED', updated_at = $7
			        WHERE run_date = $1 AND pnr = $2 AND seq = $3`, rd, c.Pnr, i+1, s.SeatIdx, s.SeatId, coachOf(layout, s.SeatId), at); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE booking SET status = 'CONFIRMED', hold_expires_at = NULL, updated_at = $3, last_event_offset = $4
		        WHERE run_date = $1 AND pnr = $2`, rd, c.Pnr, at, off); err != nil {
			return nil, err
		}
		u, err := p.count(ctx, tx, user, rd, len(c.Seats))
		if err != nil {
			return nil, err
		}
		text := fmt.Sprintf("RailRush: PNR %s CONFIRMED, %d seat(s) on %s. Download your e-ticket in the app.", c.Pnr, len(c.Seats), rd.Format("02 Jan 2006"))
		if c.SeatsChanged {
			text = fmt.Sprintf("RailRush: payment for PNR %s arrived after the hold ended; your original seats were taken, so you have new seats. Check the booking.", c.Pnr)
		}
		return []ulimit{u}, otp.QueueSMS(ctx, tx, otp.SMS{UserID: user, PNR: c.Pnr, Purpose: "TICKET", Text: text})

	case *bookingv1.Event_ConfirmFailed:
		cf := b.ConfirmFailed
		var user int64
		err := tx.QueryRow(ctx, `UPDATE booking SET status = 'CONFIRM_FAILED', updated_at = $3, last_event_offset = $4
		        WHERE run_date = $1 AND pnr = $2 AND status = 'EXPIRED' RETURNING user_id`, rd, cf.Pnr, at, off).Scan(&user)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // a second capture for a confirmed PNR: only refunded
		}
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE ticket SET status = 'FAILED', updated_at = $3 WHERE run_date = $1 AND pnr = $2 AND status IN ('HELD','EXPIRED')`,
			rd, cf.Pnr, at); err != nil {
			return nil, err
		}
		return nil, otp.QueueSMS(ctx, tx, otp.SMS{UserID: user, PNR: cf.Pnr, Purpose: "REFUND",
			Text: fmt.Sprintf("RailRush: your payment for PNR %s came after the seats were released. A full refund has been issued.", cf.Pnr)})

	case *bookingv1.Event_HoldExpired:
		pnr := b.HoldExpired.Pnr
		if _, err := tx.Exec(ctx, `UPDATE booking SET status = 'EXPIRED', updated_at = $3, last_event_offset = $4
		        WHERE run_date = $1 AND pnr = $2 AND status = 'PENDING_PAYMENT'`, rd, pnr, at, off); err != nil {
			return nil, err
		}
		_, err := tx.Exec(ctx, `UPDATE ticket SET status = 'EXPIRED', updated_at = $3 WHERE run_date = $1 AND pnr = $2 AND status = 'HELD'`, rd, pnr, at)
		return nil, err

	case *bookingv1.Event_DeadlinesExtended:
		_, err := tx.Exec(ctx, `UPDATE booking SET hold_expires_at = hold_expires_at + make_interval(secs => $3::float8 / 1000), updated_at = $4
		        WHERE run_date = $1 AND journey_id = $2 AND status = 'PENDING_PAYMENT'`, rd, ev.JourneyId, float64(b.DeadlinesExtended.DeltaMs), at)
		return nil, err

	case *bookingv1.Event_Cancelled:
		c := b.Cancelled
		tag, err := tx.Exec(ctx, `UPDATE ticket SET status = 'CANCELLED', updated_at = $4
		        WHERE run_date = $1 AND pnr = $2 AND seq = ANY($3) AND status = 'ISSUED'`, rd, c.Pnr, c.PassengerSeqs, at)
		if err != nil {
			return nil, err
		}
		var user int64
		err = tx.QueryRow(ctx, `UPDATE booking SET updated_at = $3, last_event_offset = $4,
		        status = CASE WHEN EXISTS (SELECT 1 FROM ticket t WHERE t.run_date = $1 AND t.pnr = $2 AND t.status = 'ISSUED')
		                      THEN 'PARTIALLY_CANCELLED' ELSE 'CANCELLED' END
		        WHERE run_date = $1 AND pnr = $2 RETURNING user_id`, rd, c.Pnr, at, off).Scan(&user)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		u, err := p.count(ctx, tx, user, rd, -int(tag.RowsAffected()))
		if err != nil {
			return nil, err
		}
		return []ulimit{u}, otp.QueueSMS(ctx, tx, otp.SMS{UserID: user, PNR: c.Pnr, Purpose: "CANCELLED",
			Text: fmt.Sprintf("RailRush: %d seat(s) on PNR %s cancelled. Any refund due is on its way.", tag.RowsAffected(), c.Pnr)})

	case *bookingv1.Event_SalesClosed:
		_, err := tx.Exec(ctx, `UPDATE journey SET status = 'CLOSED' WHERE journey_id = $1 AND status = 'SCHEDULED'`, ev.JourneyId)
		return nil, err
	}
	return nil, nil
}

// count adds delta seats to purchase_counter (the 4-per-travel-day rule) and
// returns the new value for the Redis mirror.
func (p *Projector) count(ctx context.Context, tx pgx.Tx, user int64, runDate time.Time, delta int) (ulimit, error) {
	var n int
	err := tx.QueryRow(ctx, `INSERT INTO purchase_counter (user_id, run_date, seats_issued) VALUES ($1, $2, GREATEST($3, 0))
	        ON CONFLICT (user_id, run_date) DO UPDATE SET seats_issued = GREATEST(purchase_counter.seats_issued + $3, 0)
	        RETURNING seats_issued`, user, runDate, delta).Scan(&n)
	return ulimit{user: user, runDate: runDate.Format("2006-01-02"), seats: n}, err
}
