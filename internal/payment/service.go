// Package payment owns money (§16, W8, W10, W11, W13): payment orders created
// only for OTP-verified holds, webhooks (CAPTURED -> Confirm through the
// outbox in one transaction), refunds and the reconciler.
package payment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/types/known/timestamppb"

	bookingv1 "railway/gen/booking/v1"
	"railway/internal/booking"
	"railway/internal/catalog"
	"railway/internal/fares"
	"railway/internal/outbox"
	"railway/internal/platform"
	"railway/internal/platform/metrics"
	"railway/internal/platform/pg"
)

var (
	ErrNotFound    = errors.New("not found")
	ErrForbidden   = errors.New("forbidden")
	ErrHoldExpired = errors.New("hold expired")
	ErrOTPRequired = errors.New("otp not verified")
	ErrBadPayload  = errors.New("bad payload")
)

// AlreadyPaidError: a captured payment exists for the PNR.
type AlreadyPaidError struct{ PaymentID string }

func (e *AlreadyPaidError) Error() string { return "already paid: " + e.PaymentID }

// Methods are the payment methods and the adapter family that serves each.
var Methods = map[string]string{
	"BKASH": "bkash", "NAGAD": "sslcommerz", "ROCKET": "sslcommerz", "UPAY": "sslcommerz",
	"VISA": "sslcommerz", "MASTERCARD": "sslcommerz", "NEXUS": "sslcommerz",
}

type View struct {
	PaymentID     string
	PNR           string
	JourneyID     int64
	UserID        int64
	Method        string
	Status        string
	AmountMinor   int64
	Currency      string
	Gateway       string
	OrderID       string
	RedirectURL   string
	RedirectUsed  bool
	HoldExpires   time.Time
	CapturedAt    *time.Time
	CreatedAt     time.Time
	BookingStatus string
}

type Service struct {
	DB     *pgxpool.Pool
	ReadDB *pgxpool.Pool
	RDB    redis.UniversalClient
	Store  *booking.Store
	Cat    *catalog.Catalog
	GW     GatewayAdapter
	GWName string // locally one adapter ("fake") serves every method
	Log    *slog.Logger
	Clock  platform.Clock
}

type holdInfo struct {
	userID, journeyID int64
	status            string
	amount            int64
	currency          string
	expires           time.Time
}

// loadHold reads hold:{pnr} (allocator-written, so it exists before the
// projection) and falls back to booking.
func (s *Service) loadHold(ctx context.Context, pnr string) (*holdInfo, error) {
	if h, ok, err := s.Store.Hold(ctx, pnr); err == nil && ok {
		return &holdInfo{h.UserID, h.JourneyID, h.Status, h.AmountMinor, h.Currency, h.ExpiresAt}, nil
	}
	var hi holdInfo
	var exp *time.Time
	err := s.ReadDB.QueryRow(ctx, `SELECT user_id, journey_id, status, amount_minor, currency, hold_expires_at
	                                FROM booking WHERE pnr = $1 LIMIT 1`, pnr).
		Scan(&hi.userID, &hi.journeyID, &hi.status, &hi.amount, &hi.currency, &exp)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if exp != nil {
		hi.expires = *exp
	}
	return &hi, nil
}

const viewCols = `payment_id::text, pnr, journey_id, user_id, method, status, amount_minor, currency, gateway,
	COALESCE(gateway_order_id,''), redirect_used_at IS NOT NULL, captured_at, created_at`

func scanView(row pgx.Row) (*View, error) {
	v := &View{}
	err := row.Scan(&v.PaymentID, &v.PNR, &v.JourneyID, &v.UserID, &v.Method, &v.Status, &v.AmountMinor, &v.Currency, &v.Gateway,
		&v.OrderID, &v.RedirectUsed, &v.CapturedAt, &v.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

// Create inserts a payment for a held, OTP-verified PNR (amount from the hold,
// never the client) and creates the gateway order (§16.1). One live payment
// per PNR: the same method returns the existing payment; a different method
// replaces the old order. A link that was already used is never re-issued.
func (s *Service) Create(ctx context.Context, userID int64, pnr, method, returnURL string) (*View, error) {
	h, err := s.loadHold(ctx, pnr)
	if err != nil {
		return nil, err
	}
	if h.userID != userID {
		return nil, ErrForbidden
	}
	var captured string
	_ = s.DB.QueryRow(ctx, `SELECT payment_id::text FROM payment WHERE pnr = $1 AND status = 'CAPTURED' LIMIT 1`, pnr).Scan(&captured)
	if captured != "" {
		return nil, &AlreadyPaidError{PaymentID: captured}
	}
	switch h.status {
	case "PENDING_PAYMENT":
	case "EXPIRED", "CONFIRM_FAILED":
		return nil, ErrHoldExpired
	default:
		return nil, &AlreadyPaidError{}
	}
	var verified *time.Time
	err = s.DB.QueryRow(ctx, `SELECT verified_at FROM booking_otp WHERE pnr = $1`, pnr).Scan(&verified)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && verified == nil) {
		return nil, ErrOTPRequired
	}
	if err != nil {
		return nil, err
	}
	if h.expires.Sub(s.Clock.Now()) < 30*time.Second {
		return nil, ErrHoldExpired // the user would likely pay too late
	}
	j, err := s.Cat.Journey(ctx, h.journeyID)
	if err != nil {
		return nil, err
	}

	var v *View
	err = pg.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		live, err := scanView(tx.QueryRow(ctx, `SELECT `+viewCols+` FROM payment WHERE pnr = $1 AND status IN ('INITIATED','CAPTURED') FOR UPDATE`, pnr))
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return err
		case live.Status == "CAPTURED":
			return &AlreadyPaidError{PaymentID: live.PaymentID}
		case live.Method == method || live.RedirectUsed:
			v = live // same method, or the user already opened that link: no second link
			return nil
		default: // a different method replaces the unused order
			if _, err := tx.Exec(ctx, `UPDATE payment SET status = 'FAILED', updated_at = now() WHERE payment_id = $1`, live.PaymentID); err != nil {
				return err
			}
		}
		v, err = scanView(tx.QueryRow(ctx, `
			INSERT INTO payment (payment_id, pnr, run_date, journey_id, user_id, method, gateway, status, amount_minor, currency)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'INITIATED', $8, $9) RETURNING `+viewCols,
			uuid.New(), pnr, j.RunDate, h.journeyID, userID, method, s.GWName, h.amount, h.currency))
		return err
	})
	if err != nil {
		return nil, err
	}
	v.HoldExpires = h.expires
	if v.RedirectUsed {
		return v, nil
	}
	orderID, redirect, err := s.GW.CreateOrder(ctx, Payment{PaymentID: v.PaymentID, PNR: pnr, Method: v.Method,
		AmountMinor: v.AmountMinor, Currency: v.Currency}, returnURL)
	if err != nil {
		return nil, fmt.Errorf("gateway create order: %w", err)
	}
	if v.OrderID == "" {
		if _, err := s.DB.Exec(ctx, `UPDATE payment SET gateway_order_id = $2, updated_at = now() WHERE payment_id = $1 AND gateway_order_id IS NULL`,
			v.PaymentID, orderID); err != nil {
			return nil, err
		}
		v.OrderID = orderID
	}
	v.RedirectURL = redirect
	return v, nil
}

// Get returns a payment (§16.2). If it is still INITIATED after 30 s, the
// gateway is asked once (rate-limited per payment) and the answer applied
// exactly like a webhook.
func (s *Service) Get(ctx context.Context, paymentID string) (*View, error) {
	if _, err := uuid.Parse(paymentID); err != nil {
		return nil, ErrNotFound
	}
	v, err := scanView(s.DB.QueryRow(ctx, `SELECT `+viewCols+` FROM payment WHERE payment_id = $1`, paymentID))
	if err != nil {
		return nil, err
	}
	now := s.Clock.Now()
	if v.Status == "INITIATED" && v.OrderID != "" && now.Sub(v.CreatedAt) > 30*time.Second {
		if ok, _ := s.RDB.SetNX(ctx, "paychk:"+paymentID, 1, 10*time.Second).Result(); ok {
			if ev, err := s.GW.OrderStatus(ctx, v.OrderID); err == nil && (ev.Status == "CAPTURED" || ev.Status == "FAILED") {
				if err := s.Apply(ctx, ev, ""); err == nil {
					v, _ = scanView(s.DB.QueryRow(ctx, `SELECT `+viewCols+` FROM payment WHERE payment_id = $1`, paymentID))
				}
			}
		}
	}
	if h, ok, _ := s.Store.Hold(ctx, v.PNR); ok {
		v.HoldExpires, v.BookingStatus = h.ExpiresAt, h.Status
	} else {
		_ = s.ReadDB.QueryRow(ctx, `SELECT status, COALESCE(hold_expires_at, 'epoch') FROM booking WHERE pnr = $1 LIMIT 1`, v.PNR).
			Scan(&v.BookingStatus, &v.HoldExpires)
	}
	return v, nil
}

// HandleWebhook verifies, dedupes and applies a gateway delivery (§16.3). A
// nil error means "safely stored": answer 2xx.
func (s *Service) HandleWebhook(ctx context.Context, gateway string, r *http.Request) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return err
	}
	if err := s.GW.VerifySignature(r, body); err != nil {
		metrics.WebhookSignatureFailures.Inc()
		return err
	}
	ev, err := s.GW.Parse(body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadPayload, err)
	}
	ev.Gateway = gateway
	return s.Apply(ctx, ev, ev.EventID)
}

// Apply moves a payment to the gateway's state in one transaction. CAPTURED
// also queues Confirm in the outbox, in the same transaction. eventID dedupes
// webhook deliveries (empty for reconciler and status checks).
func (s *Service) Apply(ctx context.Context, ev GatewayEvent, eventID string) error {
	return pg.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		if eventID != "" {
			tag, err := tx.Exec(ctx, `INSERT INTO webhook_event (gateway, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, ev.Gateway, eventID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return nil // duplicate delivery
			}
		}
		if _, err := uuid.Parse(ev.PaymentID); err != nil {
			s.Log.Warn("webhook for unknown merchant order", "payment_id", ev.PaymentID)
			return nil
		}
		var pnr, status, currency string
		var journeyID, amount int64
		err := tx.QueryRow(ctx, `SELECT pnr, journey_id, status, amount_minor, currency FROM payment WHERE payment_id = $1 FOR UPDATE`, ev.PaymentID).
			Scan(&pnr, &journeyID, &status, &amount, &currency)
		if errors.Is(err, pgx.ErrNoRows) {
			s.Log.Warn("webhook for unknown payment", "payment_id", ev.PaymentID)
			return nil
		}
		if err != nil {
			return err
		}
		// The first status callback marks the single-use link as used.
		if _, err := tx.Exec(ctx, `UPDATE payment SET redirect_used_at = COALESCE(redirect_used_at, now()) WHERE payment_id = $1`, ev.PaymentID); err != nil {
			return err
		}
		switch {
		case status == "CAPTURED" || status == "REFUNDED" || status == "PARTIALLY_REFUNDED":
			return nil // terminal
		case status == "FAILED" && ev.Status != "CAPTURED":
			return nil
		}
		switch ev.Status {
		case "CAPTURED":
			capturedAt := ev.CapturedAt
			if capturedAt.IsZero() {
				capturedAt = s.Clock.Now()
			}
			if ev.AmountMinor != amount || ev.Currency != currency {
				s.Log.Error("ALERT payment amount mismatch", "payment_id", ev.PaymentID, "want", amount, "got", ev.AmountMinor)
				if _, err := tx.Exec(ctx, `UPDATE payment SET status = 'FAILED', updated_at = now() WHERE payment_id = $1`, ev.PaymentID); err != nil {
					return err
				}
				return InsertRefund(ctx, tx, ev.PaymentID, pnr, fares.ReasonAmountMismatch, "mismatch:"+ev.PaymentID, min(ev.AmountMinor, amount))
			}
			if status == "FAILED" {
				var other bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment WHERE pnr = $1 AND payment_id <> $2
				        AND status IN ('INITIATED','CAPTURED'))`, pnr, ev.PaymentID).Scan(&other); err != nil {
					return err
				}
				if other { // a late capture of an order the user already replaced: give it back
					s.Log.Error("ALERT second capture for a PNR; refunding", "payment_id", ev.PaymentID, "pnr", pnr)
					return InsertRefund(ctx, tx, ev.PaymentID, pnr, fares.ReasonDuplicateCapture, "duplicate:"+ev.PaymentID, amount)
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE payment SET status = 'CAPTURED', captured_at = $2, updated_at = now() WHERE payment_id = $1`,
				ev.PaymentID, capturedAt); err != nil {
				return err
			}
			return outbox.AddCommand(ctx, tx, &bookingv1.Command{
				RequestId: platform.NewRequestID(), JourneyId: journeyID, SentAt: timestamppb.Now(),
				Body: &bookingv1.Command_Confirm{Confirm: &bookingv1.Confirm{
					Pnr: pnr, PaymentId: ev.PaymentID, AmountMinor: amount, CapturedAt: timestamppb.New(capturedAt)}},
			})
		case "FAILED":
			_, err := tx.Exec(ctx, `UPDATE payment SET status = 'FAILED', updated_at = now() WHERE payment_id = $1 AND status = 'INITIATED'`, ev.PaymentID)
			return err
		}
		return nil
	})
}

// InsertRefund records a refund row (idempotent by idem_key); the refunder or
// the reconciler issues it.
func InsertRefund(ctx context.Context, tx pgx.Tx, paymentID, pnr, reason, idem string, amount int64) error {
	if amount <= 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO refund (refund_id, payment_id, pnr, reason, idem_key, amount_minor, status)
	                         VALUES ($1, $2, $3, $4, $5, $6, 'INITIATED') ON CONFLICT (idem_key) DO NOTHING`,
		uuid.New(), paymentID, pnr, reason, idem, amount)
	return err
}
