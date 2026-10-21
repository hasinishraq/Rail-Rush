// Package otp issues, verifies and resends the one-time passwords sent at
// passenger details (§14, W7, W8). Codes are 6 digits from crypto/rand and are
// stored only as HMAC(OTP_KEY, pnr‖code); the SMS goes out through the outbox.
package otp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"railway/internal/outbox"
	"railway/internal/platform"
	"railway/internal/platform/kafka"
	"railway/internal/platform/metrics"
	"railway/internal/platform/pg"
	"railway/internal/wire"
)

var (
	ErrNotFound  = errors.New("otp not found")
	ErrForbidden = errors.New("not the owner")
	ErrExpired   = errors.New("otp expired")
)

// LockedError: too many attempts or resends.
type LockedError struct{ RetryAfter time.Duration }

func (e *LockedError) Error() string { return "otp locked" }

// InvalidError: wrong code.
type InvalidError struct{ AttemptsLeft int }

func (e *InvalidError) Error() string { return "otp invalid" }

// Info is what the client is told about a sent code.
type Info struct {
	SentTo      string    `json:"sent_to"`    // "+88017•••••678"
	ExpiresAt   time.Time `json:"expires_at"` // 3 min
	ResendAfter time.Time `json:"resend_after"`
}

const (
	resendGap      = 60 * time.Second
	userSendsPerHr = 10
)

type Service struct {
	DB          *pgxpool.Pool
	RDB         redis.UniversalClient
	Key         []byte
	TTL         time.Duration
	MaxAttempts int
	MaxResends  int
	Clock       platform.Clock
	// DevCode, when set, is accepted for every PNR besides the texted code.
	// Local development only (OTP_DEV_CODE), where no real SMS goes out.
	DevCode string
}

// SMS is the notifications payload for a text message.
type SMS struct {
	Kind    string `json:"kind"` // "SMS"
	UserID  int64  `json:"user_id"`
	Phone   string `json:"phone,omitempty"`
	PNR     string `json:"pnr,omitempty"`
	Purpose string `json:"purpose"` // OTP | TICKET | SEATS_CHANGED | REFUND ...
	Text    string `json:"text"`
}

// QueueSMS adds an SMS to the notifications topic through the outbox.
func QueueSMS(ctx context.Context, tx pgx.Tx, m SMS) error {
	m.Kind = "SMS"
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return outbox.Add(ctx, tx, kafka.TopicNotifications, strconv.FormatInt(m.UserID, 10), b)
}

// Mask shows the country prefix and the last three digits of a phone number.
func Mask(phone string) string {
	if len(phone) < 9 {
		return phone
	}
	return phone[:6] + "•••••" + phone[len(phone)-3:]
}

func newCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func (s *Service) hash(pnr, code string) []byte {
	m := hmac.New(sha256.New, s.Key)
	m.Write([]byte(pnr))
	m.Write([]byte(code))
	return m.Sum(nil)
}

func text(pnr, code string, ttl time.Duration) string {
	return fmt.Sprintf("RailRush: your OTP for PNR %s is %s. It expires in %d minutes. Never share it.", pnr, code, int(ttl.Minutes()))
}

func (s *Service) info(phone string, expires time.Time) Info {
	return Info{SentTo: Mask(phone), ExpiresAt: expires, ResendAfter: expires.Add(-s.TTL).Add(resendGap)}
}

// userQuota counts sends per user per hour (otp:rl:{user}); it fails open.
func (s *Service) userQuota(ctx context.Context, userID int64) (bool, time.Duration) {
	k := wire.OTPRateKey(userID)
	n, err := s.RDB.Incr(ctx, k).Result()
	if err != nil {
		return true, 0
	}
	if n == 1 {
		s.RDB.Expire(ctx, k, time.Hour)
	}
	if n > userSendsPerHr {
		ttl, _ := s.RDB.TTL(ctx, k).Result()
		return false, max(ttl, time.Second)
	}
	return true, 0
}

// Issue creates the OTP for a new hold inside tx and queues its SMS. A retry
// for the same PNR finds the row and sends nothing (no second OTP).
func (s *Service) Issue(ctx context.Context, tx pgx.Tx, pnr string, userID int64, phone string) (Info, error) {
	now := s.Clock.Now()
	code, err := newCode()
	if err != nil {
		return Info{}, err
	}
	exp := now.Add(s.TTL)
	tag, err := tx.Exec(ctx, `INSERT INTO booking_otp (pnr, user_id, code_hash, sent_to, max_attempts, expires_at)
	        VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (pnr) DO NOTHING`, pnr, userID, s.hash(pnr, code), phone, s.MaxAttempts, exp)
	if err != nil {
		return Info{}, err
	}
	if tag.RowsAffected() == 0 {
		var sentTo string
		if err := tx.QueryRow(ctx, `SELECT sent_to, expires_at FROM booking_otp WHERE pnr = $1`, pnr).Scan(&sentTo, &exp); err != nil {
			return Info{}, err
		}
		return s.info(sentTo, exp), nil
	}
	s.userQuota(ctx, userID)
	metrics.OTPSent.WithLabelValues("issue").Inc()
	if err := QueueSMS(ctx, tx, SMS{UserID: userID, Phone: phone, PNR: pnr, Purpose: "OTP", Text: text(pnr, code, s.TTL)}); err != nil {
		return Info{}, err
	}
	return s.info(phone, exp), nil
}

// Status reports whether the OTP of a PNR was verified, and its info.
func (s *Service) Status(ctx context.Context, db *pgxpool.Pool, pnr string) (verified bool, info Info, found bool, err error) {
	var sentTo string
	var exp time.Time
	var at *time.Time
	err = db.QueryRow(ctx, `SELECT sent_to, expires_at, verified_at FROM booking_otp WHERE pnr = $1`, pnr).Scan(&sentTo, &exp, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, Info{}, false, nil
	}
	if err != nil {
		return false, Info{}, false, err
	}
	return at != nil, s.info(sentTo, exp), true, nil
}

// Verify checks a code in one transaction (§14.2). The hold timer is not
// changed: OTP and payment share the same 10 minutes.
func (s *Service) Verify(ctx context.Context, pnr string, userID int64, code string) error {
	var result error
	err := pg.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var owner int64
		var hash []byte
		var attempts, maxAttempts int
		var exp time.Time
		var verified *time.Time
		err := tx.QueryRow(ctx, `SELECT user_id, code_hash, attempts, max_attempts, expires_at, verified_at
		        FROM booking_otp WHERE pnr = $1 FOR UPDATE`, pnr).Scan(&owner, &hash, &attempts, &maxAttempts, &exp, &verified)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			result = ErrNotFound
			return nil
		case err != nil:
			return err
		case owner != userID:
			result = ErrForbidden
			return nil
		case verified != nil:
			return nil // idempotent
		case s.Clock.Now().After(exp):
			result = ErrExpired
			return nil
		case attempts >= maxAttempts:
			result = &LockedError{RetryAfter: resendGap}
			return nil
		}
		dev := s.DevCode != "" && subtle.ConstantTimeCompare([]byte(code), []byte(s.DevCode)) == 1
		if !dev && !hmac.Equal(hash, s.hash(pnr, code)) { // constant time
			attempts++
			if _, err := tx.Exec(ctx, `UPDATE booking_otp SET attempts = $2 WHERE pnr = $1`, pnr, attempts); err != nil {
				return err
			}
			result = &InvalidError{AttemptsLeft: maxAttempts - attempts}
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE booking_otp SET verified_at = now() WHERE pnr = $1`, pnr)
		return err
	})
	if err != nil {
		return err
	}
	switch result.(type) {
	case nil:
		metrics.OTPVerify.WithLabelValues("ok").Inc()
	case *InvalidError:
		metrics.OTPVerify.WithLabelValues("invalid").Inc()
	default:
		metrics.OTPVerify.WithLabelValues("refused").Inc()
	}
	return result
}

// Resend replaces the code (§14.3): 60 s after the last send, at most
// MaxResends per PNR and 10 sends per user per hour. Attempts are reset and
// the code gets a new 3-minute expiry.
func (s *Service) Resend(ctx context.Context, pnr string, userID int64) (Info, error) {
	now := s.Clock.Now()
	var out Info
	var result error
	err := pg.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var owner int64
		var resends int
		var phone string
		var exp time.Time
		var verified *time.Time
		err := tx.QueryRow(ctx, `SELECT user_id, resends, sent_to, expires_at, verified_at FROM booking_otp WHERE pnr = $1 FOR UPDATE`, pnr).
			Scan(&owner, &resends, &phone, &exp, &verified)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			result = ErrNotFound
			return nil
		case err != nil:
			return err
		case owner != userID:
			result = ErrForbidden
			return nil
		case verified != nil:
			out = s.info(phone, exp)
			return nil
		}
		if next := exp.Add(-s.TTL).Add(resendGap); now.Before(next) {
			result = &LockedError{RetryAfter: next.Sub(now)}
			return nil
		}
		if resends >= s.MaxResends {
			result = &LockedError{RetryAfter: time.Until(exp.Add(time.Hour))}
			return nil
		}
		if ok, wait := s.userQuota(ctx, userID); !ok {
			result = &LockedError{RetryAfter: wait}
			return nil
		}
		code, err := newCode()
		if err != nil {
			return err
		}
		exp = now.Add(s.TTL)
		if _, err := tx.Exec(ctx, `UPDATE booking_otp SET code_hash = $2, attempts = 0, resends = resends + 1, expires_at = $3 WHERE pnr = $1`,
			pnr, s.hash(pnr, code), exp); err != nil {
			return err
		}
		metrics.OTPSent.WithLabelValues("resend").Inc()
		out = s.info(phone, exp)
		return QueueSMS(ctx, tx, SMS{UserID: userID, Phone: phone, PNR: pnr, Purpose: "OTP", Text: text(pnr, code, s.TTL)})
	})
	if err != nil {
		return Info{}, err
	}
	return out, result
}
