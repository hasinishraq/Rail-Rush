// Package notify delivers the notifications topic: OTP and ticket SMS through
// the configured SMS gateways, primary first, secondary on failure (§21). It
// runs as consumer group "notifier" inside role=relay.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"railway/internal/otp"
	"railway/internal/platform/kafka"
	"railway/internal/platform/metrics"
)

// SMSSender sends one text message.
type SMSSender interface {
	Send(ctx context.Context, phone, text string) error
}

// HTTPSender posts {"to","text"} to each provider's /send in order until one accepts.
type HTTPSender struct {
	Providers []string
	HTTP      *http.Client
}

func NewHTTPSender(providers []string) *HTTPSender {
	return &HTTPSender{Providers: providers, HTTP: &http.Client{Timeout: 5 * time.Second}}
}

func (s *HTTPSender) Send(ctx context.Context, phone, text string) error {
	if len(s.Providers) == 0 {
		return errors.New("no SMS provider configured")
	}
	body, _ := json.Marshal(map[string]string{"to": phone, "text": text})
	var errs []error
	for _, p := range s.Providers {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p+"/send", bytes.NewReader(body))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.HTTP.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode < 300 {
				metrics.SMSSent.WithLabelValues(p, "ok").Inc()
				return nil
			}
			err = fmt.Errorf("%s: status %d", p, resp.StatusCode)
		}
		metrics.SMSSent.WithLabelValues(p, "error").Inc()
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Notifier consumes the notifications topic and sends each SMS once.
type Notifier struct {
	Brokers []string
	DB      *pgxpool.Pool
	SMS     SMSSender
	Log     *slog.Logger
}

func (n *Notifier) Run(ctx context.Context) error {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(n.Brokers...),
		kgo.ConsumerGroup("notifier"),
		kgo.ConsumeTopics(kafka.TopicNotifications),
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
		fetches.EachError(func(_ string, p int32, err error) { n.Log.Warn("notifier fetch", "partition", p, "err", err) })
		var done []*kgo.Record
		fetches.EachRecord(func(r *kgo.Record) {
			n.deliver(ctx, r.Value)
			done = append(done, r)
		})
		if len(done) > 0 {
			if err := cl.CommitRecords(ctx, done...); err != nil {
				n.Log.Warn("notifier commit", "err", err)
			}
		}
	}
	return ctx.Err()
}

// deliver retries a few times, then gives up: an OTP can be resent and a
// booking SMS is not worth blocking the partition for. Codes are never logged.
func (n *Notifier) deliver(ctx context.Context, payload []byte) {
	var m otp.SMS
	if err := json.Unmarshal(payload, &m); err != nil || m.Kind != "SMS" {
		return
	}
	if m.Phone == "" && m.UserID != 0 {
		_ = n.DB.QueryRow(ctx, `SELECT phone FROM app_user WHERE user_id = $1`, m.UserID).Scan(&m.Phone)
	}
	if m.Phone == "" {
		n.Log.Warn("sms without a phone dropped", "user_id", m.UserID, "purpose", m.Purpose)
		return
	}
	for attempt := 1; attempt <= 3 && ctx.Err() == nil; attempt++ {
		err := n.SMS.Send(ctx, m.Phone, m.Text)
		if err == nil {
			return
		}
		n.Log.Warn("sms failed", "purpose", m.Purpose, "pnr", m.PNR, "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
}
