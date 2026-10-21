package api

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"railway/internal/api/dto"
	"railway/internal/payment"
)

var pnrRe = regexp.MustCompile(`^[0-9]{10}$`)

func paymentResponse(v *payment.View) dto.PaymentResponse {
	return dto.PaymentResponse{PaymentID: v.PaymentID, PNR: v.PNR, Status: v.Status,
		Amount: dto.Money{AmountMinor: v.AmountMinor, Currency: v.Currency}, Method: v.Method,
		RedirectURL: v.RedirectURL, HoldExpires: v.HoldExpires, CapturedAt: v.CapturedAt, BookingStatus: v.BookingStatus}
}

// returnURLAllowed matches scheme and host exactly (a plain prefix would let
// "http://localhost.evil.com" through as "http://localhost"). An entry without
// a port allows any port on that host; an entry with a path is a path prefix.
func (s *Server) returnURLAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	for _, entry := range s.cfg.ReturnURLs {
		p, err := url.Parse(entry)
		if err != nil || p.Scheme != u.Scheme || !strings.EqualFold(p.Hostname(), u.Hostname()) {
			continue
		}
		if p.Port() != "" && p.Port() != u.Port() {
			continue
		}
		if strings.HasPrefix(u.Path, p.Path) {
			return true
		}
	}
	return false
}

// POST /v1/payments — owner, Idempotency-Key.
func (s *Server) createPayment(w http.ResponseWriter, r *http.Request) {
	uid := claims(r).UserID()
	key, ok := requireIdemKey(w, r)
	if !ok {
		return
	}
	if !s.rateLimit(w, r, "user:payments", strconv.FormatInt(uid, 10), 10, time.Minute) {
		return
	}
	var req dto.CreatePaymentRequest
	body, ok := decode(w, r, &req)
	if !ok {
		return
	}
	req.Method = strings.ToUpper(req.Method)
	fields := map[string]string{}
	if !pnrRe.MatchString(req.PNR) {
		fields["pnr"] = "10 digits"
	}
	if _, ok := payment.Methods[req.Method]; !ok {
		fields["method"] = "BKASH | NAGAD | ROCKET | UPAY | VISA | MASTERCARD | NEXUS"
	}
	if !s.returnURLAllowed(req.ReturnURL) {
		fields["return_url"] = "not an allow-listed app origin"
	}
	if len(fields) > 0 {
		fail(w, r, "VALIDATION_FAILED", "invalid payment request", map[string]any{"fields": fields})
		return
	}
	s.idempotent(w, r, uid, "POST /v1/payments", key, body, func() outcome {
		v, err := s.payments.Create(r.Context(), uid, req.PNR, req.Method, req.ReturnURL)
		var paid *payment.AlreadyPaidError
		switch {
		case err == nil:
			return outcome{http.StatusCreated, paymentResponse(v)}
		case errors.Is(err, payment.ErrNotFound):
			return errOutcome(r, "NOT_FOUND", "unknown pnr", nil)
		case errors.Is(err, payment.ErrForbidden):
			return errOutcome(r, "FORBIDDEN", "not your booking", nil)
		case errors.Is(err, payment.ErrOTPRequired):
			return errOutcome(r, "OTP_REQUIRED", "verify the OTP before paying", nil)
		case errors.Is(err, payment.ErrHoldExpired):
			return errOutcome(r, "HOLD_EXPIRED", "hold expired or about to expire", nil)
		case errors.As(err, &paid):
			return errOutcome(r, "ALREADY_PAID", "a captured payment exists for this pnr", map[string]any{"payment_id": paid.PaymentID})
		}
		s.log.Error("create payment", "err", err, "pnr", req.PNR)
		return errOutcome(r, "INTERNAL", "payment could not be created", nil)
	})
}

// GET /v1/payments/{paymentId} — the redirect is never trusted; this is.
func (s *Server) getPayment(w http.ResponseWriter, r *http.Request) {
	c := claims(r)
	v, err := s.payments.Get(r.Context(), r.PathValue("paymentId"))
	if errors.Is(err, payment.ErrNotFound) || (err == nil && v.UserID != c.UserID() && !isStaff(c.Role)) {
		fail(w, r, "NOT_FOUND", "unknown payment", nil)
		return
	}
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	writeJSON(w, http.StatusOK, paymentResponse(v))
}

// POST /v1/webhooks/payment/{gateway} — gateway signature on the raw body.
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	gw := r.PathValue("gateway")
	if gw != s.cfg.GatewayName() {
		fail(w, r, "NOT_FOUND", "unknown gateway", nil)
		return
	}
	err := s.payments.HandleWebhook(r.Context(), gw, r)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case errors.Is(err, payment.ErrBadSignature):
		s.log.Warn("webhook signature failure", "gateway", gw, "ip", clientIP(r, s.cfg.TrustProxy))
		fail(w, r, "UNAUTHENTICATED", "bad signature", nil)
	case errors.Is(err, payment.ErrBadPayload):
		fail(w, r, "BAD_REQUEST", err.Error(), nil)
	default:
		internalErr(w, r, s, err) // the gateway retries
	}
}
