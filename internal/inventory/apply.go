package inventory

import (
	"strconv"
	"strings"
	"time"

	bookingv1 "railway/gen/booking/v1"
)

const (
	seenTTL    = 30 * time.Minute
	expiredTTL = 24 * time.Hour
)

// Apply changes state for one event. It is used both live (after Decide) and
// on replay, and reads nothing but the event and the current state: never the
// clock, Redis or Postgres.
func (j *Journey) Apply(ev *bookingv1.Event) {
	at := ev.GetAt().AsTime()
	switch b := ev.Body.(type) {
	case *bookingv1.Event_SeatSelected:
		j.applySelected(b.SeatSelected, at)
	case *bookingv1.Event_SeatReleased:
		r := b.SeatReleased
		if c := j.Carts[r.UserId]; c != nil {
			for _, s := range r.Seats {
				j.unlock(c, s.SeatIdx)
			}
			if len(c.Seats) == 0 || r.CartExpiresAt == nil {
				j.dropCart(c)
			} else {
				c.ExpiresAt = r.CartExpiresAt.AsTime()
				j.armCart(c)
			}
		}
	case *bookingv1.Event_CartExpired:
		if c := j.Carts[b.CartExpired.UserId]; c != nil {
			for _, idx := range append([]int32(nil), c.Seats...) {
				j.unlock(c, idx)
			}
			j.dropCart(c)
		}
	case *bookingv1.Event_SeatHeld:
		j.applyHeld(b.SeatHeld, at)
	case *bookingv1.Event_Confirmed:
		j.applyConfirmed(b.Confirmed)
	case *bookingv1.Event_HoldExpired:
		if h := j.Holds[b.HoldExpired.Pnr]; h != nil {
			for _, idx := range h.Seats {
				j.removeClaims(idx, func(c *Claim) bool { return c.PNR == h.PNR })
			}
			j.UserSeats[h.UserID] -= len(h.Seats)
			j.dropLive(h)
			h.ExpiredAt = at
			j.Expired[h.PNR] = h
		}
	case *bookingv1.Event_DeadlinesExtended:
		d := time.Duration(b.DeadlinesExtended.DeltaMs) * time.Millisecond
		for _, c := range j.Carts {
			c.ExpiresAt, c.FirstAt = c.ExpiresAt.Add(d), c.FirstAt.Add(d)
		}
		for _, h := range j.Holds {
			h.ExpiresAt = h.ExpiresAt.Add(d)
		}
		j.rebuildTimers()
	case *bookingv1.Event_Cancelled:
		if h := j.Booked[b.Cancelled.Pnr]; h != nil {
			for _, s := range b.Cancelled.PassengerSeqs {
				i := int(s) - 1
				if i < 0 || i >= len(h.Seats) || h.Cancelled[i] {
					continue
				}
				h.Cancelled[i] = true
				j.removeClaims(h.Seats[i], func(c *Claim) bool { return c.PNR == h.PNR })
				j.UserSeats[h.UserID]--
			}
		}
	case *bookingv1.Event_SoldOut:
		if p := j.PoolByKey[PoolKey{b.SoldOut.ClassCode, b.SoldOut.Quota}]; p != nil {
			p.SoldOut = true
		}
	case *bookingv1.Event_Available:
		if p := j.PoolByKey[PoolKey{b.Available.ClassCode, b.Available.Quota}]; p != nil {
			p.SoldOut = false
		}
	case *bookingv1.Event_Opened:
		for _, op := range b.Opened.Pools {
			if p := j.PoolByKey[PoolKey{op.ClassCode, op.Quota}]; p != nil {
				p.Open = true
			}
		}
	case *bookingv1.Event_SalesClosed:
		j.Closed = true
	case *bookingv1.Event_SelectRejected, *bookingv1.Event_HoldRejected, *bookingv1.Event_CancelRejected,
		*bookingv1.Event_ConfirmFailed, *bookingv1.Event_Warmed:
		// no inventory change
	}

	if ev.IdemKey != "" {
		j.Seen[ev.IdemKey] = &SeenEntry{UserID: userOf(ev), Event: ev}
	}
	j.Version++
	if ev.Version == 0 {
		ev.Version = j.Version
	}
	j.LastEvent = at
	if j.Version%256 == 0 {
		j.prune(at)
	}
}

// userOf extracts the requesting user from the idempotency key "{user_id}:{key}",
// for the owner check on re-sent results.
func userOf(ev *bookingv1.Event) int64 {
	if i := strings.IndexByte(ev.IdemKey, ':'); i > 0 {
		if id, err := strconv.ParseInt(ev.IdemKey[:i], 10, 64); err == nil {
			return id
		}
	}
	return 0
}

// prune drops idempotency entries older than 30 min and expired holds older
// than 24 h. It is driven by event time, so replay prunes identically.
func (j *Journey) prune(at time.Time) {
	for k, s := range j.Seen {
		if at.Sub(s.Event.GetAt().AsTime()) > seenTTL {
			delete(j.Seen, k)
		}
	}
	for k, h := range j.Expired {
		if at.Sub(h.ExpiredAt) > expiredTTL {
			delete(j.Expired, k)
		}
	}
}

func (j *Journey) applySelected(s *bookingv1.SeatSelected, at time.Time) {
	p := j.PoolByKey[PoolKey{s.ClassCode, s.Quota}]
	idx := s.GetSeat().GetSeatIdx()
	if p == nil || !j.validSeat(idx) {
		return
	}
	from, to := int(s.FromIdx), int(s.ToIdx)
	c := j.Carts[s.UserId]
	if c == nil || s.First {
		c = &Cart{UserID: s.UserId, Pool: p.Index, From: from, To: to, Mask: Mask(from, to), FirstAt: at, Channel: s.Channel}
		j.Carts[s.UserId] = c
	}
	j.addClaim(idx, Claim{Mask: c.Mask, Kind: Selected, Owner: s.UserId})
	c.Seats = seatIdxs(s.Cart)
	c.ExpiresAt = s.CartExpiresAt.AsTime()
	j.armCart(c)
}

// unlock removes one Selected claim of the cart's owner and the seat from the cart.
func (j *Journey) unlock(c *Cart, idx int32) {
	if !j.validSeat(idx) || !containsInt32(c.Seats, idx) {
		return
	}
	j.removeClaims(idx, func(cl *Claim) bool { return cl.Kind == Selected && cl.Owner == c.UserID })
	c.Seats = removeInt32(c.Seats, idx)
}

func (j *Journey) dropCart(c *Cart) { delete(j.Carts, c.UserID) }

func (j *Journey) applyHeld(sh *bookingv1.SeatHeld, at time.Time) {
	p := j.PoolByKey[PoolKey{sh.ClassCode, sh.Quota}]
	if p == nil {
		return
	}
	from, to := int(sh.FromIdx), int(sh.ToIdx)
	h := &Hold{
		PNR: sh.Pnr, UserID: sh.UserId, Pool: p.Index, From: from, To: to, Mask: Mask(from, to),
		Seats: seatIdxs(sh.Seats), Pax: sh.Passengers, ExpiresAt: sh.ExpiresAt.AsTime(),
		AmountMinor: sh.AmountMinor, Currency: sh.Currency, Channel: sh.Channel, CreatedAt: at,
	}
	h.Cancelled = make([]bool, len(h.Seats))
	// Selected -> Held on the same claims: the bitmap does not change, so there
	// is no moment when the seats are free.
	for _, idx := range h.Seats {
		if j.validSeat(idx) {
			j.setKind(idx, h.Mask, h.UserID, Selected, "", Held, h.PNR)
		}
	}
	if c := j.Carts[h.UserID]; c != nil {
		for _, idx := range c.Seats { // anything left in the cart (should be nothing)
			if !containsInt32(h.Seats, idx) {
				j.unlock(c, idx)
			}
		}
		j.dropCart(c)
	}
	j.Holds[h.PNR] = h
	j.UserSeats[h.UserID] += len(h.Seats)
	if !isCounter(h.Channel) {
		j.UserHold[h.UserID] = h.PNR
	}
	j.armHold(h)
}

func (j *Journey) applyConfirmed(c *bookingv1.Confirmed) {
	seats := seatIdxs(c.Seats)
	if h := j.Holds[c.Pnr]; h != nil {
		for i, idx := range h.Seats {
			if i < len(seats) && seats[i] == idx {
				j.setKind(idx, h.Mask, h.UserID, Held, h.PNR, Booked, h.PNR)
			} else {
				j.removeClaims(idx, func(cl *Claim) bool { return cl.PNR == h.PNR })
			}
		}
		for i, idx := range seats {
			if (i >= len(h.Seats) || h.Seats[i] != idx) && j.validSeat(idx) {
				j.addClaim(idx, Claim{Mask: h.Mask, Kind: Booked, Owner: h.UserID, PNR: h.PNR})
			}
		}
		j.UserSeats[h.UserID] += len(seats) - len(h.Seats)
		j.dropLive(h)
		h.Seats = seats
		h.Cancelled = make([]bool, len(seats))
		h.PaymentID = c.PaymentId
		j.Booked[h.PNR] = h
		return
	}
	h := j.Expired[c.Pnr]
	if h == nil {
		return
	}
	delete(j.Expired, c.Pnr)
	for _, idx := range seats {
		if j.validSeat(idx) {
			j.addClaim(idx, Claim{Mask: h.Mask, Kind: Booked, Owner: h.UserID, PNR: h.PNR})
		}
	}
	j.UserSeats[h.UserID] += len(seats)
	h.Seats = seats
	h.Cancelled = make([]bool, len(seats))
	h.PaymentID, h.Late, h.SeatsChanged = c.PaymentId, c.Late, c.SeatsChanged
	j.Booked[h.PNR] = h
}

func (j *Journey) dropLive(h *Hold) {
	delete(j.Holds, h.PNR)
	if j.UserHold[h.UserID] == h.PNR {
		delete(j.UserHold, h.UserID)
	}
}
