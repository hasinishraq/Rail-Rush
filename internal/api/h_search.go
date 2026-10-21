package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"railway/internal/search"
)

// serveCached writes a cacheable JSON body with an ETag and honours If-None-Match.
func serveCached(w http.ResponseWriter, r *http.Request, cacheControl string, body []byte) {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeRaw(w, http.StatusOK, body)
}

// GET /v1/stations?q=&limit=
func (s *Server) stations(w http.ResponseWriter, r *http.Request) {
	if !s.ipLimit(w, r, "stations", 120) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, http.StatusOK, map[string]any{"items": s.search.Stations(r.URL.Query().Get("q"), limit)})
}

// GET /v1/search?from=&to=&date=&class=
func (s *Server) searchTrains(w http.ResponseWriter, r *http.Request) {
	if !s.ipLimit(w, r, "search", 60) {
		return
	}
	q := r.URL.Query()
	from, to := strings.ToUpper(q.Get("from")), strings.ToUpper(q.Get("to"))
	fields := map[string]string{}
	if from == "" {
		fields["from"] = "required"
	}
	if to == "" {
		fields["to"] = "required"
	}
	date, err := time.Parse("2006-01-02", q.Get("date"))
	if err != nil {
		fields["date"] = "YYYY-MM-DD required"
	}
	if from != "" && from == to {
		fields["to"] = "must differ from from"
	}
	if len(fields) > 0 {
		fail(w, r, "VALIDATION_FAILED", "invalid query", map[string]any{"fields": fields})
		return
	}
	resp, err := s.search.Search(r.Context(), from, to, date, strings.ToUpper(q.Get("class")))
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=15")
	writeJSON(w, http.StatusOK, resp)
}

// GET /v1/journeys/{id}
func (s *Server) journey(w http.ResponseWriter, r *http.Request) {
	if !s.ipLimit(w, r, "journey", 120) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, r, "NOT_FOUND", "unknown journey", nil)
		return
	}
	resp, err := s.search.Journey(r.Context(), id)
	if err == search.ErrNotFound {
		fail(w, r, "NOT_FOUND", "unknown journey", nil)
		return
	}
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	b, _ := json.Marshal(resp)
	serveCached(w, r, "public, max-age=60", b)
}

// GET /v1/layouts/{layoutId}
func (s *Server) layout(w http.ResponseWriter, r *http.Request) {
	if !s.ipLimit(w, r, "layout", 120) {
		return
	}
	id, err := strconv.Atoi(r.PathValue("layoutId"))
	if err != nil {
		fail(w, r, "NOT_FOUND", "unknown layout", nil)
		return
	}
	resp, err := s.search.Layout(id)
	if err != nil {
		fail(w, r, "NOT_FOUND", "unknown layout", nil)
		return
	}
	b, _ := json.Marshal(resp)
	serveCached(w, r, "public, max-age=86400, immutable", b)
}

// GET /v1/journeys/{id}/seatmap — the seat layout for millions (§11.6). Any
// logged-in user, no admission; one Redis read; the response has no personal
// data, so the CDN caches it for 1 s whatever the Authorization header.
// 409 NOT_OPEN (cacheable too) until a pool opens.
func (s *Server) seatMap(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, r, "NOT_FOUND", "unknown journey", nil)
		return
	}
	j, err := s.cat.Journey(r.Context(), id)
	if err != nil {
		fail(w, r, "NOT_FOUND", "unknown journey", nil)
		return
	}
	raw, sm, err := s.search.SeatMap(r.Context(), id)
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	if sm == nil || !sm.AnyOpen() {
		var opens time.Time
		for _, p := range j.Pools {
			if opens.IsZero() || p.OpensAt.Before(opens) {
				opens = p.OpensAt
			}
		}
		w.Header().Set("Cache-Control", "public, max-age=1")
		fail(w, r, "NOT_OPEN", "booking window not open yet", map[string]any{"opens_at": opens})
		return
	}
	serveCached(w, r, "public, max-age=1, stale-while-revalidate=1", raw)
}
