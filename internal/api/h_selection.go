package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	bookingv1 "railway/gen/booking/v1"
	"railway/internal/admission"
	"railway/internal/api/dto"
	"railway/internal/catalog"
	"railway/internal/inventory"
	"railway/internal/platform/kafka"
	"railway/internal/platform/metrics"
	"railway/internal/wire"
)

var seatIDRe = regexp.MustCompile(`^[A-Z]{1,6}-[0-9]{1,3}$`)

func channelFor(role string) bookingv1.Channel {
	if role == "counter" {
		return bookingv1.Channel_COUNTER
	}
	return bookingv1.Channel_WEB
}

func isStaff(role string) bool { return role == "agent" || role == "counter" || role == "admin" }

func (s *Server) journeyParam(w http.ResponseWriter, r *http.Request) (*catalog.Journey, *catalog.Train, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, r, "NOT_FOUND", "unknown journey", nil)
		return nil, nil, false
	}
	j, err := s.cat.Journey(r.Context(), id)
	if err != nil {
		fail(w, r, "NOT_FOUND", "unknown journey", nil)
		return nil, nil, false
	}
	t, err := s.cat.Train(j.TrainNo)
	if err != nil {
		internalErr(w, r, s, err)
		return nil, nil, false
	}
	return j, t, true
}

func salesClosed(j *catalog.Journey, now time.Time) bool {
	return j.Status != "SCHEDULED" || !now.Before(j.SalesCloseAt)
}

// seatRef adds the coach and seat number from the layout.
func (s *Server) seatRef(l *catalog.Layout, idx int32, id string) dto.SeatRef {
	ref := dto.SeatRef{SeatID: id, SeatIdx: idx}
	if l != nil {
		if st, ok := l.SeatByID(id); ok {
			ref.Coach, ref.SeatNo = st.Coach, st.SeatNo
		}
	}
	return ref
}

// cartResponse turns cart:{journey}:{user} into the API shape.
func (s *Server) cartResponse(c *wire.CartSummary) dto.CartResponse {
	out := dto.CartResponse{JourneyID: c.JourneyID, FromIdx: c.FromIdx, ToIdx: c.ToIdx, ClassCode: c.ClassCode,
		Seats: []dto.SeatRef{}, MaxSeats: c.MaxSeats, SeatmapVersion: c.Version}
	var layout *catalog.Layout
	if j, err := s.cat.Journey(context.Background(), c.JourneyID); err == nil {
		layout, _ = s.cat.Layout(j.LayoutID)
	}
	for _, st := range c.Seats {
		out.Seats = append(out.Seats, s.seatRef(layout, st.SeatIdx, st.SeatID))
	}
	if len(c.Seats) > 0 && !c.ExpiresAt.IsZero() {
		t := c.ExpiresAt
		out.ExpiresAt = &t
	}
	return out
}

// selectLimit is select + unselect per user per journey (§9.6).
func (s *Server) selectLimit(w http.ResponseWriter, r *http.Request, uid, journeyID int64) bool {
	return s.rateLimit(w, r, "user:select", fmt.Sprintf("%d:%d", uid, journeyID), 30, time.Minute)
}

// PUT /v1/journeys/{id}/selection/{seatId} — lock one seat for the user (§13.1).
func (s *Server) selectSeat(w http.ResponseWriter, r *http.Request, _ *admission.Claims) {
	ctx := r.Context()
	c := claims(r)
	uid := c.UserID()
	j, t, ok := s.journeyParam(w, r)
	if !ok {
		return
	}
	seatID := strings.ToUpper(r.PathValue("seatId"))
	if !s.selectLimit(w, r, uid, j.JourneyID) {
		return
	}
	if n, _ := s.rdb.Exists(ctx, wire.CoolKey(uid, j.JourneyID)).Result(); n > 0 {
		fail(w, r, "RATE_LIMITED", "too many taken seats clicked; wait a moment", map[string]any{"retry_after": 5})
		return
	}
	var req dto.SelectRequest
	if _, ok := decode(w, r, &req); !ok {
		return
	}
	req.ClassCode, req.Quota = strings.ToUpper(req.ClassCode), strings.ToUpper(req.Quota)
	if req.Quota == "" {
		req.Quota = inventory.DefaultQuota
	}
	fields := map[string]string{}
	if !seatIDRe.MatchString(seatID) {
		fields["seat_id"] = `like "KHA-12"`
	}
	if req.FromIdx < 0 || req.ToIdx >= len(t.Stops) || req.FromIdx >= req.ToIdx {
		fields["from_idx"] = fmt.Sprintf("need 0 <= from_idx < to_idx <= %d", len(t.Stops)-1)
	}
	pool := j.Pool(req.ClassCode, req.Quota)
	if pool == nil {
		fields["class_code"] = "no such class on this journey"
	}
	if len(fields) > 0 {
		fail(w, r, "VALIDATION_FAILED", "invalid selection", map[string]any{"fields": fields})
		return
	}
	layout, err := s.cat.Layout(j.LayoutID)
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	seat, found := layout.SeatByID(seatID)
	if !found || seat.ClassCode != req.ClassCode {
		fail(w, r, "SEAT_NOT_IN_POOL", "seat unknown or in another class", map[string]any{"seat_id": seatID})
		return
	}

	// Fast rejections, no Kafka (§13.1 step 3). Each is conservative.
	now := s.clock.Now()
	counter := c.Role == "counter"
	fast := func(code, msg string, d map[string]any) {
		metrics.CommandResults.WithLabelValues("select", "FAST_"+code).Inc()
		fail(w, r, code, msg, d)
	}
	if salesClosed(j, now) {
		fast("SALES_CLOSED", "online sales for this train have closed", nil)
		return
	}
	if pool.OpensAt.After(now.Add(inventory.OpenSkew)) {
		w.Header().Set("Cache-Control", "public, max-age=1")
		fast("NOT_OPEN", "sales for this class are not open yet", map[string]any{"opens_at": pool.OpensAt})
		return
	}
	cart, hasCart, _ := s.store.Cart(ctx, j.JourneyID, uid)
	if hasCart {
		for _, st := range cart.Seats {
			if st.SeatID == seatID { // a double click is harmless: the current cart
				writeJSON(w, http.StatusOK, s.cartResponse(cart))
				return
			}
		}
	}
	if !counter {
		issued, _ := s.store.ULimit(ctx, uid, j.RunDateString())
		if issued >= s.cfg.DailySeatLimit {
			fast("ORDER_LIMIT_EXCEEDED", "you already have the maximum seats for this travel day",
				map[string]any{"limit": s.cfg.DailySeatLimit, "issued": issued})
			return
		}
	}
	if free, ok, err := s.store.SegFree(ctx, j.JourneyID, req.ClassCode, req.Quota); err == nil && ok {
		for seg := req.FromIdx; seg < req.ToIdx && seg < len(free); seg++ {
			if free[seg] == 0 {
				fast("SOLD_OUT", "no seat left for this trip in this class", nil)
				return
			}
		}
	}
	if taken, known := s.mirror.Taken(j.JourneyID, seat.SeatIdx, inventory.Mask(req.FromIdx, req.ToIdx)); known && taken {
		s.noteTaken(ctx, uid, j.JourneyID)
		fast("SEAT_TAKEN", "seat is taken", map[string]any{"seat_id": seatID})
		return
	}
	if hasCart && !counter && len(cart.Seats) >= cart.MaxSeats {
		fast("SELECTION_LIMIT", fmt.Sprintf("at most %d seats per journey", pool.MaxSeats),
			map[string]any{"max": pool.MaxSeats, "current": pool.MaxSeats - cart.MaxSeats + len(cart.Seats)})
		return
	}
	if lag, err := s.store.Lag(ctx, kafka.PartitionFor(j.JourneyID, s.cfg.Partitions)); err == nil && lag > time.Duration(s.cfg.LagRejectMS)*time.Millisecond {
		fast("OVERLOADED", "this train is very busy; try again shortly", map[string]any{"retry_after": 2})
		return
	}

	cmd := &bookingv1.Command{RequestId: newRequestID(), JourneyId: j.JourneyID, ReplyTo: wire.ResultsChannel(s.cfg.PodName),
		Body: &bookingv1.Command_Select{Select: &bookingv1.Select{UserId: uid, SeatId: seatID,
			FromIdx: int32(req.FromIdx), ToIdx: int32(req.ToIdx), ClassCode: req.ClassCode, Quota: req.Quota, Channel: channelFor(c.Role)}}}
	s.dispatch(w, r, cmd, uid, "select", func(_ int, res wire.Result) {
		if resultCode(res) == inventory.CodeSeatTaken {
			s.noteTaken(ctx, uid, j.JourneyID)
		}
	})
}

// noteTaken counts SEAT_TAKEN answers; 10 in 60 s give a 5 s cool-down,
// which slows scripts that sweep the map without hurting a person.
func (s *Server) noteTaken(ctx context.Context, uid, journeyID int64) {
	k := wire.TakenKey(uid, journeyID)
	n, err := s.rdb.Incr(ctx, k).Result()
	if err != nil {
		return
	}
	if n == 1 {
		s.rdb.Expire(ctx, k, time.Minute)
	}
	if n >= 10 {
		s.rdb.Set(ctx, wire.CoolKey(uid, journeyID), 1, 5*time.Second)
		s.rdb.Del(ctx, k)
	}
}

// DELETE /v1/journeys/{id}/selection/{seatId} — release one of your seats, or
// "*" for the whole cart (§13.2). Only the owner's locks can be released.
func (s *Server) unselectSeat(w http.ResponseWriter, r *http.Request, _ *admission.Claims) {
	uid := claims(r).UserID()
	j, _, ok := s.journeyParam(w, r)
	if !ok {
		return
	}
	seatID := strings.ToUpper(r.PathValue("seatId"))
	if seatID != "*" && !seatIDRe.MatchString(seatID) {
		fail(w, r, "VALIDATION_FAILED", "invalid seat id", map[string]any{"fields": map[string]string{"seat_id": `like "KHA-12" or "*"`}})
		return
	}
	if !s.selectLimit(w, r, uid, j.JourneyID) {
		return
	}
	if seatID == "*" {
		seatID = ""
	}
	cmd := &bookingv1.Command{RequestId: newRequestID(), JourneyId: j.JourneyID, ReplyTo: wire.ResultsChannel(s.cfg.PodName),
		Body: &bookingv1.Command_Release{Release: &bookingv1.Release{UserId: uid, SeatId: seatID}}}
	s.dispatch(w, r, cmd, uid, "release", nil)
}

// GET /v1/journeys/{id}/selection — my cart, to restore the page (§13.3).
func (s *Server) getSelection(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, r, "NOT_FOUND", "unknown journey", nil)
		return
	}
	cart, ok, err := s.store.Cart(r.Context(), id, claims(r).UserID())
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	if !ok {
		fail(w, r, "NOT_FOUND", "no live selection on this journey", nil)
		return
	}
	writeJSON(w, http.StatusOK, s.cartResponse(cart))
}
