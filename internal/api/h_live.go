package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"railway/internal/live"
	"railway/internal/platform/metrics"
	"railway/internal/wire"
)

// GET /v1/journeys/{id}/seatmap/live — real-time seat deltas over SSE (§11.7).
// Any logged-in user may watch, admitted or still in the waiting room: watching
// costs one buffered channel per connection, and seeing seats go in real time
// keeps waiting users from picking seats that are already gone. Locking seats
// still needs admission. The client loads the snapshot, then connects with
// Last-Event-ID: <version>. Missed deltas are replayed from smlog:{journey}; a
// version older than the stream gets event: resync. Connections close after
// LIVE_CONN_MAX_AGE so load is re-balanced; a newer stream of the same user on
// the same journey closes this one (reason "replaced").
func (s *Server) liveStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := claims(r).UserID()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, r, "NOT_FOUND", "unknown journey", nil)
		return
	}
	if _, err := s.cat.Journey(ctx, id); err != nil {
		fail(w, r, "NOT_FOUND", "unknown journey", nil)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, r, "BAD_REQUEST", "streaming unsupported", nil)
		return
	}
	from := int64(-1)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		from, _ = strconv.ParseInt(v, 10, 64)
	} else if v := r.URL.Query().Get("last_event_id"); v != "" {
		from, _ = strconv.ParseInt(v, 10, 64)
	}

	sub, err := s.hub.Attach(ctx, id) // before reading smlog, so nothing falls in between
	if errors.Is(err, live.ErrFull) {
		fail(w, r, "OVERLOADED", "live stream full; poll the seat map", map[string]any{"retry_after": 2})
		return
	}
	if err != nil {
		fail(w, r, "UNAVAILABLE", "live stream unavailable; poll the seat map", map[string]any{"retry_after": 2})
		return
	}
	defer s.hub.Detach(sub)
	connID := newRequestID()
	s.hub.ClaimConnection(ctx, uid, id, connID, s.cfg.LiveConnMaxAge+time.Minute)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	snapshot := fmt.Sprintf("/v1/journeys/%d/seatmap", id)
	resync := func(reason string) {
		fmt.Fprintf(w, "event: resync\ndata: {\"reason\":%q,\"snapshot_url\":%q}\n\n", reason, snapshot)
		flusher.Flush()
	}
	held := from
	write := func(d wire.SeatDelta) bool {
		if d.V <= held {
			return true // already applied
		}
		if d.FromV > held {
			resync("gap")
			return false
		}
		b, _ := json.Marshal(d)
		if _, err := fmt.Fprintf(w, "event: delta\nid: %d\ndata: %s\n\n", d.V, b); err != nil {
			return false
		}
		held = d.V
		return true
	}

	if from < 0 {
		resync("no_version")
		return
	}
	missed, gap, err := s.hub.Missed(ctx, id, from)
	switch {
	case err != nil:
		resync("smlog_unavailable")
		return
	case gap:
		resync("gap")
		return
	}
	for _, d := range missed {
		if !write(d) {
			return
		}
	}
	flusher.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	maxAge := time.NewTimer(s.cfg.LiveConnMaxAge)
	defer maxAge.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Done():
			if sub.Reconnect() || sub.Slow() {
				fmt.Fprint(w, "event: reconnect\ndata: {}\n\n")
				flusher.Flush()
			}
			return
		case <-maxAge.C:
			fmt.Fprint(w, "event: reconnect\ndata: {\"reason\":\"max_age\"}\n\n")
			flusher.Flush()
			return
		case <-ping.C:
			if !s.hub.Owns(ctx, uid, id, connID) {
				fmt.Fprint(w, "event: reconnect\ndata: {\"reason\":\"replaced\"}\n\n")
				flusher.Flush()
				return
			}
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case d := <-sub.C():
			metrics.LiveDeltaAge.Observe(time.Since(d.At).Seconds())
			if !write(d) {
				return
			}
			flusher.Flush()
		}
	}
}
