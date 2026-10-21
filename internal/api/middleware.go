package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"railway/internal/admission"
	"railway/internal/auth"
	"railway/internal/platform/metrics"
)

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// base: X-Request-Id (generated if absent, echoed back) and panic recovery.
func (s *Server) base(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 128 {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		r = withValue(r, keyRequestID, id)
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("panic", "panic", p, "path", r.URL.Path, "request_id", id)
				fail(w, r, "INTERNAL", "internal error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// instrument: metrics plus the per-pod in-flight semaphore (429 when full).
func (s *Server) instrument(route string, fn http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		default:
			fail(w, r, "OVERLOADED", "server busy", map[string]any{"retry_after": 1})
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		fn(sw, r)
		metrics.HTTPRequests.WithLabelValues(route, strconv.Itoa(sw.status)).Inc()
		metrics.HTTPDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
	})
}

// user requires a valid access JWT that is not on the denylist.
func (s *Server) user(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			fail(w, r, "UNAUTHENTICATED", "missing bearer token", nil)
			return
		}
		c, err := s.verifier.Verify(r.Context(), strings.TrimPrefix(h, "Bearer "))
		switch {
		case errors.Is(err, auth.ErrTokenExpired):
			fail(w, r, "TOKEN_EXPIRED", "access token expired; refresh it", nil)
			return
		case err != nil:
			fail(w, r, "UNAUTHENTICATED", "invalid token", nil)
			return
		}
		next(w, withValue(r, keyClaims, c))
	}
}

// admitted checks the admission token for the current user. It returns the
// claims, or writes the error and returns nil.
func (s *Server) admitted(w http.ResponseWriter, r *http.Request) (*admission.Claims, bool) {
	tok := r.Header.Get("X-Admission-Token")
	c, err := s.admit.Verify(r.Context(), tok, claims(r).UserID())
	var used *admission.UsedError
	switch {
	case err == nil:
		return c, true
	case errors.Is(err, admission.ErrRequired):
		fail(w, r, "ADMISSION_REQUIRED", "join the waiting room first", map[string]any{"queue_url": s.cfg.AdmissionQueueURL})
	case errors.Is(err, admission.ErrInvalid):
		fail(w, r, "ADMISSION_INVALID", "admission token invalid, expired or bound to another user", map[string]any{"queue_url": s.cfg.AdmissionQueueURL})
	case errors.As(err, &used):
		fail(w, r, "ADMISSION_USED", "admission token already used for a booking", map[string]any{"pnr": used.PNR})
	default:
		internalErr(w, r, s, err)
	}
	return nil, false
}

// admittedUser is User + a valid admission token bound to this user (§9.3).
func (s *Server) admittedUser(next func(http.ResponseWriter, *http.Request, *admission.Claims)) http.HandlerFunc {
	return s.user(func(w http.ResponseWriter, r *http.Request) {
		adm, ok := s.admitted(w, r)
		if !ok {
			return
		}
		next(w, r, adm)
	})
}

// internal requires X-Internal-Token.
func (s *Server) internal(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Internal-Token")
		if s.cfg.InternalToken == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.InternalToken)) != 1 {
			fail(w, r, "FORBIDDEN", "internal token required", nil)
			return
		}
		next(w, r)
	}
}

// limit is a fixed-window counter in Redis. It fails open when Redis is down.
func (s *Server) limit(ctx context.Context, scope, id string, n int, window time.Duration) (bool, int) {
	slot := time.Now().Unix() / int64(window.Seconds())
	key := fmt.Sprintf("rl:%s:%s:%d", scope, id, slot)
	pipe := s.rdb.Pipeline()
	incr := pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, window+time.Second)
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return true, 0
	}
	retry := int(window.Seconds()) - int(time.Now().Unix()%int64(window.Seconds()))
	return incr.Val() <= int64(n), retry
}

// rateLimit writes 429 and returns false when over the limit.
func (s *Server) rateLimit(w http.ResponseWriter, r *http.Request, scope, id string, n int, window time.Duration) bool {
	ok, retry := s.limit(r.Context(), scope, id, n, window)
	if !ok {
		fail(w, r, "RATE_LIMITED", "too many requests", map[string]any{"retry_after": retry})
	}
	return ok
}

func (s *Server) ipLimit(w http.ResponseWriter, r *http.Request, scope string, perMin int) bool {
	return s.rateLimit(w, r, "ip:"+scope, clientIP(r, s.cfg.TrustProxy), perMin, time.Minute)
}

func requireIdemKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	k := r.Header.Get("Idempotency-Key")
	if k == "" {
		fail(w, r, "BAD_REQUEST", "Idempotency-Key header required", nil)
		return "", false
	}
	if !isUUID(k) {
		fail(w, r, "BAD_REQUEST", "Idempotency-Key must be a UUID", nil)
		return "", false
	}
	return strings.ToLower(k), true
}
