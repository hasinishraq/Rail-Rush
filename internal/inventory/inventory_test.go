package inventory

import (
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"pgregory.net/rapid"

	bookingv1 "railway/gen/booking/v1"
)

var t0 = time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)

// testDef: coaches KA, KHA (S_CHAIR/ONLINE) and GA (SNIGDHA/ONLINE), 8 seats
// each, plus 4 COUNTER-quota seats in GA that have no pool; 5 segments.
func testDef() Definition {
	var seats []SeatDef
	for _, c := range []struct{ coach, class string }{{"KA", "S_CHAIR"}, {"KHA", "S_CHAIR"}, {"GA", "SNIGDHA"}} {
		for n := 1; n <= 8; n++ {
			q := "ONLINE"
			if c.coach == "GA" && n > 4 {
				q = "COUNTER"
			}
			seats = append(seats, SeatDef{SeatID: fmt.Sprintf("%s-%d", c.coach, n), Coach: c.coach, SeatNo: n, Class: c.class, Quota: q})
		}
	}
	return Definition{JourneyID: 1, Segments: 5, Seats: seats, Pools: []PoolDef{
		{Class: "S_CHAIR", Quota: "ONLINE", OpensAt: t0, MaxSeats: 4},
		{Class: "SNIGDHA", Quota: "ONLINE", OpensAt: t0, MaxSeats: 4},
	}}
}

func price(_ PoolKey, from, to int, pax []*bookingv1.Passenger) (int64, string) {
	return int64(len(pax)*(to-from)) * 10000, "BDT"
}

func newJourney(t interface{ Fatal(...any) }) *Journey {
	j, err := New(testDef(), Config{Price: price})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

var seq int

func cmd(body any) *bookingv1.Command {
	seq++
	c := &bookingv1.Command{RequestId: fmt.Sprintf("r%d", seq), JourneyId: 1}
	switch b := body.(type) {
	case *bookingv1.Select:
		c.Body = &bookingv1.Command_Select{Select: b}
	case *bookingv1.Release:
		c.Body = &bookingv1.Command_Release{Release: b}
	case *bookingv1.Hold:
		c.IdemKey = fmt.Sprintf("%d:h%d", b.UserId, seq)
		c.Body = &bookingv1.Command_Hold{Hold: b}
	case *bookingv1.Confirm:
		c.Body = &bookingv1.Command_Confirm{Confirm: b}
	case *bookingv1.Cancel:
		c.IdemKey = fmt.Sprintf("%d:c%d", b.UserId, seq)
		c.Body = &bookingv1.Command_Cancel{Cancel: b}
	case *bookingv1.Open:
		c.Body = &bookingv1.Command_Open{Open: b}
	case *bookingv1.Close:
		c.Body = &bookingv1.Command_Close{Close: b}
	}
	return c
}

func sel(user int64, seat string, from, to int32) *bookingv1.Command {
	return cmd(&bookingv1.Select{UserId: user, SeatId: seat, FromIdx: from, ToIdx: to, ClassCode: "S_CHAIR"})
}

func pax(n int) []*bookingv1.Passenger {
	out := make([]*bookingv1.Passenger, n)
	for i := range out {
		out[i] = &bookingv1.Passenger{Name: fmt.Sprintf("P%d", i+1), Gender: bookingv1.Gender_FEMALE, Type: bookingv1.PassengerType_ADULT}
	}
	return out
}

func kind(t *testing.T, evs []*bookingv1.Event, want string) *bookingv1.Event {
	t.Helper()
	if len(evs) == 0 {
		t.Fatalf("no events, want %s", want)
	}
	if got := Kind(evs[0]); got != want {
		t.Fatalf("got %s (%v), want %s", got, evs[0], want)
	}
	return evs[0]
}

func code(t *testing.T, evs []*bookingv1.Event) string {
	t.Helper()
	switch b := kind(t, evs, Kind(evs[0])).Body.(type) {
	case *bookingv1.Event_SelectRejected:
		return b.SelectRejected.Code
	case *bookingv1.Event_HoldRejected:
		return b.HoldRejected.Code
	case *bookingv1.Event_CancelRejected:
		return b.CancelRejected.Code
	case *bookingv1.Event_ConfirmFailed:
		return b.ConfirmFailed.Reason
	}
	t.Fatalf("not a rejection: %v", evs[0])
	return ""
}

func clean(t *testing.T, j *Journey) {
	t.Helper()
	if v := CheckInvariants(j); len(v) > 0 {
		t.Fatal(v)
	}
}

func TestSelectBeforeOpenAndWindowGate(t *testing.T) {
	j := newJourney(t)
	if c := code(t, j.Handle(sel(7, "KA-1", 0, 5), t0)); c != CodeNotOpen {
		t.Fatalf("code %s", c)
	}
	if evs := j.Handle(cmd(&bookingv1.Open{}), t0.Add(-3*time.Second)); len(evs) != 0 {
		t.Fatalf("early open produced %v", evs)
	}
	kind(t, j.Handle(cmd(&bookingv1.Open{}), t0.Add(-time.Second)), "Opened")
	kind(t, j.Handle(sel(7, "KA-1", 0, 5), t0), "SeatSelected")
}

func TestSelectLocksAgainstOthers(t *testing.T) {
	j := newJourney(t)
	j.Handle(cmd(&bookingv1.Open{}), t0)
	ev := kind(t, j.Handle(sel(1, "KA-1", 1, 3), t0), "SeatSelected").GetSeatSelected()
	if !ev.First || len(ev.Cart) != 1 || !ev.CartExpiresAt.AsTime().Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("got %v", ev)
	}
	if evs := j.Handle(sel(1, "KA-1", 1, 3), t0); len(evs) != 0 { // double click
		t.Fatalf("double click produced %v", evs)
	}
	if c := code(t, j.Handle(sel(2, "KA-1", 2, 5), t0)); c != CodeSeatTaken {
		t.Fatalf("code %s", c)
	}
	kind(t, j.Handle(sel(2, "KA-1", 3, 5), t0), "SeatSelected") // touching segments do not overlap
	if evs := j.Handle(cmd(&bookingv1.Release{UserId: 2, SeatId: "KA-2"}), t0); len(evs) != 0 {
		t.Fatalf("release of a seat not in the cart produced %v", evs)
	}
	if c := code(t, j.Handle(cmd(&bookingv1.Select{UserId: 1, SeatId: "GA-1", FromIdx: 1, ToIdx: 3, ClassCode: "SNIGDHA"}), t0)); c != CodeCartConflict {
		t.Fatalf("code %s", c)
	}
	if c := code(t, j.Handle(sel(1, "KA-2", 0, 3), t0)); c != CodeCartConflict {
		t.Fatalf("code %s", c)
	}
	if c := code(t, j.Handle(cmd(&bookingv1.Select{UserId: 3, SeatId: "GA-5", FromIdx: 0, ToIdx: 2, ClassCode: "SNIGDHA"}), t0)); c != CodeSeatNotInPool {
		t.Fatalf("code %s", c)
	}
	// Only the owner releases; after release the other user can take it.
	kind(t, j.Handle(cmd(&bookingv1.Release{UserId: 1, SeatId: "KA-1"}), t0), "SeatReleased")
	if j.Carts[1] != nil {
		t.Fatal("empty cart kept")
	}
	kind(t, j.Handle(sel(4, "KA-1", 0, 3), t0), "SeatSelected")
	clean(t, j)
}

func TestSelectionLimitAndTimers(t *testing.T) {
	j := newJourney(t)
	j.Handle(cmd(&bookingv1.Open{}), t0)
	for i := 1; i <= 4; i++ {
		kind(t, j.Handle(sel(1, fmt.Sprintf("KA-%d", i), 0, 5), t0.Add(time.Duration(i)*time.Minute)), "SeatSelected")
	}
	if c := code(t, j.Handle(sel(1, "KA-5", 0, 5), t0.Add(4*time.Minute))); c != CodeSelectionLimit {
		t.Fatalf("code %s", c)
	}
	// Cart deadline is capped at first + 5 min even though the last click was at +4 min.
	if got := j.Carts[1].ExpiresAt; !got.Equal(t0.Add(6 * time.Minute)) {
		t.Fatalf("deadline %v", got)
	}
	if evs := j.ExpireDue(t0.Add(5*time.Minute + 59*time.Second)); len(evs) != 0 {
		t.Fatalf("expired early: %v", evs)
	}
	ce := kind(t, j.ExpireDue(t0.Add(6*time.Minute)), "CartExpired").GetCartExpired()
	if len(ce.Seats) != 4 || j.Carts[1] != nil || j.Seats[0] != 0 {
		t.Fatalf("got %v", ce)
	}
	// Idle timeout: 2 min after the last click.
	kind(t, j.Handle(sel(2, "KA-1", 0, 5), t0.Add(10*time.Minute)), "SeatSelected")
	kind(t, j.ExpireDue(t0.Add(12*time.Minute)), "CartExpired")
	clean(t, j)
}

func TestHoldConfirmCancel(t *testing.T) {
	j := newJourney(t)
	j.Handle(cmd(&bookingv1.Open{}), t0)
	if c := code(t, j.Handle(cmd(&bookingv1.Hold{Pnr: "1000000001", UserId: 1, Passengers: pax(1)}), t0)); c != CodeEmptyCart {
		t.Fatalf("code %s", c)
	}
	j.Handle(sel(1, "KA-1", 0, 2), t0)
	j.Handle(sel(1, "KA-2", 0, 2), t0)
	if c := code(t, j.Handle(cmd(&bookingv1.Hold{Pnr: "1000000001", UserId: 1, Passengers: pax(1)}), t0)); c != CodePaxMismatch {
		t.Fatalf("code %s", c)
	}
	before := append([]uint64(nil), j.Seats...)
	sh := kind(t, j.Handle(cmd(&bookingv1.Hold{Pnr: "1000000002", UserId: 1, Passengers: pax(2)}), t0), "SeatHeld").GetSeatHeld()
	if len(sh.Seats) != 2 || sh.Seats[0].SeatId != "KA-1" || sh.AmountMinor != 40000 || !sh.ExpiresAt.AsTime().Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("got %v", sh)
	}
	for i := range before {
		if before[i] != j.Seats[i] {
			t.Fatal("bitmap changed when a cart became a hold")
		}
	}
	if j.Carts[1] != nil || j.UserSeats[1] != 2 {
		t.Fatal("cart not converted")
	}
	// A live hold blocks new selections and a second hold.
	if c := code(t, j.Handle(sel(1, "KA-3", 0, 2), t0)); c != CodeActiveHoldExists {
		t.Fatalf("code %s", c)
	}
	if c := code(t, j.Handle(cmd(&bookingv1.Confirm{Pnr: "1000000002", PaymentId: "p1", AmountMinor: 1}), t0)); c != ReasonAmountMismatch {
		t.Fatalf("code %s", c)
	}
	kind(t, j.Handle(cmd(&bookingv1.Confirm{Pnr: "1000000002", PaymentId: "p1", AmountMinor: 40000, CapturedAt: timestamppb.New(t0)}), t0), "Confirmed")
	if evs := j.Handle(cmd(&bookingv1.Confirm{Pnr: "1000000002", PaymentId: "p1", AmountMinor: 40000}), t0); len(evs) != 0 {
		t.Fatalf("duplicate confirm produced %v", evs)
	}
	if c := code(t, j.Handle(cmd(&bookingv1.Confirm{Pnr: "1000000002", PaymentId: "p2", AmountMinor: 40000}), t0)); c != ReasonAlreadyConfirmed {
		t.Fatalf("code %s", c)
	}
	// Booked seats still count toward the limit: 2 booked + 2 selected = 4.
	j.Handle(sel(1, "KA-3", 0, 2), t0)
	j.Handle(sel(1, "KA-4", 0, 2), t0)
	if c := code(t, j.Handle(sel(1, "KA-5", 0, 2), t0)); c != CodeSelectionLimit {
		t.Fatalf("code %s", c)
	}
	if c := code(t, j.Handle(cmd(&bookingv1.Cancel{Pnr: "1000000002", UserId: 9, CancelId: "c"}), t0)); c != CodeForbidden {
		t.Fatalf("code %s", c)
	}
	kind(t, j.Handle(cmd(&bookingv1.Cancel{Pnr: "1000000002", UserId: 1, CancelId: "c", PassengerSeqs: []int32{2}}), t0), "Cancelled")
	if j.Seats[1] != 0 || j.Seats[0] == 0 || j.UserSeats[1] != 1 {
		t.Fatal("partial cancel did not free exactly seat 2")
	}
	if c := code(t, j.Handle(cmd(&bookingv1.Cancel{Pnr: "1000000002", UserId: 1, CancelId: "d", PassengerSeqs: []int32{2}}), t0)); c != CodeNotCancellable {
		t.Fatalf("code %s", c)
	}
	clean(t, j)
}

func TestHoldExpiryAndLateConfirm(t *testing.T) {
	j := newJourney(t)
	j.Handle(cmd(&bookingv1.Open{}), t0)
	hold := func(user int64, pnr string, seats ...string) {
		for _, s := range seats {
			kind(t, j.Handle(sel(user, s, 0, 5), t0), "SeatSelected")
		}
		kind(t, j.Handle(cmd(&bookingv1.Hold{Pnr: pnr, UserId: user, Passengers: pax(len(seats))}), t0), "SeatHeld")
	}
	hold(1, "1000000001", "KA-1")
	hold(2, "1000000002", "KA-2", "KA-3")
	hold(3, "1000000003", "KA-4")
	exp := t0.Add(10 * time.Minute)
	evs := j.ExpireDue(exp)
	if n := len(evs); n != 3 || len(j.Holds) != 0 || len(j.Expired) != 3 {
		t.Fatalf("expired %d events %v", n, evs)
	}
	// Original seat still free: Confirmed{late} on it.
	c := kind(t, j.Handle(cmd(&bookingv1.Confirm{Pnr: "1000000001", PaymentId: "p1", AmountMinor: 50000, CapturedAt: timestamppb.New(exp.Add(time.Minute))}), exp), "Confirmed").GetConfirmed()
	if !c.Late || c.SeatsChanged || c.Seats[0].SeatId != "KA-1" {
		t.Fatalf("got %v", c)
	}
	// KA-2 taken meanwhile; on-time payer gets the nearest free seat.
	j.Handle(sel(9, "KA-2", 0, 5), exp)
	c = kind(t, j.Handle(cmd(&bookingv1.Confirm{Pnr: "1000000002", PaymentId: "p2", AmountMinor: 100000, CapturedAt: timestamppb.New(exp.Add(20 * time.Second))}), exp), "Confirmed").GetConfirmed()
	if !c.SeatsChanged || c.Seats[0].SeatId == "KA-2" || c.Seats[1].SeatId != "KA-3" {
		t.Fatalf("got %v", c)
	}
	// KA-4 taken and the payment came too late: ConfirmFailed -> refund.
	j.Handle(cmd(&bookingv1.Release{UserId: 9}), exp)
	j.Handle(sel(8, "KA-4", 0, 5), exp)
	if r := code(t, j.Handle(cmd(&bookingv1.Confirm{Pnr: "1000000003", PaymentId: "p3", AmountMinor: 50000, CapturedAt: timestamppb.New(exp.Add(time.Minute))}), exp)); r != ReasonNoSeat {
		t.Fatalf("reason %s", r)
	}
	clean(t, j)
}

func TestCloseAndDeadlinesExtended(t *testing.T) {
	j := newJourney(t)
	j.Handle(cmd(&bookingv1.Open{}), t0)
	j.Handle(sel(1, "KA-1", 0, 5), t0)
	j.Handle(sel(2, "KA-2", 0, 5), t0)
	j.Handle(cmd(&bookingv1.Hold{Pnr: "1000000002", UserId: 2, Passengers: pax(1)}), t0)
	j.Handle(sel(3, "KA-3", 0, 5), t0)
	kind(t, j.ExtendDeadlines(time.Minute, t0), "DeadlinesExtended")
	if !j.Carts[1].ExpiresAt.Equal(t0.Add(3*time.Minute)) || !j.Holds["1000000002"].ExpiresAt.Equal(t0.Add(11*time.Minute)) {
		t.Fatal("deadlines not extended")
	}
	evs := j.Handle(cmd(&bookingv1.Close{}), t0)
	if Kind(evs[0]) != "SalesClosed" || len(j.Carts) != 0 || len(j.Holds) != 1 {
		t.Fatalf("got %v", evs)
	}
	if c := code(t, j.Handle(sel(4, "KA-5", 0, 5), t0)); c != CodeSalesClosed {
		t.Fatalf("code %s", c)
	}
	kind(t, j.Handle(cmd(&bookingv1.Confirm{Pnr: "1000000002", PaymentId: "p", AmountMinor: 50000}), t0), "Confirmed")
	clean(t, j)
}

func TestSoldOutAndAvailable(t *testing.T) {
	j := newJourney(t)
	j.Handle(cmd(&bookingv1.Open{}), t0)
	var last []*bookingv1.Event
	for i := 1; i <= 4; i++ {
		last = j.Handle(cmd(&bookingv1.Select{UserId: int64(i), SeatId: fmt.Sprintf("GA-%d", i), FromIdx: 0, ToIdx: 5, ClassCode: "SNIGDHA"}), t0)
	}
	if len(last) != 2 || Kind(last[1]) != "SoldOut" {
		t.Fatalf("got %v", last)
	}
	evs := j.Handle(cmd(&bookingv1.Release{UserId: 1}), t0)
	if len(evs) != 2 || Kind(evs[1]) != "Available" {
		t.Fatalf("got %v", evs)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	j := newJourney(t)
	j.Handle(cmd(&bookingv1.Open{}), t0)
	j.Handle(sel(1, "KA-1", 0, 2), t0)
	j.Handle(sel(2, "KA-2", 1, 3), t0)
	j.Handle(cmd(&bookingv1.Hold{Pnr: "1000000002", UserId: 2, Passengers: pax(1)}), t0)
	j.Handle(sel(3, "KA-3", 0, 5), t0)
	j.Handle(cmd(&bookingv1.Hold{Pnr: "1000000003", UserId: 3, Passengers: pax(1)}), t0)
	j.Handle(cmd(&bookingv1.Confirm{Pnr: "1000000003", PaymentId: "p", AmountMinor: 50000}), t0)
	b, err := proto.Marshal(j.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var s bookingv1.JourneySnapshot
	if err := proto.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	k := newJourney(t)
	if err := k.Restore(&s); err != nil {
		t.Fatal(err)
	}
	if !SameState(j, k) {
		t.Fatal("snapshot round trip differs")
	}
	clean(t, k)
	for i := range j.Seats {
		if j.Seats[i] != k.Seats[i] {
			t.Fatal("bitmaps differ")
		}
	}
	at, _ := k.NextDeadline()
	if !at.Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("timers not re-armed: %v", at)
	}
}

// Property test (§25 test 1): thousands of random Select / Release / Hold /
// Confirm / Expire / Cancel / Close sequences over random segments. No claims
// overlap, carts stay within the limit, and replaying the emitted events into
// a fresh journey reproduces the live state exactly.
func TestPropertyNoDoubleSellAndReplay(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		j := newJourney(rt)
		var log []*bookingv1.Event
		now := t0
		emit := func(evs []*bookingv1.Event) { log = append(log, evs...) }
		emit(j.Handle(cmd(&bookingv1.Open{}), now))
		pnrs := []string{}
		steps := rapid.IntRange(1, 150).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			now = now.Add(time.Duration(rapid.IntRange(0, 40).Draw(rt, "dt")) * time.Second)
			user := int64(rapid.IntRange(1, 6).Draw(rt, "user"))
			switch rapid.IntRange(0, 9).Draw(rt, "op") {
			case 0, 1, 2, 3:
				from := rapid.IntRange(0, 4).Draw(rt, "from")
				to := rapid.IntRange(from+1, 5).Draw(rt, "to")
				coach := rapid.SampledFrom([]string{"KA", "KHA", "GA"}).Draw(rt, "coach")
				class := "S_CHAIR"
				if coach == "GA" {
					class = "SNIGDHA"
				}
				seat := fmt.Sprintf("%s-%d", coach, rapid.IntRange(1, 8).Draw(rt, "seat"))
				ch := bookingv1.Channel_WEB
				if user == 6 {
					ch = bookingv1.Channel_COUNTER
				}
				emit(j.Handle(cmd(&bookingv1.Select{UserId: user, SeatId: seat, FromIdx: int32(from), ToIdx: int32(to), ClassCode: class, Channel: ch}), now))
			case 4:
				seat := ""
				if c := j.Carts[user]; c != nil && rapid.Bool().Draw(rt, "one") {
					seat = j.SeatIDs[c.Seats[0]]
				}
				emit(j.Handle(cmd(&bookingv1.Release{UserId: user, SeatId: seat}), now))
			case 5:
				n := 1
				if c := j.Carts[user]; c != nil {
					n = len(c.Seats)
				}
				pnr := fmt.Sprintf("%010d", 1000000000+i)
				evs := j.Handle(cmd(&bookingv1.Hold{Pnr: pnr, UserId: user, Passengers: pax(n)}), now)
				if len(evs) > 0 && Kind(evs[0]) == "SeatHeld" {
					pnrs = append(pnrs, pnr)
				}
				emit(evs)
			case 6:
				if len(pnrs) > 0 {
					pnr := rapid.SampledFrom(pnrs).Draw(rt, "pnr")
					if h, _ := j.Lookup(pnr); h != nil {
						amt := h.AmountMinor
						cap := now.Add(-time.Duration(rapid.IntRange(0, 120).Draw(rt, "late")) * time.Second)
						emit(j.Handle(cmd(&bookingv1.Confirm{Pnr: pnr, PaymentId: "pay-" + pnr, AmountMinor: amt, CapturedAt: timestamppb.New(cap)}), now))
					}
				}
			case 7:
				if len(pnrs) > 0 {
					pnr := rapid.SampledFrom(pnrs).Draw(rt, "pnr")
					var seqs []int32
					if rapid.Bool().Draw(rt, "partial") {
						seqs = []int32{int32(rapid.IntRange(1, 4).Draw(rt, "seq"))}
					}
					owner := user
					if h, _ := j.Lookup(pnr); h != nil && rapid.Bool().Draw(rt, "owner") {
						owner = h.UserID
					}
					emit(j.Handle(cmd(&bookingv1.Cancel{Pnr: pnr, UserId: owner, CancelId: fmt.Sprint(i), PassengerSeqs: seqs}), now))
				}
			case 8:
				emit(j.ExpireDue(now))
			case 9:
				if rapid.IntRange(0, 20).Draw(rt, "close") == 0 {
					emit(j.Handle(cmd(&bookingv1.Close{}), now))
				} else {
					emit(j.ExtendDeadlines(time.Duration(rapid.IntRange(1, 60).Draw(rt, "ext"))*time.Second, now))
				}
			}
			if v := CheckInvariants(j); len(v) > 0 {
				rt.Fatalf("step %d: %v", i, v)
			}
		}
		// Replay through the wire format, exactly as after a failover.
		k := newJourney(rt)
		for _, ev := range log {
			b, _ := proto.Marshal(ev)
			var c bookingv1.Event
			if err := proto.Unmarshal(b, &c); err != nil {
				rt.Fatal(err)
			}
			k.Apply(&c)
		}
		if !SameState(j, k) {
			rt.Fatalf("replay differs from live state")
		}
		for i := range j.Seats {
			if j.Seats[i] != k.Seats[i] {
				rt.Fatalf("seat %d bitmap differs after replay", i)
			}
		}
		for i, ev := range log {
			if ev.Version != int64(i+1) {
				rt.Fatalf("event %d has version %d", i, ev.Version)
			}
		}
	})
}
