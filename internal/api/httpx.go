package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"railway/internal/api/dto"
	"railway/internal/auth"
	"railway/internal/platform"
	"railway/internal/platform/metrics"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyClaims
)

func requestID(r *http.Request) string {
	if v, ok := r.Context().Value(keyRequestID).(string); ok {
		return v
	}
	return ""
}

func claims(r *http.Request) *auth.AccessClaims {
	c, _ := r.Context().Value(keyClaims).(*auth.AccessClaims)
	return c
}

func withValue(r *http.Request, k ctxKey, v any) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), k, v))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRaw(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// Error codes -> HTTP status (§9.5).
var codeStatus = map[string]int{
	"BAD_REQUEST": 400, "VALIDATION_FAILED": 422, "UNAUTHENTICATED": 401, "TOKEN_EXPIRED": 401,
	"INVALID_CREDENTIALS": 401, "REFRESH_REUSED": 401, "FORBIDDEN": 403, "ADMISSION_REQUIRED": 403,
	"ADMISSION_INVALID": 403, "CAPTCHA_INVALID": 403, "ADMISSION_USED": 403, "NOT_FOUND": 404, "IDEMPOTENCY_KEY_REUSED": 422,
	"NOT_OPEN": 409, "SALES_CLOSED": 409, "SOLD_OUT": 409, "SEAT_TAKEN": 409, "SEAT_NOT_IN_POOL": 422,
	"SELECTION_LIMIT": 409, "CART_CONFLICT": 409, "EMPTY_CART": 409, "PASSENGER_COUNT_MISMATCH": 422,
	"ACTIVE_HOLD_EXISTS": 409, "ORDER_LIMIT_EXCEEDED": 409, "OTP_INVALID": 422, "OTP_EXPIRED": 409,
	"OTP_LOCKED": 429, "OTP_REQUIRED": 409, "HOLD_EXPIRED": 409, "ALREADY_PAID": 409, "NOT_CANCELLABLE": 409,
	"REQUEST_IN_PROGRESS": 409, "TICKET_INVALID": 422, "RATE_LIMITED": 429, "OVERLOADED": 429, "LOG_UNAVAILABLE": 503,
	"UNAVAILABLE": 503, "INTERNAL": 500,
}

func errorBody(r *http.Request, code, msg string, details map[string]any) dto.ErrorResponse {
	return dto.ErrorResponse{Error: dto.APIError{Code: code, Message: msg, RequestID: requestID(r), Details: details}}
}

// fail writes the error envelope; Retry-After is taken from details.retry_after.
func fail(w http.ResponseWriter, r *http.Request, code, msg string, details map[string]any) {
	status, ok := codeStatus[code]
	if !ok {
		status = 500
	}
	if ra, ok := details["retry_after"]; ok {
		w.Header().Set("Retry-After", toStr(ra))
	}
	metrics.APIErrors.WithLabelValues(code).Inc()
	writeJSON(w, status, errorBody(r, code, msg, details))
}

func toStr(v any) string {
	switch x := v.(type) {
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case string:
		return x
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func internalErr(w http.ResponseWriter, r *http.Request, s *Server, err error) {
	s.log.Error("internal error", "err", err, "path", r.URL.Path, "request_id", requestID(r))
	fail(w, r, "INTERNAL", "internal error", nil)
}

// decode reads a JSON body (max 1 MiB); it returns the raw bytes for hashing.
func decode(w http.ResponseWriter, r *http.Request, v any) ([]byte, bool) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		fail(w, r, "BAD_REQUEST", "cannot read body", nil)
		return nil, false
	}
	if len(b) == 0 {
		b = []byte("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		fail(w, r, "BAD_REQUEST", "malformed JSON: "+err.Error(), nil)
		return nil, false
	}
	return b, true
}

func hashBody(b []byte) []byte {
	var v any
	if json.Unmarshal(b, &v) == nil { // canonical form: key order does not matter
		if c, err := json.Marshal(v); err == nil {
			b = c
		}
	}
	h := sha256.Sum256(b)
	return h[:]
}

func hexHash(b []byte) string { return hex.EncodeToString(hashBody(b)) }

func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			return strings.TrimSpace(strings.Split(xf, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func newRequestID() string { return platform.NewRequestID() }

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}
