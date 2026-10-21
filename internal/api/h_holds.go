package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"

	bookingv1 "railway/gen/booking/v1"
	"railway/internal/admission"
	"railway/internal/api/dto"
	"railway/internal/catalog"
	"railway/internal/fares"
	"railway/internal/inventory"
	"railway/internal/otp"
	"railway/internal/platform/metrics"
	"railway/internal/platform/pg"
	"railway/internal/wire"
)

var pju = protojson.UnmarshalOptions{DiscardUnknown: true}

var (
	genderPB = map[string]bookingv1.Gender{"MALE": bookingv1.Gender_MALE, "FEMALE": bookingv1.Gender_FEMALE, "OTHER": bookingv1.Gender_OTHER}
	typePB   = map[string]bookingv1.PassengerType{"": bookingv1.PassengerType_ADULT, "ADULT": bookingv1.PassengerType_ADULT,
		"CHILD": bookingv1.PassengerType_CHILD}
)

// POST /v1/holds — passenger details turn the locked seats into a 10-minute
// hold with a PNR and an OTP (§14.1, W7). Admitted, Idempotency-Key.
func (s *Server) createHold(w http.ResponseWriter, r *http.Request, adm *admission.Claims) {
	ctx := r.Context()
	c := claims(r)
	uid := c.UserID()
	key, ok := requireIdemKey(w, r)
	if !ok {
		return
	}
	if !s.rateLimit(w, r, "user:holds", strconv.FormatInt(uid, 10), 5, time.Minute) {
		return
	}
	var req dto.CreateHoldRequest
	body, ok := decode(w, r, &req)
	if !ok {
		return
	}
	retry, _ := s.rdb.Exists(ctx, wire.IdemKey(uid, "POST /v1/holds", key)).Result()
	if !s.sameBody(w, r, uid, "POST /v1/holds", key, body) {
		return
	}
	// One admission, one booking; a retry of the hold that used it still gets
	// its original answer from the allocator.
	if err := s.admit.CheckUnused(ctx, adm); err != nil && retry == 0 {
		var used *admission.UsedError
		if errors.As(err, &used) {
			fail(w, r, "ADMISSION_USED", "this place in the queue already produced a booking", map[string]any{"pnr": used.PNR})
			return
		}
		internalErr(w, r, s, err)
		return
	}
	j, err := s.cat.Journey(ctx, req.JourneyID)
	if err != nil {
		fail(w, r, "NOT_FOUND", "unknown journey", nil)
		return
	}
	fields := map[string]string{}
	if n := len(req.Passengers); n < 1 || n > 4 {
		fields["passengers"] = "1 to 4 passengers"
	}
	for i := range req.Passengers {
		req.Passengers[i].Gender, req.Passengers[i].Type = strings.ToUpper(req.Passengers[i].Gender), strings.ToUpper(req.Passengers[i].Type)
		validatePassenger(req.Passengers[i], fmt.Sprintf("passengers[%d].", i), fields)
	}
	if len(fields) > 0 {
		fail(w, r, "VALIDATION_FAILED", "invalid passenger details", map[string]any{"fields": fields})
		return
	}
	if salesClosed(j, s.clock.Now()) {
		fail(w, r, "SALES_CLOSED", "online sales for this train have closed", nil)
		return
	}
	if cart, ok, _ := s.store.Cart(ctx, j.JourneyID, uid); ok {
		if len(cart.Seats) != len(req.Passengers) {
			fail(w, r, "PASSENGER_COUNT_MISMATCH", "one passenger per selected seat", map[string]any{"seats": len(cart.Seats)})
			return
		}
		if c.Role != "counter" {
			issued, _ := s.store.ULimit(ctx, uid, j.RunDateString())
			if issued+len(cart.Seats) > s.cfg.DailySeatLimit {
				fail(w, r, "ORDER_LIMIT_EXCEEDED", "this would exceed the seats allowed for this travel day",
					map[string]any{"limit": s.cfg.DailySeatLimit, "issued": issued})
				return
			}
		}
	}

	pnr, err := s.pnrs.Next(ctx)
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	hold := &bookingv1.Hold{Pnr: pnr, UserId: uid, Channel: channelFor(c.Role)}
	for _, p := range req.Passengers {
		hold.Passengers = append(hold.Passengers, &bookingv1.Passenger{Name: strings.TrimSpace(p.Name), Gender: genderPB[p.Gender], Type: typePB[p.Type]})
	}
	cmd := &bookingv1.Command{RequestId: newRequestID(), IdemKey: fmt.Sprintf("%d:%s", uid, key), JourneyId: req.JourneyID,
		ReplyTo: wire.ResultsChannel(s.cfg.PodName), Body: &bookingv1.Command_Hold{Hold: hold}}
	s.dispatch(w, r, cmd, uid, "hold", func(status int, res wire.Result) {
		if status == http.StatusCreated && res.Hold != nil {
			_ = s.admit.MarkUsed(ctx, adm, res.Hold.PNR) // one admission, one PNR
		}
	})
}

// ensureOTP issues the OTP for a new hold (booking_otp + SMS outbox row, one
// transaction). It is idempotent: a retry, or a result read again through
// /v1/requests, finds the row and sends no second code.
func (s *Server) ensureOTP(ctx context.Context, pnr string, userID int64) (otp.Info, error) {
	var phone string
	if err := s.readDB.QueryRow(ctx, `SELECT phone FROM app_user WHERE user_id = $1`, userID).Scan(&phone); err != nil {
		return otp.Info{}, err
	}
	var info otp.Info
	err := pg.Tx(ctx, s.db, func(tx pgx.Tx) error {
		var err error
		info, err = s.otp.Issue(ctx, tx, pnr, userID, phone)
		return err
	})
	return info, err
}

// dispatch registers a waiter, produces the command and waits up to
// RESULT_WAIT for the allocator's answer; on timeout it answers 202 with
// poll and stream URLs.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, cmd *bookingv1.Command, uid int64, command string, after func(int, wire.Result)) {
	ctx := r.Context()
	ch := s.waiter.Register(cmd.RequestId) // before producing
	defer s.waiter.Forget(cmd.RequestId)
	if err := s.store.MarkPending(ctx, cmd.RequestId, uid); err != nil {
		s.log.Warn("mark pending failed", "err", err)
	}
	start := time.Now()
	if err := s.producer.Produce(ctx, cmd); err != nil {
		s.log.Warn("produce failed", "err", err, "request_id", cmd.RequestId)
		metrics.CommandResults.WithLabelValues(command, "LOG_UNAVAILABLE").Inc()
		fail(w, r, "LOG_UNAVAILABLE", "booking is briefly unavailable; nothing was changed", map[string]any{"retry_after": 2})
		return
	}
	timer := time.NewTimer(s.cfg.ResultWait)
	defer timer.Stop()
	select {
	case res := <-ch:
		metrics.DecisionSeconds.WithLabelValues(command).Observe(time.Since(start).Seconds())
		status, body := s.mapResult(ctx, r, res)
		metrics.CommandResults.WithLabelValues(command, resultLabel(res)).Inc()
		if after != nil {
			after(status, res)
		}
		writeJSON(w, status, body)
	case <-timer.C:
		metrics.CommandResults.WithLabelValues(command, "ACCEPTED").Inc()
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusAccepted, dto.AcceptedResponse{RequestID: cmd.RequestId,
			PollURL: "/v1/requests/" + cmd.RequestId, StreamURL: "/v1/requests/" + cmd.RequestId + "/stream"})
	case <-ctx.Done():
	}
}

// resultCode is the rejection code of a result, or "".
func resultCode(res wire.Result) string {
	var v struct {
		Code string `json:"code"`
	}
	switch res.Kind {
	case "SelectRejected", "HoldRejected", "CancelRejected":
		_ = json.Unmarshal(res.Event, &v)
	}
	return v.Code
}

func resultLabel(res wire.Result) string {
	if c := resultCode(res); c != "" {
		return c
	}
	return res.Kind
}

// mapResult turns an allocator result into the HTTP status and body the
// original call would have had (Appendix A, MapResult).
func (s *Server) mapResult(ctx context.Context, r *http.Request, res wire.Result) (int, any) {
	switch res.Kind {
	case "SeatSelected", "SeatReleased", wire.KindCart:
		if res.Cart == nil {
			return http.StatusOK, dto.CartResponse{JourneyID: res.JourneyID, Seats: []dto.SeatRef{}, SeatmapVersion: res.Version}
		}
		cr := s.cartResponse(res.Cart)
		cr.SeatmapVersion = res.Version
		return http.StatusOK, cr
	case "SeatHeld":
		var sh bookingv1.SeatHeld
		if err := pju.Unmarshal(res.Event, &sh); err != nil {
			return 500, errorBody(r, "INTERNAL", "bad result", nil)
		}
		info, err := s.ensureOTP(ctx, sh.Pnr, sh.UserId)
		if err != nil {
			s.log.Error("otp issue failed", "pnr", sh.Pnr, "err", err)
		}
		return http.StatusCreated, s.holdResponse(ctx, res.RequestID, res.JourneyID, &sh, info)
	case "Cancelled":
		var c bookingv1.Cancelled
		_ = pju.Unmarshal(res.Event, &c)
		return http.StatusOK, s.cancelResponse(ctx, res, &c)
	case "SelectRejected", "HoldRejected", "CancelRejected":
		return s.rejection(r, res)
	}
	return http.StatusOK, map[string]any{"request_id": res.RequestID, "kind": res.Kind, "event": json.RawMessage(res.Event)}
}

func (s *Server) rejection(r *http.Request, res wire.Result) (int, any) {
	var v struct {
		Code   string `json:"code"`
		SeatID string `json:"seat_id"`
	}
	_ = json.Unmarshal(res.Event, &v)
	code, msg := v.Code, strings.ToLower(strings.ReplaceAll(v.Code, "_", " "))
	var details map[string]any
	switch code {
	case inventory.CodeSeatTaken:
		msg, details = "seat is selected, held or sold by someone else", map[string]any{"seat_id": v.SeatID, "seatmap_version": res.Version}
	case inventory.CodeSeatNotInPool:
		msg, details = "seat unknown or in another class", map[string]any{"seat_id": v.SeatID}
	case inventory.CodeSelectionLimit:
		max := 4
		if res.Cart != nil {
			details = map[string]any{"max": max, "current": max - res.Cart.MaxSeats + len(res.Cart.Seats)}
		}
		msg = "you have reached the seat limit for this journey"
	case inventory.CodeCartConflict:
		msg = "your selection is for another class or trip; release it first"
		if res.Cart != nil {
			details = map[string]any{"cart": s.cartResponse(res.Cart)}
		}
	case inventory.CodeNotOpen:
		msg = "sales for this class are not open yet"
		if j, err := s.cat.Journey(r.Context(), res.JourneyID); err == nil && res.Cart != nil {
			if p := j.Pool(res.Cart.ClassCode, res.Cart.Quota); p != nil {
				details = map[string]any{"opens_at": p.OpensAt}
			}
		}
	case inventory.CodeActiveHoldExists:
		msg = "you already hold seats on this journey; pay or let the hold expire first"
		if res.Hold != nil {
			details = map[string]any{"pnr": res.Hold.PNR}
		}
	case inventory.CodeEmptyCart:
		msg = "your seat selection expired; select seats again"
	case inventory.CodePaxMismatch:
		msg = "one passenger per selected seat"
	case inventory.CodeSalesClosed:
		msg = "online sales for this train have closed"
	case inventory.CodeInvalidSegment:
		code, details = "VALIDATION_FAILED", map[string]any{"fields": map[string]string{"from_idx": "invalid trip or class"}}
	case inventory.CodeForbidden, inventory.CodeNotFound:
		code, msg = "NOT_FOUND", "unknown pnr"
	case inventory.CodeNotCancellable:
		msg, details = "booking cannot be cancelled", map[string]any{"reason": "status"}
	}
	st, ok := codeStatus[code]
	if !ok {
		st = http.StatusConflict
	}
	return st, errorBody(r, code, msg, details)
}

func passengerFare(adult int64, typ string) dto.Money {
	return dto.Money{AmountMinor: fares.PassengerFare(adult, typ == "CHILD"), Currency: fares.DefaultCurrency}
}

func (s *Server) holdResponse(ctx context.Context, reqID string, journeyID int64, sh *bookingv1.SeatHeld, info otp.Info) dto.HoldResponse {
	out := dto.HoldResponse{RequestID: reqID, PNR: sh.Pnr, Status: "PENDING_OTP", JourneyID: journeyID, ClassCode: sh.ClassCode,
		Passengers: []dto.PassengerSeat{}, Amount: dto.Money{AmountMinor: sh.AmountMinor, Currency: sh.Currency},
		ExpiresAt: sh.ExpiresAt.AsTime(), OTP: info}
	var layout *catalog.Layout
	var adult int64
	if j, err := s.cat.Journey(ctx, journeyID); err == nil {
		layout, _ = s.cat.Layout(j.LayoutID)
		if t, err := s.cat.Train(j.TrainNo); err == nil {
			out.TrainName = t.Name + " (" + t.TrainNo + ")"
			out.From, out.To = s.search.StopRef(j, t, int(sh.FromIdx), false), s.search.StopRef(j, t, int(sh.ToIdx), true)
			kids := make([]bool, len(sh.Passengers))
			for i, p := range sh.Passengers {
				kids[i] = p.Type == bookingv1.PassengerType_CHILD
			}
			_, charge := s.cat.Price(j, t, sh.ClassCode, int(sh.FromIdx), int(sh.ToIdx), kids)
			out.Charge = dto.Money{AmountMinor: charge.AmountMinor, Currency: charge.Currency}
			adult = s.cat.Fare(j, t, sh.ClassCode, int(sh.FromIdx), int(sh.ToIdx)).AmountMinor
		}
	}
	out.Fare = dto.Money{AmountMinor: sh.AmountMinor - out.Charge.AmountMinor, Currency: sh.Currency}
	for i, st := range sh.Seats {
		ps := dto.PassengerSeat{Seq: i + 1, Status: "HELD", Seat: s.seatRef(layout, st.SeatIdx, st.SeatId), Type: "ADULT"}
		if i < len(sh.Passengers) {
			p := sh.Passengers[i]
			ps.Name, ps.Gender = p.Name, p.Gender.String()
			if p.Type == bookingv1.PassengerType_CHILD {
				ps.Type = "CHILD"
			}
		}
		ps.Fare = passengerFare(adult, ps.Type)
		out.Passengers = append(out.Passengers, ps)
	}
	return out
}

// liveHold returns the allocator's hold summary if the hold is still payable.
func (s *Server) liveHold(ctx context.Context, pnr string, uid int64) (*wire.HoldSummary, string) {
	h, ok, err := s.store.Hold(ctx, pnr)
	switch {
	case err != nil || !ok || h.UserID != uid:
		return nil, "NOT_FOUND"
	case h.Status != "PENDING_PAYMENT" || !s.clock.Now().Before(h.ExpiresAt):
		return h, "HOLD_EXPIRED"
	}
	return h, ""
}

// POST /v1/holds/{pnr}/otp/verify (§14.2)
func (s *Server) verifyOTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := claims(r).UserID()
	pnr := r.PathValue("pnr")
	var req dto.VerifyOTPRequest
	if _, ok := decode(w, r, &req); !ok {
		return
	}
	if len(req.OTP) != 6 || strings.Trim(req.OTP, "0123456789") != "" {
		fail(w, r, "VALIDATION_FAILED", "the code has 6 digits", map[string]any{"fields": map[string]string{"otp": "6 digits"}})
		return
	}
	h, problem := s.liveHold(ctx, pnr, uid)
	if problem != "" {
		fail(w, r, problem, strings.ToLower(strings.ReplaceAll(problem, "_", " ")), nil)
		return
	}
	err := s.otp.Verify(ctx, pnr, uid, req.OTP)
	var invalid *otp.InvalidError
	var locked *otp.LockedError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, dto.VerifyOTPResponse{PNR: pnr, Status: "PENDING_PAYMENT", ExpiresAt: h.ExpiresAt})
	case errors.As(err, &invalid):
		fail(w, r, "OTP_INVALID", "wrong code", map[string]any{"attempts_left": invalid.AttemptsLeft})
	case errors.As(err, &locked):
		fail(w, r, "OTP_LOCKED", "too many attempts; request a new code", map[string]any{"retry_after": int(locked.RetryAfter.Seconds() + 0.999)})
	case errors.Is(err, otp.ErrExpired):
		fail(w, r, "OTP_EXPIRED", "the code expired; request a new one", nil)
	case errors.Is(err, otp.ErrNotFound), errors.Is(err, otp.ErrForbidden):
		fail(w, r, "NOT_FOUND", "unknown pnr", nil)
	default:
		internalErr(w, r, s, err)
	}
}

// POST /v1/holds/{pnr}/otp/resend (§14.3)
func (s *Server) resendOTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := claims(r).UserID()
	pnr := r.PathValue("pnr")
	if _, problem := s.liveHold(ctx, pnr, uid); problem != "" {
		fail(w, r, problem, strings.ToLower(strings.ReplaceAll(problem, "_", " ")), nil)
		return
	}
	info, err := s.otp.Resend(ctx, pnr, uid)
	var locked *otp.LockedError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, info)
	case errors.As(err, &locked):
		fail(w, r, "OTP_LOCKED", "no more codes for now", map[string]any{"retry_after": int(locked.RetryAfter.Seconds() + 0.999)})
	case errors.Is(err, otp.ErrNotFound), errors.Is(err, otp.ErrForbidden):
		fail(w, r, "NOT_FOUND", "unknown pnr", nil)
	default:
		internalErr(w, r, s, err)
	}
}

// GET /v1/requests/{requestId} (§15.1)
func (s *Server) requestStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("requestId")
	c := claims(r)
	res, found, err := s.store.Result(r.Context(), id)
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	if found {
		if res.UserID != c.UserID() && !isStaff(c.Role) {
			fail(w, r, "NOT_FOUND", "unknown request", nil)
			return
		}
		status, body := s.mapResult(r.Context(), r, res)
		b, _ := json.Marshal(body)
		writeJSON(w, http.StatusOK, dto.RequestStatus{RequestID: id, State: "DONE", StatusCode: status, Body: b})
		return
	}
	owner, pending, err := s.store.PendingOwner(r.Context(), id)
	if err != nil || !pending || (owner != c.UserID() && !isStaff(c.Role)) {
		fail(w, r, "NOT_FOUND", "unknown or expired request", nil)
		return
	}
	w.Header().Set("Retry-After", "1")
	writeJSON(w, http.StatusOK, dto.RequestStatus{RequestID: id, State: "PENDING"})
}

// GET /v1/requests/{requestId}/stream — SSE, one result event, pings every 10 s, at most 30 s.
func (s *Server) requestStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("requestId")
	c := claims(r)
	ctx := r.Context()
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, r, "BAD_REQUEST", "streaming unsupported", nil)
		return
	}
	owner, pending, _ := s.store.PendingOwner(ctx, id)
	res, found, _ := s.store.Result(ctx, id)
	switch {
	case found && (res.UserID == c.UserID() || isStaff(c.Role)):
	case !found && pending && (owner == c.UserID() || isStaff(c.Role)):
	default:
		fail(w, r, "NOT_FOUND", "unknown or expired request", nil)
		return
	}
	ch := s.waiter.Register(id)
	defer s.waiter.Forget(id)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	send := func(res wire.Result) {
		status, body := s.mapResult(ctx, r, res)
		b, _ := json.Marshal(body)
		msg, _ := json.Marshal(dto.RequestStatus{RequestID: id, State: "DONE", StatusCode: status, Body: b})
		fmt.Fprintf(w, "event: result\ndata: %s\n\n", msg)
		flusher.Flush()
	}
	if found {
		send(res)
		return
	}
	flusher.Flush()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	ping := time.NewTicker(10 * time.Second)
	defer ping.Stop()
	poll := time.NewTicker(time.Second) // the result may have gone to another pod's channel
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			fmt.Fprint(w, "event: timeout\ndata: {}\n\n")
			flusher.Flush()
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case res := <-ch:
			send(res)
			return
		case <-poll.C:
			if res, found, _ := s.store.Result(ctx, id); found {
				send(res)
				return
			}
		}
	}
}

// POST /v1/admission — local stand-in for the waiting room (ADMISSION_MODE=local).
func (s *Server) localAdmission(w http.ResponseWriter, r *http.Request) {
	var req dto.AdmissionRequest
	if _, ok := decode(w, r, &req); !ok {
		return
	}
	res, err := s.localAdm.Admit(r.Context(), claims(r).UserID(), s.clock.Now())
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	if !res.Admitted {
		w.Header().Set("Retry-After", strconv.Itoa(int(res.RetryIn.Seconds()+0.999)))
		writeJSON(w, http.StatusOK, dto.AdmissionResponse{Admitted: false, Position: res.Position})
		return
	}
	exp := res.ExpiresAt
	writeJSON(w, http.StatusOK, dto.AdmissionResponse{Admitted: true, AdmissionToken: res.Token, ExpiresAt: &exp})
}
