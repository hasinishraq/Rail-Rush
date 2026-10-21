package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"railway/internal/scheduler"
)

// POST /internal/v1/journeys/{id}/{warm|open|close} — the same command
// producers the scheduler uses; for operators and local testing. 202.
func (s *Server) journeyCommand(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, r, "NOT_FOUND", "unknown journey", nil)
		return
	}
	if _, err := s.cat.Journey(r.Context(), id); err != nil {
		fail(w, r, "NOT_FOUND", "unknown journey", nil)
		return
	}
	kind := strings.ToUpper(r.PathValue("action"))
	if kind != "WARM" && kind != "OPEN" && kind != "CLOSE" {
		fail(w, r, "NOT_FOUND", "action must be warm, open or close", nil)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if len(body) > 0 && !json.Valid(body) {
		fail(w, r, "BAD_REQUEST", "malformed JSON", nil)
		return
	}
	cmd, err := scheduler.Command(id, kind, body)
	if err != nil {
		fail(w, r, "BAD_REQUEST", err.Error(), nil)
		return
	}
	if err := s.producer.Produce(r.Context(), cmd); err != nil {
		fail(w, r, "LOG_UNAVAILABLE", "kafka unavailable", map[string]any{"retry_after": 2})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"request_id": cmd.RequestId, "command": kind})
}

// GET /internal/v1/journeys/{id}/invariants — §24 checks for one journey.
func (s *Server) invariants(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || s.invariant == nil {
		fail(w, r, "NOT_FOUND", "unknown journey", nil)
		return
	}
	res, err := s.invariant(r.Context(), id)
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
