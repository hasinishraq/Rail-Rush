package api

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	bookingv1 "railway/gen/booking/v1"
	"railway/internal/api/dto"
	"railway/internal/auth"
	"railway/internal/catalog"
	"railway/internal/fares"
	"railway/internal/wire"
)

var errNoBooking = errors.New("no booking")

// bookingView reads the projection (read replica). If the PNR is not projected
// yet but hold:{pnr} exists, it is built from the hold; the allocator's
// summary is fresher than the projection, so its status wins when newer.
// PENDING_OTP is derived: PENDING_PAYMENT with no verified OTP (§17.1).
func (s *Server) bookingView(ctx context.Context, pnr string) (*dto.BookingResponse, int64, error) {
	hold, _, _ := s.store.Hold(ctx, pnr)
	out := &dto.BookingResponse{PNR: pnr, Passengers: []dto.PassengerSeat{}}
	var owner int64
	var from, to int
	var amount int64
	var currency string
	err := s.readDB.QueryRow(ctx, `SELECT status, journey_id, user_id, from_idx, to_idx, class_code, amount_minor, currency,
	        hold_expires_at, updated_at FROM booking WHERE pnr = $1 LIMIT 1`, pnr).
		Scan(&out.Status, &out.JourneyID, &owner, &from, &to, &out.ClassCode, &amount, &currency, &out.HoldExpiresAt, &out.UpdatedAt)
	projected := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		if hold == nil {
			return nil, 0, errNoBooking
		}
		out.Status, out.JourneyID, owner, from, to = hold.Status, hold.JourneyID, hold.UserID, hold.FromIdx, hold.ToIdx
		out.ClassCode, amount, currency, out.UpdatedAt = hold.ClassCode, hold.AmountMinor, hold.Currency, hold.UpdatedAt
		exp := hold.ExpiresAt
		out.HoldExpiresAt = &exp
	} else if err != nil {
		return nil, 0, err
	}
	out.Amount = dto.Money{AmountMinor: amount, Currency: currency}

	j, err := s.cat.Journey(ctx, out.JourneyID)
	if err != nil {
		return nil, 0, err
	}
	layout, _ := s.cat.Layout(j.LayoutID)
	var adult int64
	if t, err := s.cat.Train(j.TrainNo); err == nil {
		out.TrainNo, out.TrainName = t.TrainNo, t.Name
		out.From, out.To = s.search.StopRef(j, t, from, false), s.search.StopRef(j, t, to, true)
		adult = s.cat.Fare(j, t, out.ClassCode, from, to).AmountMinor
	}
	out.RunDate = j.RunDateString()

	if hold != nil && (!projected || hold.UpdatedAt.After(out.UpdatedAt)) {
		out.Status, out.UpdatedAt, out.SeatsChanged = hold.Status, hold.UpdatedAt, hold.SeatsChanged
		if out.Status == "PENDING_PAYMENT" {
			exp := hold.ExpiresAt
			out.HoldExpiresAt = &exp
		}
		out.Passengers = s.paxFromHold(layout, hold, adult)
	} else {
		rows, err := s.readDB.Query(ctx, `SELECT seq, passenger_name, gender, passenger_type, status, seat_idx, seat_id, fare_minor
		        FROM ticket WHERE pnr = $1 ORDER BY seq`, pnr)
		if err != nil {
			return nil, 0, err
		}
		for rows.Next() {
			var p dto.PassengerSeat
			var idx int32
			var seatID string
			if err := rows.Scan(&p.Seq, &p.Name, &p.Gender, &p.Type, &p.Status, &idx, &seatID, &p.Fare.AmountMinor); err != nil {
				rows.Close()
				return nil, 0, err
			}
			p.Seat, p.Fare.Currency = s.seatRef(layout, idx, seatID), currency
			out.Passengers = append(out.Passengers, p)
		}
		rows.Close()
	}
	if out.Status != "PENDING_PAYMENT" {
		out.HoldExpiresAt = nil
	} else if verified, _, found, err := s.otp.Status(ctx, s.readDB, pnr); err == nil && (!found || !verified) {
		out.Status = "PENDING_OTP"
	}

	var pb dto.PaymentBrief
	err = s.readDB.QueryRow(ctx, `SELECT payment_id::text, status, method, amount_minor, currency FROM payment WHERE pnr = $1
	        ORDER BY (captured_at IS NOT NULL) DESC, created_at DESC LIMIT 1`, pnr).
		Scan(&pb.PaymentID, &pb.Status, &pb.Method, &pb.Amount.AmountMinor, &pb.Amount.Currency)
	if err == nil {
		out.Payment = &pb
	}
	rows, err := s.readDB.Query(ctx, `SELECT refund_id::text, reason, amount_minor, status FROM refund WHERE pnr = $1 ORDER BY created_at`, pnr)
	if err == nil {
		for rows.Next() {
			var rb dto.RefundBrief
			if rows.Scan(&rb.RefundID, &rb.Reason, &rb.Amount.AmountMinor, &rb.Status) == nil {
				rb.Amount.Currency = fares.DefaultCurrency
				out.Refunds = append(out.Refunds, rb)
			}
		}
		rows.Close()
	}
	return out, owner, nil
}

func (s *Server) paxFromHold(layout *catalog.Layout, h *wire.HoldSummary, adult int64) []dto.PassengerSeat {
	out := []dto.PassengerSeat{}
	for _, p := range h.Passengers {
		out = append(out, dto.PassengerSeat{Seq: p.Seq, Name: p.Name, Gender: p.Gender, Type: p.Type, Status: p.Status,
			Seat: s.seatRef(layout, p.SeatIdx, p.SeatID), Fare: passengerFare(adult, p.Type)})
	}
	return out
}

// GET /v1/bookings/{pnr} — owner, or counter / admin.
func (s *Server) getBooking(w http.ResponseWriter, r *http.Request) {
	c := claims(r)
	b, owner, err := s.bookingView(r.Context(), r.PathValue("pnr"))
	if errors.Is(err, errNoBooking) || (err == nil && owner != c.UserID() && !isStaff(c.Role)) {
		fail(w, r, "NOT_FOUND", "unknown pnr", nil)
		return
	}
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

var cancellable = map[string]bool{"CONFIRMED": true, "PARTIALLY_CANCELLED": true}

// POST /v1/bookings/{pnr}/cancel — owner or counter; Idempotency-Key (§17.3, W13).
func (s *Server) cancelBooking(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c := claims(r)
	uid := c.UserID()
	pnr := r.PathValue("pnr")
	key, ok := requireIdemKey(w, r)
	if !ok {
		return
	}
	var req dto.CancelRequest
	body, ok := decode(w, r, &req)
	if !ok {
		return
	}
	if !s.sameBody(w, r, uid, "POST /v1/bookings/"+pnr+"/cancel", key, body) {
		return
	}
	b, owner, err := s.bookingView(ctx, pnr)
	if errors.Is(err, errNoBooking) || (err == nil && owner != uid && c.Role != "counter") {
		fail(w, r, "NOT_FOUND", "unknown pnr", nil)
		return
	}
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	if !cancellable[b.Status] {
		fail(w, r, "NOT_CANCELLABLE", "booking cannot be cancelled", map[string]any{"reason": "status " + b.Status})
		return
	}
	if !s.clock.Now().Before(b.From.Time) {
		fail(w, r, "NOT_CANCELLABLE", "train has departed", map[string]any{"reason": "departed"})
		return
	}
	seen := map[int]bool{}
	for _, q := range req.PassengerSeqs {
		if q < 1 || q > len(b.Passengers) || seen[q] {
			fail(w, r, "VALIDATION_FAILED", "invalid passenger_seqs", map[string]any{"fields": map[string]string{"passenger_seqs": fmt.Sprintf("bad seq %d", q)}})
			return
		}
		seen[q] = true
	}
	cancel := &bookingv1.Cancel{Pnr: pnr, UserId: uid, CancelId: key, Channel: channelFor(c.Role)}
	for _, q := range req.PassengerSeqs {
		cancel.PassengerSeqs = append(cancel.PassengerSeqs, int32(q))
	}
	cmd := &bookingv1.Command{RequestId: newRequestID(), IdemKey: fmt.Sprintf("%d:%s", uid, key), JourneyId: b.JourneyID,
		ReplyTo: wire.ResultsChannel(s.cfg.PodName), Body: &bookingv1.Command_Cancel{Cancel: cancel}}
	s.dispatch(w, r, cmd, uid, "cancel", nil)
}

func (s *Server) cancelResponse(ctx context.Context, res wire.Result, c *bookingv1.Cancelled) dto.CancelResponse {
	out := dto.CancelResponse{RequestID: res.RequestID, PNR: c.Pnr, CancelID: c.CancelId, RefundStatus: "PENDING",
		BookingStatus: "CANCELLED", Passengers: []dto.PassengerSeat{}, CancelledSeqs: []int{}}
	for _, q := range c.PassengerSeqs {
		out.CancelledSeqs = append(out.CancelledSeqs, int(q))
	}
	sum := res.Hold
	if sum == nil {
		return out
	}
	out.BookingStatus = sum.Status
	var layout *catalog.Layout
	departs := s.clock.Now()
	var adult int64
	if j, err := s.cat.Journey(ctx, sum.JourneyID); err == nil {
		layout, _ = s.cat.Layout(j.LayoutID)
		departs = j.DepartsAt
		if t, err := s.cat.Train(j.TrainNo); err == nil {
			departs = s.cat.StopTime(j, t, sum.FromIdx, false)
			adult = s.cat.Fare(j, t, sum.ClassCode, sum.FromIdx, sum.ToIdx).AmountMinor
		}
	}
	out.Passengers = s.paxFromHold(layout, sum, adult)
	amt := fares.RefundAmount(sum.AmountMinor, len(sum.Passengers), len(c.PassengerSeqs), sum.ClassCode, departs, s.clock.Now(), fares.ReasonCancelled)
	out.RefundEstimate = dto.Money{AmountMinor: amt, Currency: sum.Currency}
	return out
}

// GET /v1/bookings/{pnr}/ticket — application/pdf for confirmed bookings.
func (s *Server) ticket(w http.ResponseWriter, r *http.Request) {
	c := claims(r)
	b, owner, err := s.bookingView(r.Context(), r.PathValue("pnr"))
	if err != nil || (owner != c.UserID() && !isStaff(c.Role)) {
		fail(w, r, "NOT_FOUND", "unknown pnr", nil)
		return
	}
	if b.Status != "CONFIRMED" && b.Status != "PARTIALLY_CANCELLED" {
		fail(w, r, "NOT_FOUND", "no ticket for an unpaid or failed booking", nil)
		return
	}
	if s.tickets == nil {
		fail(w, r, "UNAVAILABLE", "ticket signing is not configured", nil)
		return
	}
	qr, err := qrPayload(s.tickets, b)
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	pdf, err := renderTicket(b, qr, time.Now().UTC())
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="ticket-%s.pdf"`, b.PNR))
	w.Header().Set("Cache-Control", "private, max-age=60")
	_, _ = w.Write(pdf)
}

// GET /v1/tickets/keys — public keys that verify ticket QR codes (JWKS), for
// gate scanners that check tickets offline.
func (s *Server) ticketKeys(w http.ResponseWriter, r *http.Request) {
	if s.tickets == nil {
		fail(w, r, "UNAVAILABLE", "ticket signing is not configured", nil)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeRaw(w, http.StatusOK, auth.JWKSFor(map[string]*ecdsa.PublicKey{s.tickets.Kid(): s.tickets.Public()}))
}

// POST /v1/tickets/verify — staff only. Checks the QR signature, then the
// booking's current state: a ticket cancelled after printing is no longer valid.
func (s *Server) verifyTicket(w http.ResponseWriter, r *http.Request) {
	if !isStaff(claims(r).Role) {
		fail(w, r, "FORBIDDEN", "gate staff only", nil)
		return
	}
	if s.tickets == nil {
		fail(w, r, "UNAVAILABLE", "ticket signing is not configured", nil)
		return
	}
	var req dto.VerifyTicketRequest
	if _, ok := decode(w, r, &req); !ok {
		return
	}
	p, err := s.tickets.Verifier().Verify(req.QR)
	if err != nil {
		fail(w, r, "TICKET_INVALID", "ticket QR code is not genuine", map[string]any{"reason": err.Error()})
		return
	}
	b, _, err := s.bookingView(r.Context(), p.PNR)
	if errors.Is(err, errNoBooking) {
		fail(w, r, "TICKET_INVALID", "ticket QR code is not genuine", map[string]any{"reason": "unknown pnr"})
		return
	}
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	if b.TrainNo != p.TrainNo || b.RunDate != p.RunDate || b.From.StationCode != p.From || b.To.StationCode != p.To {
		fail(w, r, "TICKET_INVALID", "ticket QR code is not genuine", map[string]any{"reason": "trip mismatch"})
		return
	}
	bySeat := make(map[string]dto.PassengerSeat, len(b.Passengers))
	for _, ps := range b.Passengers {
		bySeat[ps.Seat.SeatID] = ps
	}
	out := dto.VerifyTicketResponse{PNR: b.PNR, BookingStatus: b.Status, TrainNo: b.TrainNo, TrainName: b.TrainName,
		RunDate: b.RunDate, From: b.From, To: b.To, ClassCode: b.ClassCode, Passengers: []dto.PassengerSeat{}}
	for _, seat := range p.Seats {
		if ps, ok := bySeat[seat]; ok {
			out.Passengers = append(out.Passengers, ps)
			if ps.Status == "ISSUED" {
				out.Valid = true
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}
