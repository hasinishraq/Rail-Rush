package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"railway/internal/wire"
)

type outcome struct {
	status int
	body   any
}

func errOutcome(r *http.Request, code, msg string, details map[string]any) outcome {
	st, ok := codeStatus[code]
	if !ok {
		st = 500
	}
	return outcome{st, errorBody(r, code, msg, details)}
}

// idempotent runs fn once per (user, endpoint, key) using the idempotency_key
// table; a retry gets the stored response, the same key with a different body
// gets 422 IDEMPOTENCY_KEY_REUSED. 5xx outcomes are not stored, so they can be retried.
func (s *Server) idempotent(w http.ResponseWriter, r *http.Request, userID int64, endpoint, key string, body []byte, fn func() outcome) {
	ctx := r.Context()
	hash := hashBody(body)
	tag, err := s.db.Exec(ctx, `INSERT INTO idempotency_key (user_id, endpoint, key, request_hash) VALUES ($1, $2, $3, $4)
	                             ON CONFLICT DO NOTHING`, userID, endpoint, key, hash)
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	if tag.RowsAffected() == 0 {
		var stored []byte
		var status *int16
		var resp []byte
		var created time.Time
		err := s.db.QueryRow(ctx, `SELECT request_hash, status_code, response, created_at FROM idempotency_key
		                            WHERE user_id = $1 AND endpoint = $2 AND key = $3`, userID, endpoint, key).Scan(&stored, &status, &resp, &created)
		if errors.Is(err, pgx.ErrNoRows) { // removed concurrently: let the client retry
			fail(w, r, "REQUEST_IN_PROGRESS", "retry the request", map[string]any{"retry_after": 1})
			return
		}
		if err != nil {
			internalErr(w, r, s, err)
			return
		}
		if !bytes.Equal(stored, hash) {
			fail(w, r, "IDEMPOTENCY_KEY_REUSED", "Idempotency-Key reused with a different body", nil)
			return
		}
		if status == nil {
			if time.Since(created) > time.Minute { // the first attempt died mid-way
				_, _ = s.db.Exec(ctx, `DELETE FROM idempotency_key WHERE user_id = $1 AND endpoint = $2 AND key = $3 AND status_code IS NULL`, userID, endpoint, key)
			}
			fail(w, r, "REQUEST_IN_PROGRESS", "the original request is still being processed", map[string]any{"retry_after": 1})
			return
		}
		writeRaw(w, int(*status), resp)
		return
	}

	out := fn()
	b, _ := json.Marshal(out.body)
	if out.status >= 500 {
		_, _ = s.db.Exec(ctx, `DELETE FROM idempotency_key WHERE user_id = $1 AND endpoint = $2 AND key = $3`, userID, endpoint, key)
	} else {
		_, _ = s.db.Exec(ctx, `UPDATE idempotency_key SET status_code = $4, response = $5 WHERE user_id = $1 AND endpoint = $2 AND key = $3`,
			userID, endpoint, key, out.status, b)
	}
	writeRaw(w, out.status, append(b, '\n'))
}

// sameBody enforces "same key, same body" for allocator-deduplicated endpoints
// (holds, cancels): the allocator returns the original answer, the API only
// guards against a reused key with a different request.
func (s *Server) sameBody(w http.ResponseWriter, r *http.Request, userID int64, endpoint, key string, body []byte) bool {
	h := hexHash(body)
	k := wire.IdemKey(userID, endpoint, key)
	ok, err := s.rdb.SetNX(r.Context(), k, h, 24*time.Hour).Result()
	if err != nil {
		return true // Redis down: the allocator still deduplicates
	}
	if ok {
		return true
	}
	prev, err := s.rdb.Get(r.Context(), k).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return true
	}
	if prev != "" && prev != h {
		fail(w, r, "IDEMPOTENCY_KEY_REUSED", "Idempotency-Key reused with a different body", nil)
		return false
	}
	return true
}
