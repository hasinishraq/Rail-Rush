package payment

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// GatewayEvent is the normalized form every adapter turns its payload into.
type GatewayEvent struct {
	Gateway        string
	EventID        string // gateway's unique delivery id
	GatewayOrderID string
	PaymentID      string // our merchant order id
	Status         string // CAPTURED | FAILED | REFUNDED | PENDING
	AmountMinor    int64
	Currency       string
	CapturedAt     time.Time
}

type Payment struct {
	PaymentID      string
	PNR            string
	Method         string // BKASH | NAGAD | ...
	AmountMinor    int64
	Currency       string
	GatewayOrderID string
}

type Refund struct {
	RefundID       string
	PaymentID      string
	GatewayOrderID string
	AmountMinor    int64
	Currency       string
	IdemKey        string
}

type GatewayAdapter interface {
	VerifySignature(r *http.Request, body []byte) error // HMAC / RSA per gateway
	Parse(body []byte) (GatewayEvent, error)
	CreateOrder(ctx context.Context, p Payment, returnURL string) (orderID, redirectURL string, err error)
	OrderStatus(ctx context.Context, orderID string) (GatewayEvent, error)
	Refund(ctx context.Context, r Refund) (gatewayRefundID string, err error)
}

var ErrBadSignature = errors.New("bad webhook signature")

// FakeAdapter talks to cmd/fakegateway: JSON over HTTP, webhooks signed with
// HMAC-SHA256 (hex) of the raw body in X-Signature.
type FakeAdapter struct {
	Name    string
	BaseURL string
	Secret  []byte
	HTTP    *http.Client
}

func NewFakeAdapter(name, baseURL, secret string) *FakeAdapter {
	return &FakeAdapter{Name: name, BaseURL: baseURL, Secret: []byte(secret), HTTP: &http.Client{Timeout: 5 * time.Second}}
}

// Sign returns the hex HMAC of body; shared with the fake gateway.
func Sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func (f *FakeAdapter) VerifySignature(r *http.Request, body []byte) error {
	got, err := hex.DecodeString(r.Header.Get("X-Signature"))
	if err != nil || !hmac.Equal(got, mustHex(Sign(f.Secret, body))) {
		return ErrBadSignature
	}
	return nil
}

func mustHex(s string) []byte { b, _ := hex.DecodeString(s); return b }

// FakeWire is the JSON shape of fake gateway orders and webhooks.
type FakeWire struct {
	EventID         string    `json:"event_id,omitempty"`
	OrderID         string    `json:"order_id"`
	MerchantOrderID string    `json:"merchant_order_id"`
	Method          string    `json:"method,omitempty"`
	Status          string    `json:"status"`
	AmountMinor     int64     `json:"amount_minor"`
	Currency        string    `json:"currency"`
	CapturedAt      time.Time `json:"captured_at,omitempty"`
	ReturnURL       string    `json:"return_url,omitempty"`
	RedirectURL     string    `json:"redirect_url,omitempty"`
}

func (f *FakeAdapter) toEvent(w FakeWire) GatewayEvent {
	return GatewayEvent{Gateway: f.Name, EventID: w.EventID, GatewayOrderID: w.OrderID, PaymentID: w.MerchantOrderID,
		Status: w.Status, AmountMinor: w.AmountMinor, Currency: w.Currency, CapturedAt: w.CapturedAt}
}

func (f *FakeAdapter) Parse(body []byte) (GatewayEvent, error) {
	var w FakeWire
	if err := json.Unmarshal(body, &w); err != nil {
		return GatewayEvent{}, err
	}
	if w.EventID == "" || w.MerchantOrderID == "" {
		return GatewayEvent{}, errors.New("missing event_id or merchant_order_id")
	}
	return f.toEvent(w), nil
}

func (f *FakeAdapter) do(ctx context.Context, method, path, idem string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("gateway %s %s: %d %s", method, path, resp.StatusCode, bytes.TrimSpace(b))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (f *FakeAdapter) CreateOrder(ctx context.Context, p Payment, returnURL string) (string, string, error) {
	var out FakeWire
	err := f.do(ctx, http.MethodPost, "/orders", p.PaymentID, FakeWire{MerchantOrderID: p.PaymentID, Method: p.Method,
		AmountMinor: p.AmountMinor, Currency: p.Currency, ReturnURL: returnURL}, &out)
	return out.OrderID, out.RedirectURL, err
}

func (f *FakeAdapter) OrderStatus(ctx context.Context, orderID string) (GatewayEvent, error) {
	var out FakeWire
	if err := f.do(ctx, http.MethodGet, "/orders/"+orderID, "", nil, &out); err != nil {
		return GatewayEvent{}, err
	}
	return f.toEvent(out), nil
}

func (f *FakeAdapter) Refund(ctx context.Context, r Refund) (string, error) {
	var out struct {
		RefundID string `json:"refund_id"`
		Status   string `json:"status"`
	}
	err := f.do(ctx, http.MethodPost, "/refunds", r.IdemKey, map[string]any{
		"order_id": r.GatewayOrderID, "amount_minor": r.AmountMinor, "currency": r.Currency, "idempotency_key": r.IdemKey,
	}, &out)
	if err == nil && out.Status != "SUCCEEDED" {
		err = fmt.Errorf("refund status %s", out.Status)
	}
	return out.RefundID, err
}
