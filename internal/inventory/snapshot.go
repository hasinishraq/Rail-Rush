package inventory

import (
	"fmt"
	"sort"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	bookingv1 "railway/gen/booking/v1"
)

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func tm(t *timestamppb.Timestamp) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.AsTime()
}

func sortedPNRs(m map[string]*Hold) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (j *Journey) holdState(h *Hold) *bookingv1.HoldState {
	p := j.Pools[h.Pool]
	hs := &bookingv1.HoldState{
		Pnr: h.PNR, UserId: h.UserID, ClassCode: p.Key.Class, Quota: p.Key.Quota,
		FromIdx: int32(h.From), ToIdx: int32(h.To), Seats: append([]int32(nil), h.Seats...), Passengers: h.Pax,
		ExpiresAt: ts(h.ExpiresAt), AmountMinor: h.AmountMinor, Currency: h.Currency, Channel: h.Channel,
		PaymentId: h.PaymentID, ExpiredAt: ts(h.ExpiredAt), CreatedAt: ts(h.CreatedAt),
		Late: h.Late, SeatsChanged: h.SeatsChanged,
	}
	for i, c := range h.Cancelled {
		if c {
			hs.CancelledSeqs = append(hs.CancelledSeqs, int32(i+1))
		}
	}
	return hs
}

func (j *Journey) holdsState(m map[string]*Hold) []*bookingv1.HoldState {
	out := make([]*bookingv1.HoldState, 0, len(m))
	for _, k := range sortedPNRs(m) {
		out = append(out, j.holdState(m[k]))
	}
	return out
}

// Snapshot captures the dynamic state of the journey, deterministically.
func (j *Journey) Snapshot() *bookingv1.JourneySnapshot {
	s := &bookingv1.JourneySnapshot{
		JourneyId: j.ID, Version: j.Version, Closed: j.Closed, LastEvent: ts(j.LastEvent),
		Holds: j.holdsState(j.Holds), Booked: j.holdsState(j.Booked), Expired: j.holdsState(j.Expired),
	}
	for i, cs := range j.Claims {
		if len(cs) == 0 {
			continue
		}
		sc := &bookingv1.SeatClaims{SeatIdx: int32(i)}
		for _, c := range cs {
			sc.Claims = append(sc.Claims, &bookingv1.ClaimState{Mask: c.Mask, Kind: int32(c.Kind), Owner: c.Owner, Pnr: c.PNR})
		}
		s.Claims = append(s.Claims, sc)
	}
	for _, p := range j.Pools {
		s.Pools = append(s.Pools, &bookingv1.PoolState{ClassCode: p.Key.Class, Quota: p.Key.Quota, Open: p.Open, SoldOut: p.SoldOut})
	}
	users := make([]int64, 0, len(j.Carts))
	for u := range j.Carts {
		users = append(users, u)
	}
	sort.Slice(users, func(a, b int) bool { return users[a] < users[b] })
	for _, u := range users {
		c := j.Carts[u]
		p := j.Pools[c.Pool]
		s.Carts = append(s.Carts, &bookingv1.CartState{
			UserId: u, ClassCode: p.Key.Class, Quota: p.Key.Quota, FromIdx: int32(c.From), ToIdx: int32(c.To),
			Seats: append([]int32(nil), c.Seats...), FirstAt: ts(c.FirstAt), ExpiresAt: ts(c.ExpiresAt), Channel: c.Channel,
		})
	}
	keys := make([]string, 0, len(j.Seen))
	for k := range j.Seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := j.Seen[k]
		s.Seen = append(s.Seen, &bookingv1.SeenEntry{IdemKey: k, UserId: e.UserID, Event: e.Event})
	}
	return s
}

func (j *Journey) restoreHold(hs *bookingv1.HoldState) (*Hold, error) {
	p := j.PoolByKey[PoolKey{hs.ClassCode, hs.Quota}]
	if p == nil {
		return nil, fmt.Errorf("snapshot pool %s/%s no longer exists", hs.ClassCode, hs.Quota)
	}
	from, to := int(hs.FromIdx), int(hs.ToIdx)
	h := &Hold{
		PNR: hs.Pnr, UserID: hs.UserId, Pool: p.Index, From: from, To: to, Mask: Mask(from, to),
		Seats: append([]int32(nil), hs.Seats...), Pax: hs.Passengers, ExpiresAt: tm(hs.ExpiresAt),
		AmountMinor: hs.AmountMinor, Currency: hs.Currency, Channel: hs.Channel, PaymentID: hs.PaymentId,
		ExpiredAt: tm(hs.ExpiredAt), CreatedAt: tm(hs.CreatedAt), Late: hs.Late, SeatsChanged: hs.SeatsChanged,
	}
	h.Cancelled = make([]bool, len(h.Seats))
	for _, s := range hs.CancelledSeqs {
		if s >= 1 && int(s) <= len(h.Seats) {
			h.Cancelled[s-1] = true
		}
	}
	for _, idx := range h.Seats {
		if !j.validSeat(idx) {
			return nil, fmt.Errorf("snapshot seat %d out of range", idx)
		}
	}
	return h, nil
}

// Restore overlays a snapshot on an empty journey built by New from the same
// layout. It fails if the layout no longer matches, in which case the caller
// rebuilds from the full log instead.
func (j *Journey) Restore(s *bookingv1.JourneySnapshot) error {
	if s.JourneyId != j.ID {
		return fmt.Errorf("snapshot is for journey %d, not %d", s.JourneyId, j.ID)
	}
	for _, sc := range s.Claims {
		idx := sc.SeatIdx
		if !j.validSeat(idx) {
			return fmt.Errorf("snapshot seat %d out of range", idx)
		}
		for _, c := range sc.Claims {
			j.addClaim(idx, Claim{Mask: c.Mask, Kind: ClaimKind(c.Kind), Owner: c.Owner, PNR: c.Pnr})
		}
	}
	for _, ps := range s.Pools {
		p := j.PoolByKey[PoolKey{ps.ClassCode, ps.Quota}]
		if p == nil {
			return fmt.Errorf("snapshot pool %s/%s no longer exists", ps.ClassCode, ps.Quota)
		}
		p.Open, p.SoldOut = ps.Open, ps.SoldOut
	}
	for _, cs := range s.Carts {
		p := j.PoolByKey[PoolKey{cs.ClassCode, cs.Quota}]
		if p == nil {
			return fmt.Errorf("snapshot pool %s/%s no longer exists", cs.ClassCode, cs.Quota)
		}
		from, to := int(cs.FromIdx), int(cs.ToIdx)
		j.Carts[cs.UserId] = &Cart{UserID: cs.UserId, Pool: p.Index, From: from, To: to, Mask: Mask(from, to),
			Seats: append([]int32(nil), cs.Seats...), FirstAt: tm(cs.FirstAt), ExpiresAt: tm(cs.ExpiresAt), Channel: cs.Channel}
	}
	load := func(list []*bookingv1.HoldState, into map[string]*Hold, live bool) error {
		for _, hs := range list {
			h, err := j.restoreHold(hs)
			if err != nil {
				return err
			}
			into[h.PNR] = h
			if live && !isCounter(h.Channel) {
				j.UserHold[h.UserID] = h.PNR
			}
			j.UserSeats[h.UserID] += h.LiveSeats()
		}
		return nil
	}
	if err := load(s.Holds, j.Holds, true); err != nil {
		return err
	}
	if err := load(s.Booked, j.Booked, false); err != nil {
		return err
	}
	for _, hs := range s.Expired {
		h, err := j.restoreHold(hs)
		if err != nil {
			return err
		}
		j.Expired[h.PNR] = h
	}
	for _, e := range s.Seen {
		j.Seen[e.IdemKey] = &SeenEntry{UserID: e.UserId, Event: e.Event}
	}
	j.Version, j.Closed, j.LastEvent = s.Version, s.Closed, tm(s.LastEvent)
	j.rebuildTimers()
	j.TakeDirty()
	clear(j.dirtyPools)
	return nil
}
