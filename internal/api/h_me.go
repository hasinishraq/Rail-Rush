package api

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"railway/internal/api/dto"
)

var (
	genders  = map[string]bool{"MALE": true, "FEMALE": true, "OTHER": true}
	paxTypes = map[string]bool{"": true, "ADULT": true, "CHILD": true}
)

func validatePassenger(p dto.PassengerIn, prefix string, fields map[string]string) {
	if n := len([]rune(strings.TrimSpace(p.Name))); n < 1 || n > 40 {
		fields[prefix+"name"] = "1..40 characters"
	}
	if !genders[p.Gender] {
		fields[prefix+"gender"] = "MALE | FEMALE | OTHER"
	}
	if !paxTypes[p.Type] {
		fields[prefix+"type"] = "ADULT | CHILD"
	}
}

// GET /v1/me/passengers — saved passengers (max 20), so the opening minute
// carries only seat clicks, passenger details and payment (§11.1).
func (s *Server) listPassengers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.readDB.Query(r.Context(), `SELECT id, name, gender, passenger_type FROM saved_passenger
	                                          WHERE user_id = $1 ORDER BY id LIMIT 20`, claims(r).UserID())
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	defer rows.Close()
	out := dto.Page[dto.SavedPassenger]{Items: []dto.SavedPassenger{}}
	for rows.Next() {
		var p dto.SavedPassenger
		if err := rows.Scan(&p.ID, &p.Name, &p.Gender, &p.Type); err != nil {
			internalErr(w, r, s, err)
			return
		}
		out.Items = append(out.Items, p)
	}
	writeJSON(w, http.StatusOK, out)
}

// POST /v1/me/passengers (Idempotency-Key)
func (s *Server) addPassenger(w http.ResponseWriter, r *http.Request) {
	key, ok := requireIdemKey(w, r)
	if !ok {
		return
	}
	var p dto.PassengerIn
	body, ok := decode(w, r, &p)
	if !ok {
		return
	}
	p.Gender, p.Type = strings.ToUpper(p.Gender), strings.ToUpper(p.Type)
	fields := map[string]string{}
	validatePassenger(p, "", fields)
	if len(fields) > 0 {
		fail(w, r, "VALIDATION_FAILED", "invalid passenger", map[string]any{"fields": fields})
		return
	}
	if p.Type == "" {
		p.Type = "ADULT"
	}
	uid := claims(r).UserID()
	s.idempotent(w, r, uid, "POST /v1/me/passengers", key, body, func() outcome {
		var n int
		_ = s.db.QueryRow(r.Context(), `SELECT count(*) FROM saved_passenger WHERE user_id = $1`, uid).Scan(&n)
		if n >= 20 {
			return errOutcome(r, "VALIDATION_FAILED", "at most 20 saved passengers", map[string]any{"fields": map[string]string{"passengers": "max 20"}})
		}
		p.Name = strings.TrimSpace(p.Name)
		out := dto.SavedPassenger{PassengerIn: p}
		if err := s.db.QueryRow(r.Context(), `INSERT INTO saved_passenger (user_id, name, gender, passenger_type) VALUES ($1,$2,$3,$4) RETURNING id`,
			uid, p.Name, p.Gender, p.Type).Scan(&out.ID); err != nil {
			s.log.Error("add passenger", "err", err)
			return errOutcome(r, "INTERNAL", "internal error", nil)
		}
		return outcome{http.StatusCreated, out}
	})
}

// DELETE /v1/me/passengers/{id}
func (s *Server) deletePassenger(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, r, "NOT_FOUND", "unknown passenger", nil)
		return
	}
	tag, err := s.db.Exec(r.Context(), `DELETE FROM saved_passenger WHERE id = $1 AND user_id = $2`, id, claims(r).UserID())
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	if tag.RowsAffected() == 0 {
		fail(w, r, "NOT_FOUND", "unknown passenger", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /v1/me/bookings?status=upcoming|past&limit=&cursor= — newest first (§17.2).
func (s *Server) myBookings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	upcoming := q.Get("status") != "past"
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	curTime, curPNR := time.Now().Add(time.Hour), "~"
	if c := q.Get("cursor"); c != "" {
		b, err := base64.RawURLEncoding.DecodeString(c)
		parts := strings.SplitN(string(b), "|", 2)
		if err != nil || len(parts) != 2 {
			fail(w, r, "BAD_REQUEST", "bad cursor", nil)
			return
		}
		ns, _ := strconv.ParseInt(parts[0], 10, 64)
		curTime, curPNR = time.Unix(0, ns), parts[1]
	}
	cmp := ">="
	if !upcoming {
		cmp = "<"
	}
	rows, err := s.readDB.Query(r.Context(), `
		SELECT b.pnr, b.status, b.journey_id, b.from_idx, b.to_idx, b.created_at,
		       COALESCE((SELECT array_agg(t.seat_idx ORDER BY t.seq) FROM ticket t WHERE t.run_date = b.run_date AND t.pnr = b.pnr), '{}'),
		       COALESCE((SELECT array_agg(t.seat_id ORDER BY t.seq) FROM ticket t WHERE t.run_date = b.run_date AND t.pnr = b.pnr), '{}')
		  FROM booking b
		 WHERE b.user_id = $1 AND b.run_date `+cmp+` (now() AT TIME ZONE 'Asia/Dhaka')::date AND (b.created_at, b.pnr) < ($2, $3)
		 ORDER BY b.created_at DESC, b.pnr DESC LIMIT $4`, claims(r).UserID(), curTime, curPNR, limit+1)
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	defer rows.Close()
	out := dto.Page[dto.BookingSummary]{Items: []dto.BookingSummary{}}
	var lastT time.Time
	var lastP string
	for rows.Next() {
		bs := dto.BookingSummary{Seats: []dto.SeatRef{}}
		var jid int64
		var from, to int
		var created time.Time
		var idx []int32
		var ids []string
		if err := rows.Scan(&bs.PNR, &bs.Status, &jid, &from, &to, &created, &idx, &ids); err != nil {
			internalErr(w, r, s, err)
			return
		}
		if len(out.Items) == limit {
			out.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(lastT.UnixNano(), 10) + "|" + lastP))
			break
		}
		if j, err := s.cat.Journey(r.Context(), jid); err == nil {
			layout, _ := s.cat.Layout(j.LayoutID)
			for i := range idx {
				if i < len(ids) {
					bs.Seats = append(bs.Seats, s.seatRef(layout, idx[i], ids[i]))
				}
			}
			if t, err := s.cat.Train(j.TrainNo); err == nil {
				bs.TrainNo, bs.RunDate = t.TrainNo, j.RunDateString()
				bs.From, bs.To = s.search.StopRef(j, t, from, false), s.search.StopRef(j, t, to, true)
			}
		}
		out.Items = append(out.Items, bs)
		lastT, lastP = created, bs.PNR
	}
	writeJSON(w, http.StatusOK, out)
}
