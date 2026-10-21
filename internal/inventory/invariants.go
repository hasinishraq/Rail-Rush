package inventory

import (
	"fmt"

	"google.golang.org/protobuf/proto"
)

// CheckInvariants verifies, for every seat, that the masks of its claims never
// overlap and that their OR equals the seat bitmap; that every claim belongs to
// exactly one live cart, hold or booking and vice versa; and that the per-pool
// free counts and per-user seat counts match. It returns one string per violation.
func CheckInvariants(j *Journey) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }

	type want struct {
		kind  ClaimKind
		owner int64
		pnr   string
		mask  uint64
	}
	expected := map[int32][]want{}
	for u, c := range j.Carts {
		if len(c.Seats) == 0 {
			add("cart of user %d is empty", u)
		}
		if p := j.Pool(c.Pool); p != nil && len(c.Seats) > p.MaxSeats && !isCounter(c.Channel) {
			add("cart of user %d has %d seats, max %d", u, len(c.Seats), p.MaxSeats)
		}
		for _, idx := range c.Seats {
			expected[idx] = append(expected[idx], want{Selected, u, "", c.Mask})
		}
	}
	userSeats := map[int64]int{}
	for _, h := range j.Holds {
		for _, idx := range h.Seats {
			expected[idx] = append(expected[idx], want{Held, h.UserID, h.PNR, h.Mask})
		}
		userSeats[h.UserID] += len(h.Seats)
	}
	for _, h := range j.Booked {
		for i, idx := range h.Seats {
			if !h.Cancelled[i] {
				expected[idx] = append(expected[idx], want{Booked, h.UserID, h.PNR, h.Mask})
			}
		}
		userSeats[h.UserID] += h.LiveSeats()
	}

	for i := range j.Seats {
		idx := int32(i)
		var or uint64
		for _, c := range j.Claims[i] {
			if or&c.Mask != 0 {
				add("seat %s: segment claimed twice (owner %d %s)", j.SeatIDs[i], c.Owner, c.PNR)
			}
			or |= c.Mask
		}
		if or != j.Seats[i] {
			add("seat %s: bitmap %x != claims %x", j.SeatIDs[i], j.Seats[i], or)
		}
		ws := expected[idx]
		if len(ws) != len(j.Claims[i]) {
			add("seat %s: %d claims, %d expected from carts/holds/bookings", j.SeatIDs[i], len(j.Claims[i]), len(ws))
			continue
		}
		for _, w := range ws {
			found := false
			for _, c := range j.Claims[i] {
				if c.Kind == w.kind && c.Owner == w.owner && c.PNR == w.pnr && c.Mask == w.mask {
					found = true
				}
			}
			if !found {
				add("seat %s: missing claim kind %d owner %d %s", j.SeatIDs[i], w.kind, w.owner, w.pnr)
			}
		}
	}
	for _, p := range j.Pools {
		for seg := range p.FreeOnSeg {
			n := int32(0)
			for _, idx := range p.SeatIdx {
				if j.Seats[idx]&(uint64(1)<<seg) == 0 {
					n++
				}
			}
			if n != p.FreeOnSeg[seg] {
				add("pool %s segment %d: free %d, counted %d", p.Key, seg, p.FreeOnSeg[seg], n)
			}
		}
	}
	for u, n := range userSeats {
		if j.UserSeats[u] != n {
			add("user %d: UserSeats %d, counted %d", u, j.UserSeats[u], n)
		}
	}
	for u, n := range j.UserSeats {
		if n != 0 && userSeats[u] == 0 {
			add("user %d: UserSeats %d, counted 0", u, n)
		}
	}
	return out
}

// SameState reports whether two journeys hold identical dynamic state; used to
// check that replaying the emitted events reproduces the live state exactly.
func SameState(a, b *Journey) bool {
	return proto.Equal(a.Snapshot(), b.Snapshot())
}
