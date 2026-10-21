package inventory

import (
	"container/heap"
	"sort"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	bookingv1 "railway/gen/booking/v1"
)

// OpenSkew is how far ahead of opens_at an Open command is still honoured.
// The allocator, not the scheduler, is the final gate for the window opening.
const OpenSkew = 2 * time.Second

// Rejection codes carried in SelectRejected, HoldRejected and CancelRejected.
const (
	CodeNotOpen          = "NOT_OPEN"
	CodeSalesClosed      = "SALES_CLOSED"
	CodeSeatTaken        = "SEAT_TAKEN"
	CodeSeatNotInPool    = "SEAT_NOT_IN_POOL"
	CodeSelectionLimit   = "SELECTION_LIMIT"
	CodeCartConflict     = "CART_CONFLICT"
	CodeActiveHoldExists = "ACTIVE_HOLD_EXISTS"
	CodeInvalidSegment   = "INVALID_SEGMENT"
	CodeEmptyCart        = "EMPTY_CART"
	CodePaxMismatch      = "PASSENGER_COUNT_MISMATCH"
	CodeNotFound         = "NOT_FOUND"
	CodeForbidden        = "FORBIDDEN"
	CodeNotCancellable   = "NOT_CANCELLABLE"
)

// ConfirmFailed reasons.
const (
	ReasonAlreadyConfirmed = "ALREADY_CONFIRMED"
	ReasonAmountMismatch   = "AMOUNT_MISMATCH"
	ReasonUnknownPNR       = "UNKNOWN_PNR"
	ReasonSalesClosed      = "SALES_CLOSED"
	ReasonNoSeat           = "NO_SEAT"
)

func newID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

func (j *Journey) newEvent(cmd *bookingv1.Command, now time.Time) *bookingv1.Event {
	ev := &bookingv1.Event{EventId: newID(), JourneyId: j.ID, At: timestamppb.New(now)}
	if cmd != nil {
		ev.RequestId, ev.IdemKey = cmd.RequestId, cmd.IdemKey
	}
	return ev
}

func one(ev *bookingv1.Event) []*bookingv1.Event { return []*bookingv1.Event{ev} }

func isCounter(c bookingv1.Channel) bool { return c == bookingv1.Channel_COUNTER }

// cartDeadline is min(now + idle TTL, first + max TTL).
func (j *Journey) cartDeadline(first, now time.Time) time.Time {
	d := now.Add(j.cfg.SelectIdleTTL)
	if m := first.Add(j.cfg.SelectMaxTTL); m.Before(d) {
		return m
	}
	return d
}

// Decide validates a command against current state and returns the events
// that describe the outcome. It does NOT change j. A nil result means the
// command changes nothing (a double click, an idempotent unselect, an early
// Open); the caller answers with the current state. Follow-up events
// (SoldOut, Available) are produced by Handle.
func (j *Journey) Decide(cmd *bookingv1.Command, now time.Time) []*bookingv1.Event {
	switch b := cmd.Body.(type) {
	case *bookingv1.Command_Select:
		return j.decideSelect(cmd, b.Select, now)
	case *bookingv1.Command_Release:
		return j.decideRelease(cmd, b.Release, now)
	case *bookingv1.Command_Hold:
		return j.decideHold(cmd, b.Hold, now)
	case *bookingv1.Command_Confirm:
		return j.decideConfirm(cmd, b.Confirm, now)
	case *bookingv1.Command_Cancel:
		return j.decideCancel(cmd, b.Cancel, now)
	case *bookingv1.Command_Warm:
		ev := j.newEvent(cmd, now)
		ev.Body = &bookingv1.Event_Warmed{Warmed: &bookingv1.JourneyWarmed{}}
		return one(ev)
	case *bookingv1.Command_Open:
		return j.decideOpen(cmd, b.Open, now)
	case *bookingv1.Command_Close:
		return j.decideClose(cmd, now)
	}
	return nil
}

// Handle is the live path: Decide, then Apply each event so that later
// commands in the same batch see the result, then the follow-up events.
func (j *Journey) Handle(cmd *bookingv1.Command, now time.Time) []*bookingv1.Event {
	evs := j.Decide(cmd, now)
	for _, ev := range evs {
		j.Apply(ev)
	}
	return append(evs, j.settle(now)...)
}

// ExpireDue releases every cart and hold whose deadline has passed and applies
// the resulting events (CartExpired, HoldExpired, Available).
func (j *Journey) ExpireDue(now time.Time) []*bookingv1.Event {
	var out []*bookingv1.Event
	for {
		at, ok := j.NextDeadline()
		if !ok || at.After(now) {
			break
		}
		d := heap.Pop(&j.Timers).(deadline)
		ev := j.newEvent(nil, now)
		switch d.Kind {
		case cartTimer:
			ev.Body = &bookingv1.Event_CartExpired{CartExpired: &bookingv1.CartExpired{
				UserId: d.User, Seats: j.seatRefs(j.Carts[d.User].Seats)}}
		case holdTimer:
			ev.Body = &bookingv1.Event_HoldExpired{HoldExpired: &bookingv1.HoldExpired{Pnr: d.PNR}}
		}
		j.Apply(ev)
		out = append(out, ev)
	}
	if len(out) == 0 {
		return nil
	}
	return append(out, j.settle(now)...)
}

// ExtendDeadlines emits DeadlinesExtended after a log outage, so nobody loses
// a cart or a hold because the log was down. Nil if nothing is live.
func (j *Journey) ExtendDeadlines(delta time.Duration, now time.Time) []*bookingv1.Event {
	if (len(j.Carts) == 0 && len(j.Holds) == 0) || delta <= 0 {
		return nil
	}
	ev := j.newEvent(nil, now)
	ev.Body = &bookingv1.Event_DeadlinesExtended{DeadlinesExtended: &bookingv1.DeadlinesExtended{DeltaMs: delta.Milliseconds()}}
	j.Apply(ev)
	return one(ev)
}

// settle emits SoldOut / Available for pools whose bitmaps changed.
func (j *Journey) settle(now time.Time) []*bookingv1.Event {
	if len(j.dirtyPools) == 0 {
		return nil
	}
	idx := make([]int, 0, len(j.dirtyPools))
	for pi := range j.dirtyPools {
		idx = append(idx, int(pi))
	}
	clear(j.dirtyPools)
	sort.Ints(idx)
	var out []*bookingv1.Event
	for _, pi := range idx {
		p := j.Pools[pi]
		f := full(p)
		if f == p.SoldOut {
			continue
		}
		ev := j.newEvent(nil, now)
		if f {
			ev.Body = &bookingv1.Event_SoldOut{SoldOut: &bookingv1.SoldOut{ClassCode: p.Key.Class, Quota: p.Key.Quota}}
		} else {
			ev.Body = &bookingv1.Event_Available{Available: &bookingv1.Available{ClassCode: p.Key.Class, Quota: p.Key.Quota}}
		}
		j.Apply(ev)
		out = append(out, ev)
	}
	clear(j.dirtyPools)
	return out
}

// ---------------------------------------------------------------- Select

func (j *Journey) selectRejected(cmd *bookingv1.Command, now time.Time, s *bookingv1.Select, code string) []*bookingv1.Event {
	ev := j.newEvent(cmd, now)
	ev.Body = &bookingv1.Event_SelectRejected{SelectRejected: &bookingv1.SelectRejected{UserId: s.UserId, SeatId: s.SeatId, Code: code}}
	return one(ev)
}

func (j *Journey) decideSelect(cmd *bookingv1.Command, s *bookingv1.Select, now time.Time) []*bookingv1.Event {
	if j.Closed {
		return j.selectRejected(cmd, now, s, CodeSalesClosed)
	}
	quota := s.Quota
	if quota == "" {
		quota = DefaultQuota
	}
	p := j.PoolByKey[PoolKey{s.ClassCode, quota}]
	from, to := int(s.FromIdx), int(s.ToIdx)
	if p == nil || from < 0 || from >= to || to > j.Segments {
		return j.selectRejected(cmd, now, s, CodeInvalidSegment)
	}
	if !p.Open {
		return j.selectRejected(cmd, now, s, CodeNotOpen)
	}
	idx, ok := j.SeatByID[s.SeatId]
	if !ok || j.PoolOf[idx] != p.Index {
		return j.selectRejected(cmd, now, s, CodeSeatNotInPool)
	}
	counter := isCounter(s.Channel)
	if _, live := j.UserHold[s.UserId]; live && !counter {
		return j.selectRejected(cmd, now, s, CodeActiveHoldExists)
	}
	cart := j.Carts[s.UserId]
	if cart != nil && (cart.Pool != p.Index || cart.From != from || cart.To != to) {
		return j.selectRejected(cmd, now, s, CodeCartConflict)
	}
	if cart != nil && containsInt32(cart.Seats, idx) {
		return nil // double click: the caller replies with the current cart
	}
	inCart := 0
	if cart != nil {
		inCart = len(cart.Seats)
	}
	if !counter && inCart+j.UserSeats[s.UserId] >= p.MaxSeats {
		return j.selectRejected(cmd, now, s, CodeSelectionLimit)
	}
	m := Mask(from, to)
	if !Free(j.Seats[idx], m) {
		return j.selectRejected(cmd, now, s, CodeSeatTaken) // never says who holds it
	}
	first := now
	var seats []int32
	if cart != nil {
		first = cart.FirstAt
		seats = append(seats, cart.Seats...)
	}
	seats = append(seats, idx)
	ev := j.newEvent(cmd, now)
	ev.Body = &bookingv1.Event_SeatSelected{SeatSelected: &bookingv1.SeatSelected{
		UserId: s.UserId, Seat: j.seatRef(idx), FromIdx: s.FromIdx, ToIdx: s.ToIdx,
		ClassCode: p.Key.Class, Quota: p.Key.Quota, Cart: j.seatRefs(seats),
		CartExpiresAt: timestamppb.New(j.cartDeadline(first, now)), First: cart == nil, Channel: s.Channel,
	}}
	return one(ev)
}

// ---------------------------------------------------------------- Release

func (j *Journey) decideRelease(cmd *bookingv1.Command, r *bookingv1.Release, now time.Time) []*bookingv1.Event {
	cart := j.Carts[r.UserId]
	if cart == nil {
		return nil
	}
	var release []int32
	if r.SeatId == "" || r.SeatId == "*" {
		release = append(release, cart.Seats...)
	} else {
		idx, ok := j.SeatByID[r.SeatId]
		if !ok || !containsInt32(cart.Seats, idx) {
			return nil // not yours: nobody can release someone else's lock
		}
		release = []int32{idx}
	}
	sr := &bookingv1.SeatReleased{UserId: r.UserId, Seats: j.seatRefs(release)}
	if len(release) < len(cart.Seats) {
		sr.CartExpiresAt = timestamppb.New(j.cartDeadline(cart.FirstAt, now))
	}
	ev := j.newEvent(cmd, now)
	ev.Body = &bookingv1.Event_SeatReleased{SeatReleased: sr}
	return one(ev)
}

// ---------------------------------------------------------------- Hold

func (j *Journey) holdRejected(cmd *bookingv1.Command, now time.Time, h *bookingv1.Hold, code string) []*bookingv1.Event {
	ev := j.newEvent(cmd, now)
	ev.Body = &bookingv1.Event_HoldRejected{HoldRejected: &bookingv1.HoldRejected{Pnr: h.Pnr, UserId: h.UserId, Code: code}}
	return one(ev)
}

func (j *Journey) decideHold(cmd *bookingv1.Command, h *bookingv1.Hold, now time.Time) []*bookingv1.Event {
	if j.Closed {
		return j.holdRejected(cmd, now, h, CodeSalesClosed)
	}
	cart := j.Carts[h.UserId]
	if cart == nil {
		if _, live := j.UserHold[h.UserId]; live {
			return j.holdRejected(cmd, now, h, CodeActiveHoldExists)
		}
		return j.holdRejected(cmd, now, h, CodeEmptyCart) // expired or never made: select again
	}
	if len(h.Passengers) != len(cart.Seats) {
		return j.holdRejected(cmd, now, h, CodePaxMismatch)
	}
	if old, _ := j.Lookup(h.Pnr); old != nil || h.Pnr == "" {
		return j.holdRejected(cmd, now, h, CodeEmptyCart)
	}
	p := j.Pools[cart.Pool]
	amount, cur := j.cfg.Price(p.Key, cart.From, cart.To, h.Passengers)
	ev := j.newEvent(cmd, now)
	ev.Body = &bookingv1.Event_SeatHeld{SeatHeld: &bookingv1.SeatHeld{
		Pnr: h.Pnr, UserId: h.UserId, FromIdx: int32(cart.From), ToIdx: int32(cart.To),
		ClassCode: p.Key.Class, Quota: p.Key.Quota, Passengers: h.Passengers, Seats: j.seatRefs(cart.Seats),
		ExpiresAt: timestamppb.New(now.Add(j.cfg.HoldTTL)), AmountMinor: amount, Currency: cur, Channel: h.Channel,
	}}
	return one(ev)
}

// ---------------------------------------------------------------- Confirm

func (j *Journey) confirmFailed(cmd *bookingv1.Command, now time.Time, c *bookingv1.Confirm, reason string) []*bookingv1.Event {
	ev := j.newEvent(cmd, now)
	ev.Body = &bookingv1.Event_ConfirmFailed{ConfirmFailed: &bookingv1.ConfirmFailed{Pnr: c.Pnr, PaymentId: c.PaymentId, Reason: reason}}
	return one(ev)
}

func (j *Journey) decideConfirm(cmd *bookingv1.Command, c *bookingv1.Confirm, now time.Time) []*bookingv1.Event {
	if b := j.Booked[c.Pnr]; b != nil {
		if b.PaymentID == c.PaymentId {
			return nil // already confirmed with this payment: idempotent
		}
		return j.confirmFailed(cmd, now, c, ReasonAlreadyConfirmed) // a second capture: refunded
	}
	confirmed := func(seats []int32, late, changed bool) []*bookingv1.Event {
		ev := j.newEvent(cmd, now)
		ev.Body = &bookingv1.Event_Confirmed{Confirmed: &bookingv1.Confirmed{
			Pnr: c.Pnr, PaymentId: c.PaymentId, Seats: j.seatRefs(seats), Late: late, SeatsChanged: changed}}
		return one(ev)
	}
	if h := j.Holds[c.Pnr]; h != nil {
		if c.AmountMinor != h.AmountMinor {
			return j.confirmFailed(cmd, now, c, ReasonAmountMismatch)
		}
		return confirmed(h.Seats, false, false)
	}
	e := j.Expired[c.Pnr]
	if e == nil {
		return j.confirmFailed(cmd, now, c, ReasonUnknownPNR)
	}
	if c.AmountMinor != e.AmountMinor {
		return j.confirmFailed(cmd, now, c, ReasonAmountMismatch)
	}
	if j.Closed {
		return j.confirmFailed(cmd, now, c, ReasonSalesClosed)
	}
	allFree := true
	for _, idx := range e.Seats {
		if !Free(j.Seats[idx], e.Mask) {
			allFree = false
		}
	}
	if allFree {
		return confirmed(e.Seats, true, false)
	}
	onTime := c.CapturedAt != nil && !c.CapturedAt.AsTime().After(e.ExpiresAt.Add(j.cfg.LateGrace))
	if onTime {
		if seats := j.nearestFree(e); seats != nil {
			return confirmed(seats, true, true)
		}
	}
	return j.confirmFailed(cmd, now, c, ReasonNoSeat)
}

// nearestFree keeps each original seat that is still free and replaces the
// others with the nearest free seat of the same pool, preferring the same coach.
func (j *Journey) nearestFree(h *Hold) []int32 {
	p := j.Pools[h.Pool]
	out := make([]int32, len(h.Seats))
	used := map[int32]bool{}
	for i, idx := range h.Seats {
		if Free(j.Seats[idx], h.Mask) {
			out[i], used[idx] = idx, true
		} else {
			out[i] = -1
		}
	}
	for i, orig := range h.Seats {
		if out[i] >= 0 {
			continue
		}
		best, bestScore := int32(-1), 0
		for _, idx := range p.SeatIdx {
			if used[idx] || !Free(j.Seats[idx], h.Mask) {
				continue
			}
			d := int(idx - orig)
			if d < 0 {
				d = -d
			}
			if j.Coach[idx] != j.Coach[orig] {
				d += 1 << 20 // another coach only when this one is full
			}
			if best < 0 || d < bestScore {
				best, bestScore = idx, d
			}
		}
		if best < 0 {
			return nil
		}
		out[i], used[best] = best, true
	}
	return out
}

// ---------------------------------------------------------------- Cancel

func (j *Journey) cancelRejected(cmd *bookingv1.Command, now time.Time, c *bookingv1.Cancel, code string) []*bookingv1.Event {
	ev := j.newEvent(cmd, now)
	ev.Body = &bookingv1.Event_CancelRejected{CancelRejected: &bookingv1.CancelRejected{Pnr: c.Pnr, UserId: c.UserId, Code: code}}
	return one(ev)
}

func (j *Journey) decideCancel(cmd *bookingv1.Command, c *bookingv1.Cancel, now time.Time) []*bookingv1.Event {
	h := j.Booked[c.Pnr]
	if h == nil {
		if live, _ := j.Lookup(c.Pnr); live != nil {
			return j.cancelRejected(cmd, now, c, CodeNotCancellable)
		}
		return j.cancelRejected(cmd, now, c, CodeNotFound)
	}
	if h.UserID != c.UserId && !isCounter(c.Channel) {
		return j.cancelRejected(cmd, now, c, CodeForbidden)
	}
	var seqs []int32
	if len(c.PassengerSeqs) == 0 {
		for i := range h.Seats {
			if !h.Cancelled[i] {
				seqs = append(seqs, int32(i+1))
			}
		}
	} else {
		dup := map[int32]bool{}
		for _, s := range c.PassengerSeqs {
			if s < 1 || int(s) > len(h.Seats) || h.Cancelled[s-1] || dup[s] {
				return j.cancelRejected(cmd, now, c, CodeNotCancellable)
			}
			dup[s] = true
			seqs = append(seqs, s)
		}
		sort.Slice(seqs, func(a, b int) bool { return seqs[a] < seqs[b] })
	}
	if len(seqs) == 0 {
		return j.cancelRejected(cmd, now, c, CodeNotCancellable)
	}
	ev := j.newEvent(cmd, now)
	ev.Body = &bookingv1.Event_Cancelled{Cancelled: &bookingv1.Cancelled{Pnr: c.Pnr, CancelId: c.CancelId, PassengerSeqs: seqs}}
	return one(ev)
}

// ---------------------------------------------------------------- Open / Close

func (j *Journey) decideOpen(cmd *bookingv1.Command, o *bookingv1.Open, now time.Time) []*bookingv1.Event {
	due := func(p *Pool) bool { return p != nil && !p.Open && !p.OpensAt.After(now.Add(OpenSkew)) }
	var pools []*bookingv1.Pool
	if len(o.GetPools()) == 0 {
		for _, p := range j.Pools {
			if due(p) {
				pools = append(pools, &bookingv1.Pool{ClassCode: p.Key.Class, Quota: p.Key.Quota})
			}
		}
	} else {
		for _, op := range o.Pools {
			if due(j.PoolByKey[PoolKey{op.ClassCode, op.Quota}]) {
				pools = append(pools, &bookingv1.Pool{ClassCode: op.ClassCode, Quota: op.Quota})
			}
		}
	}
	if len(pools) == 0 || j.Closed {
		return nil
	}
	ev := j.newEvent(cmd, now)
	ev.Body = &bookingv1.Event_Opened{Opened: &bookingv1.Opened{Pools: pools}}
	return one(ev)
}

// IgnoredOpenPools lists pools named in an Open command that are not due yet,
// so the caller can log them (Decide itself never logs).
func (j *Journey) IgnoredOpenPools(o *bookingv1.Open, now time.Time) []PoolKey {
	var out []PoolKey
	for _, op := range o.GetPools() {
		p := j.PoolByKey[PoolKey{op.ClassCode, op.Quota}]
		if p != nil && !p.Open && p.OpensAt.After(now.Add(OpenSkew)) {
			out = append(out, p.Key)
		}
	}
	return out
}

// decideClose: SalesClosed, then every cart released in the same batch. Live
// holds keep their timers so users already paying can finish (W15).
func (j *Journey) decideClose(cmd *bookingv1.Command, now time.Time) []*bookingv1.Event {
	if j.Closed {
		return nil
	}
	sc := j.newEvent(cmd, now)
	sc.Body = &bookingv1.Event_SalesClosed{SalesClosed: &bookingv1.SalesClosed{}}
	out := one(sc)
	users := make([]int64, 0, len(j.Carts))
	for u := range j.Carts {
		users = append(users, u)
	}
	sort.Slice(users, func(a, b int) bool { return users[a] < users[b] })
	for _, u := range users {
		ev := j.newEvent(nil, now)
		ev.Body = &bookingv1.Event_CartExpired{CartExpired: &bookingv1.CartExpired{UserId: u, Seats: j.seatRefs(j.Carts[u].Seats)}}
		out = append(out, ev)
	}
	return out
}
