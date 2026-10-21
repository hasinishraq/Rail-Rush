// Command fakegateway is the local payment gateway stand-in: orders, a pay page,
// signed webhooks with configurable delay, failure and lost-webhook ("unknown
// status") rates, order status queries and refunds.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"railway/internal/payment"
)

type order struct {
	payment.FakeWire
	refunded int64
}

type gateway struct {
	mu        sync.Mutex
	orders    map[string]*order // order_id -> order
	byMerch   map[string]string // merchant_order_id -> order_id
	refunds   map[string]map[string]any
	publicURL string
	webhook   string
	secret    []byte
	delay     time.Duration
	failRate  float64
	lostRate  float64
	log       *slog.Logger
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	delay, _ := time.ParseDuration(env("DELAY", "500ms"))
	fr, _ := strconv.ParseFloat(env("FAIL_RATE", "0"), 64)
	ur, _ := strconv.ParseFloat(env("UNKNOWN_RATE", "0"), 64)
	secret := env("SECRET", "")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "missing required env: SECRET (same value as the app's GATEWAY_SECRET)")
		os.Exit(1)
	}
	g := &gateway{
		orders: map[string]*order{}, byMerch: map[string]string{}, refunds: map[string]map[string]any{},
		publicURL: env("PUBLIC_URL", "http://localhost:8090"), webhook: env("WEBHOOK_URL", "http://localhost:8080/v1/webhooks/payment/fake"),
		secret: []byte(secret), delay: delay, failRate: fr, lostRate: ur,
		log: slog.New(slog.NewJSONHandler(os.Stdout, nil)),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", g.createOrder)
	mux.HandleFunc("GET /orders/{id}", g.getOrder)
	mux.HandleFunc("POST /orders/{id}/pay", g.payAPI)
	mux.HandleFunc("GET /pay/{id}", g.payPage)
	mux.HandleFunc("POST /refunds", g.refund)
	addr := env("ADDR", ":8090")
	g.log.Info("fake gateway listening", "addr", addr, "fail_rate", fr, "unknown_rate", ur, "delay", delay)
	if err := http.ListenAndServe(addr, mux); err != nil {
		g.log.Error("listen", "err", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (g *gateway) createOrder(w http.ResponseWriter, r *http.Request) {
	var in payment.FakeWire
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.MerchantOrderID == "" || in.AmountMinor <= 0 {
		writeJSON(w, 400, map[string]string{"error": "bad order"})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if id, ok := g.byMerch[in.MerchantOrderID]; ok { // idempotent by merchant order id
		writeJSON(w, 200, g.orders[id].FakeWire)
		return
	}
	o := &order{FakeWire: in}
	o.OrderID = "ord_" + uuid.NewString()[:13]
	o.Status = "PENDING"
	o.RedirectURL = g.publicURL + "/pay/" + o.OrderID
	g.orders[o.OrderID], g.byMerch[in.MerchantOrderID] = o, o.OrderID
	writeJSON(w, 201, o.FakeWire)
}

func (g *gateway) getOrder(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	o := g.orders[r.PathValue("id")]
	var out payment.FakeWire
	if o != nil {
		out = o.FakeWire
	}
	g.mu.Unlock()
	if o == nil {
		writeJSON(w, 404, map[string]string{"error": "unknown order"})
		return
	}
	writeJSON(w, 200, out)
}

// pay settles an order: CAPTURED or FAILED, then the webhook after DELAY
// (or never, for the "unknown status" share — the reconciler must find it).
func (g *gateway) pay(id, outcome string) (*payment.FakeWire, error) {
	g.mu.Lock()
	o := g.orders[id]
	if o == nil {
		g.mu.Unlock()
		return nil, fmt.Errorf("unknown order")
	}
	if o.Status != "PENDING" {
		out := o.FakeWire
		g.mu.Unlock()
		return &out, nil
	}
	if outcome == "" {
		switch x := rand.Float64(); {
		case x < g.failRate:
			outcome = "fail"
		case x < g.failRate+g.lostRate:
			outcome = "unknown"
		default:
			outcome = "success"
		}
	}
	if outcome == "fail" {
		o.Status = "FAILED"
	} else {
		o.Status, o.CapturedAt = "CAPTURED", time.Now().UTC()
	}
	out := o.FakeWire
	g.mu.Unlock()
	if outcome != "unknown" {
		go g.sendWebhook(out)
	} else {
		g.log.Info("webhook deliberately lost", "order_id", id)
	}
	return &out, nil
}

func (g *gateway) sendWebhook(o payment.FakeWire) {
	time.Sleep(g.delay)
	o.EventID = "evt_" + uuid.NewString()
	o.RedirectURL, o.ReturnURL = "", ""
	body, _ := json.Marshal(o)
	for attempt := 1; attempt <= 6; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, g.webhook, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Signature", payment.Sign(g.secret, body))
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				g.log.Info("webhook delivered", "order_id", o.OrderID, "status", o.Status)
				return
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		g.log.Warn("webhook failed; retrying", "order_id", o.OrderID, "attempt", attempt, "err", err)
		time.Sleep(time.Duration(attempt*attempt) * time.Second)
	}
}

func (g *gateway) payPage(w http.ResponseWriter, r *http.Request) {
	o, err := g.pay(r.PathValue("id"), r.URL.Query().Get("outcome"))
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	g.mu.Lock()
	ret := g.orders[o.OrderID].ReturnURL
	g.mu.Unlock()
	if ret != "" {
		http.Redirect(w, r, ret+"?payment_id="+o.MerchantOrderID, http.StatusFound)
		return
	}
	fmt.Fprintf(w, "payment %s: %s\n", o.MerchantOrderID, o.Status)
}

// POST /orders/{id}/pay?outcome=success|fail|unknown — scripted payments for tests.
func (g *gateway) payAPI(w http.ResponseWriter, r *http.Request) {
	o, err := g.pay(r.PathValue("id"), r.URL.Query().Get("outcome"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, o)
}

func (g *gateway) refund(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OrderID        string `json:"order_id"`
		AmountMinor    int64  `json:"amount_minor"`
		Currency       string `json:"currency"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.IdempotencyKey == "" {
		writeJSON(w, 400, map[string]string{"error": "bad refund"})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if prev, ok := g.refunds[in.IdempotencyKey]; ok {
		writeJSON(w, 200, prev)
		return
	}
	o := g.orders[in.OrderID]
	if o == nil || o.Status != "CAPTURED" || o.refunded+in.AmountMinor > o.AmountMinor {
		writeJSON(w, 422, map[string]string{"error": "not refundable"})
		return
	}
	o.refunded += in.AmountMinor
	out := map[string]any{"refund_id": "rfd_" + uuid.NewString()[:13], "status": "SUCCEEDED", "amount_minor": in.AmountMinor}
	g.refunds[in.IdempotencyKey] = out
	writeJSON(w, 200, out)
}
