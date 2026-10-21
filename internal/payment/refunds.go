package payment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	bookingv1 "railway/gen/booking/v1"
	"railway/internal/booking"
	"railway/internal/catalog"
	"railway/internal/fares"
	"railway/internal/inventory"
	"railway/internal/platform/kafka"
	"railway/internal/platform/metrics"
	"railway/internal/platform/pg"
)

// Refunds issues and retries refunds.
type Refunds struct {
	DB  *pgxpool.Pool
	GW  GatewayAdapter
	Log *slog.Logger
}

// Issue calls the gateway for one refund row and records the outcome. The
// gateway call is idempotent by the refund's idem_key.
func (r *Refunds) Issue(ctx context.Context, refundID string) error {
	var paymentID, idem, currency, status, orderID string
	var amount int64
	err := r.DB.QueryRow(ctx, `SELECT r.payment_id::text, r.idem_key, p.currency, r.status, r.amount_minor, COALESCE(p.gateway_order_id,'')
	                             FROM refund r JOIN payment p USING (payment_id) WHERE r.refund_id = $1`, refundID).
		Scan(&paymentID, &idem, &currency, &status, &amount, &orderID)
	if err != nil || status == "SUCCEEDED" {
		return err
	}
	gwID, gerr := r.GW.Refund(ctx, Refund{RefundID: refundID, PaymentID: paymentID, GatewayOrderID: orderID,
		AmountMinor: amount, Currency: currency, IdemKey: idem})
	if gerr != nil {
		_, err := r.DB.Exec(ctx, `UPDATE refund SET status = 'FAILED', attempts = attempts + 1, updated_at = now() WHERE refund_id = $1`, refundID)
		return errors.Join(gerr, err)
	}
	return pg.Tx(ctx, r.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE refund SET status = 'SUCCEEDED', attempts = attempts + 1, gateway_refund_id = $2, updated_at = now()
		                            WHERE refund_id = $1`, refundID, gwID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE payment p SET updated_at = now(),
			       status = CASE WHEN p.amount_minor <= s.total THEN 'REFUNDED' ELSE 'PARTIALLY_REFUNDED' END
			  FROM (SELECT COALESCE(sum(amount_minor), 0) AS total FROM refund WHERE payment_id = $1 AND status = 'SUCCEEDED') s
			 WHERE p.payment_id = $1 AND p.status IN ('CAPTURED','PARTIALLY_REFUNDED','FAILED') AND p.captured_at IS NOT NULL`, paymentID)
		return err
	})
}

// Refunder is the consumer group "refunder" on inventory-events (inside the
// reconciler role, W13). Cancelled and ConfirmFailed become refund rows
// (idempotent by idem_key = pnr:cancel_id | event_id) before the offset is
// committed; then the refund is issued.
type Refunder struct {
	Brokers []string
	DB      *pgxpool.Pool
	Store   *booking.Store
	Cat     *catalog.Catalog
	Refunds *Refunds
	Log     *slog.Logger
}

func (f *Refunder) Run(ctx context.Context) error {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(f.Brokers...),
		kgo.ConsumerGroup("refunder"),
		kgo.ConsumeTopics(kafka.TopicInventoryEvents),
		kgo.FetchIsolationLevel(kgo.ReadCommitted()),
		kgo.DisableAutoCommit(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	if err != nil {
		return err
	}
	defer cl.Close()
	for ctx.Err() == nil {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			break
		}
		fetches.EachError(func(_ string, p int32, err error) { f.Log.Warn("refunder fetch", "partition", p, "err", err) })
		var done []*kgo.Record
		fetches.EachRecord(func(rec *kgo.Record) {
			var ev bookingv1.Event
			if err := proto.Unmarshal(rec.Value, &ev); err != nil {
				f.Log.Error("refunder: bad event", "offset", rec.Offset, "err", err)
				done = append(done, rec)
				return
			}
			for attempt := 1; ctx.Err() == nil; attempt++ {
				err := f.handle(ctx, &ev)
				if err == nil {
					break
				}
				if attempt >= 20 {
					f.Log.Error("PAGE refunder gave up on event", "event_id", ev.EventId, "err", err)
					break
				}
				f.Log.Warn("refunder retry", "event_id", ev.EventId, "attempt", attempt, "err", err)
				sleep(ctx, time.Duration(min(attempt, 10))*time.Second)
			}
			done = append(done, rec)
		})
		if len(done) > 0 {
			if err := cl.CommitRecords(ctx, done...); err != nil {
				f.Log.Warn("refunder commit", "err", err)
			}
		}
	}
	return ctx.Err()
}

type paidInfo struct {
	paymentID string
	amount    int64
}

func (f *Refunder) captured(ctx context.Context, pnr, paymentID string) (*paidInfo, error) {
	q := `SELECT payment_id::text, amount_minor FROM payment WHERE pnr = $1 AND captured_at IS NOT NULL
	        AND status IN ('CAPTURED','REFUNDED','PARTIALLY_REFUNDED') ORDER BY captured_at LIMIT 1`
	args := []any{pnr}
	if paymentID != "" {
		q = `SELECT payment_id::text, amount_minor FROM payment WHERE payment_id = $1::uuid AND captured_at IS NOT NULL`
		args = []any{paymentID}
	}
	p := &paidInfo{}
	err := f.DB.QueryRow(ctx, q, args...).Scan(&p.paymentID, &p.amount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// tripInfo returns the passenger count, the boarding time and the class, from
// the projection or hold:{pnr}.
func (f *Refunder) tripInfo(ctx context.Context, journeyID int64, pnr string) (nPax int, departs time.Time, class string, err error) {
	var fromIdx int
	err = f.DB.QueryRow(ctx, `SELECT b.from_idx, b.class_code, (SELECT count(*) FROM ticket t WHERE t.pnr = b.pnr AND t.run_date = b.run_date)
	                           FROM booking b WHERE b.pnr = $1 LIMIT 1`, pnr).Scan(&fromIdx, &class, &nPax)
	if errors.Is(err, pgx.ErrNoRows) {
		h, ok, herr := f.Store.Hold(ctx, pnr)
		if herr != nil || !ok {
			return 0, time.Time{}, "", fmt.Errorf("pnr %s not projected yet", pnr)
		}
		fromIdx, class, nPax, err = h.FromIdx, h.ClassCode, len(h.Passengers), nil
	}
	if err != nil {
		return 0, time.Time{}, "", err
	}
	j, err := f.Cat.Journey(ctx, journeyID)
	if err != nil {
		return 0, time.Time{}, "", err
	}
	departs = j.DepartsAt
	if t, terr := f.Cat.Train(j.TrainNo); terr == nil {
		departs = f.Cat.StopTime(j, t, fromIdx, false)
	}
	return nPax, departs, class, nil
}

func confirmFailedReason(r string) string {
	switch r {
	case inventory.ReasonSalesClosed:
		return fares.ReasonSalesClosed
	case inventory.ReasonAlreadyConfirmed:
		return fares.ReasonDuplicateCapture
	case inventory.ReasonAmountMismatch:
		return fares.ReasonAmountMismatch
	}
	return fares.ReasonConfirmFailed
}

func (f *Refunder) handle(ctx context.Context, ev *bookingv1.Event) error {
	var pnr, paymentID, reason, idem string
	var seqs int
	switch b := ev.Body.(type) {
	case *bookingv1.Event_ConfirmFailed:
		pnr, paymentID, idem = b.ConfirmFailed.Pnr, b.ConfirmFailed.PaymentId, ev.EventId
		reason = confirmFailedReason(b.ConfirmFailed.Reason)
	case *bookingv1.Event_Cancelled:
		pnr, reason, seqs = b.Cancelled.Pnr, fares.ReasonCancelled, len(b.Cancelled.PassengerSeqs)
		idem = pnr + ":" + b.Cancelled.CancelId
	default:
		return nil
	}
	paid, err := f.captured(ctx, pnr, paymentID)
	if err != nil {
		return err
	}
	if paid == nil {
		return nil // nothing was captured: nothing to give back
	}
	amount := paid.amount
	if reason == fares.ReasonCancelled {
		nPax, departs, class, err := f.tripInfo(ctx, ev.JourneyId, pnr)
		if err != nil {
			return err
		}
		amount = fares.RefundAmount(paid.amount, nPax, seqs, class, departs, ev.GetAt().AsTime(), reason)
	}
	if amount <= 0 {
		return nil
	}
	if err := pg.Tx(ctx, f.DB, func(tx pgx.Tx) error {
		return InsertRefund(ctx, tx, paid.paymentID, pnr, reason, idem, amount)
	}); err != nil {
		return err
	}
	var refundID string
	if err := f.DB.QueryRow(ctx, `SELECT refund_id::text FROM refund WHERE idem_key = $1`, idem).Scan(&refundID); err != nil {
		return err
	}
	if err := f.Refunds.Issue(ctx, refundID); err != nil {
		f.Log.Warn("refund issue failed; reconciler will retry", "refund_id", refundID, "err", err)
	}
	return nil
}

// Reconciler settles stuck payments (W11) and retries refunds; leader only.
type Reconciler struct {
	DB      *pgxpool.Pool
	Svc     *Service
	Refunds *Refunds
	Log     *slog.Logger
	Every   time.Duration
}

func (r *Reconciler) Run(ctx context.Context) error {
	if r.Every == 0 {
		r.Every = time.Minute
	}
	t := time.NewTicker(r.Every)
	defer t.Stop()
	for {
		r.once(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (r *Reconciler) once(ctx context.Context) {
	rows, err := r.DB.Query(ctx, `SELECT payment_id::text, COALESCE(gateway_order_id,''), created_at FROM payment
	        WHERE status = 'INITIATED' AND created_at < now() - interval '15 minutes'
	          AND updated_at < now() - interval '1 minute'
	        ORDER BY created_at LIMIT 200`)
	if err != nil {
		r.Log.Warn("reconciler scan", "err", err)
		return
	}
	type stuck struct {
		id, order string
		created   time.Time
	}
	var list []stuck
	for rows.Next() {
		var s stuck
		if rows.Scan(&s.id, &s.order, &s.created) == nil {
			list = append(list, s)
		}
	}
	rows.Close()
	metrics.PaymentsStuck.Set(float64(len(list)))
	for _, s := range list {
		_, _ = r.DB.Exec(ctx, `UPDATE payment SET updated_at = now() WHERE payment_id = $1::uuid`, s.id)
		if s.order == "" { // the order was never created: the user never reached the gateway
			_ = r.Svc.Apply(ctx, GatewayEvent{PaymentID: s.id, Status: "FAILED"}, "")
			continue
		}
		ev, err := r.Svc.GW.OrderStatus(ctx, s.order)
		if err != nil {
			r.Log.Warn("reconciler order status", "payment_id", s.id, "err", err)
			continue
		}
		switch ev.Status {
		case "CAPTURED", "FAILED":
			if err := r.Svc.Apply(ctx, ev, ""); err != nil {
				r.Log.Warn("reconciler apply", "payment_id", s.id, "err", err)
			}
		default:
			if time.Since(s.created) > 24*time.Hour {
				r.Log.Error("PAGE payment unresolved for 24h", "payment_id", s.id)
			}
		}
	}

	// Refunds stuck in INITIATED or FAILED: exponential backoff up to 24 h, then page.
	rows, err = r.DB.Query(ctx, `SELECT refund_id::text, created_at FROM refund
	        WHERE status IN ('INITIATED','FAILED')
	          AND updated_at < now() - make_interval(secs => LEAST(86400, 30 * power(2, attempts)))
	        ORDER BY created_at LIMIT 200`)
	if err != nil {
		r.Log.Warn("refund scan", "err", err)
		return
	}
	type pending struct {
		id      string
		created time.Time
	}
	var refunds []pending
	for rows.Next() {
		var p pending
		if rows.Scan(&p.id, &p.created) == nil {
			refunds = append(refunds, p)
		}
	}
	rows.Close()
	for _, p := range refunds {
		if err := r.Refunds.Issue(ctx, p.id); err != nil {
			r.Log.Warn("refund retry failed", "refund_id", p.id, "err", err)
			if time.Since(p.created) > 24*time.Hour {
				r.Log.Error("PAGE refund not succeeded after 24h", "refund_id", p.id)
			}
		}
	}
	var n int
	_ = r.DB.QueryRow(ctx, `SELECT count(*) FROM refund WHERE status <> 'SUCCEEDED'`).Scan(&n)
	metrics.RefundsPending.Set(float64(n))
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
