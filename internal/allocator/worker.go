package allocator

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	bookingv1 "railway/gen/booking/v1"
	"railway/internal/catalog"
	"railway/internal/inventory"
	"railway/internal/wire"
)

// resident is one journey held in memory by its partition's worker.
type resident struct {
	j  *inventory.Journey
	cj *catalog.Journey

	lastOffset    int64        // last committed event offset for this journey
	pendingOffset atomic.Int64 // max offset produced in the current transaction
	lastUsed      time.Time

	eventsSinceSnap int
	lastSnap        time.Time

	mapVersion int64 // version of the last published seat map
	forceMap   bool  // publish the seat map on the next commit, not on the ticker
	lastMapPub time.Time

	deltaV    int64   // version up to which deltas were published
	dirty     []int32 // seats changed since the last delta
	dirtyMark map[int32]bool
	lastDelta time.Time
}

// reply is a command answer to publish after the commit. The summaries are
// taken right after the decision, so a later command in the same batch does
// not change what this caller is told.
type reply struct {
	requestID string
	replyTo   string
	userID    int64
	journeyID int64
	kind      string
	ev        *bookingv1.Event // nil for KindCart
	version   int64
	cart      *wire.CartSummary
	hold      *wire.HoldSummary
}

// worker owns one partition: every journey on it has exactly this one writer.
type worker struct {
	part int32
	a    *Allocator
	log  *slog.Logger

	journeys map[int64]*resident
	reload   []int64 // journeys to reload eagerly after a dropped state

	// per-iteration output
	out       []*bookingv1.Event
	outRes    []*resident
	replies   []reply
	ephemeral []reply // answers that need no commit (unknown journey)
	carts     map[*resident]map[int64]bool
	pnrs      map[*resident]map[string]bool
	touched   map[*resident]bool
	lag       time.Duration
	abort     bool
}

func newWorker(a *Allocator, part int32) *worker {
	return &worker{part: part, a: a, log: a.log.With("partition", part), journeys: map[int64]*resident{}}
}

func (w *worker) resetOutput() {
	w.out, w.outRes, w.replies, w.ephemeral = w.out[:0], w.outRes[:0], w.replies[:0], w.ephemeral[:0]
	w.carts = map[*resident]map[int64]bool{}
	w.pnrs = map[*resident]map[string]bool{}
	w.touched = map[*resident]bool{}
	w.abort = false
}

// journey returns the resident journey, loading it (snapshot + log tail) on first use.
func (w *worker) journey(ctx context.Context, id int64, now time.Time) (*resident, error) {
	if r := w.journeys[id]; r != nil {
		r.lastUsed = now
		return r, nil
	}
	start := time.Now()
	ld, err := w.a.loader.Load(ctx, id, true)
	if err != nil {
		return nil, err
	}
	if ld.Partition != w.part {
		return nil, fmt.Errorf("journey %d hashes to partition %d, not %d", id, ld.Partition, w.part)
	}
	ld.J.TakeDirty() // replay marks seats dirty; the published map is rebuilt below
	r := &resident{j: ld.J, cj: ld.CJ, lastOffset: ld.LastOffset, lastUsed: now, lastSnap: now,
		mapVersion: -1, forceMap: true, deltaV: ld.J.Version, dirtyMark: map[int32]bool{}}
	r.pendingOffset.Store(ld.LastOffset)
	w.journeys[id] = r
	w.log.Info("journey loaded", "journey", id, "replayed", ld.Replayed, "version", ld.J.Version, "took", time.Since(start))
	return r, nil
}

func (w *worker) emit(r *resident, evs []*bookingv1.Event) {
	if len(evs) == 0 {
		return
	}
	w.touched[r] = true
	for _, ev := range evs {
		w.out = append(w.out, ev)
		w.outRes = append(w.outRes, r)
		if u := inventory.CartUserOf(ev); u != 0 {
			w.markCart(r, u)
		}
		if p := inventory.PNROf(ev); p != "" {
			w.markPNR(r, p)
		}
		switch ev.Body.(type) {
		case *bookingv1.Event_Opened, *bookingv1.Event_SalesClosed, *bookingv1.Event_SoldOut, *bookingv1.Event_Available:
			r.forceMap = true // publish at once, not on the 1 s ticker
		case *bookingv1.Event_DeadlinesExtended:
			for u := range r.j.Carts {
				w.markCart(r, u)
			}
			for pnr := range r.j.Holds {
				w.markPNR(r, pnr)
			}
		}
	}
	for _, idx := range r.j.TakeDirty() {
		if !r.dirtyMark[idx] {
			r.dirtyMark[idx] = true
			r.dirty = append(r.dirty, idx)
		}
	}
}

func (w *worker) markCart(r *resident, u int64) {
	m := w.carts[r]
	if m == nil {
		m = map[int64]bool{}
		w.carts[r] = m
	}
	m[u] = true
}

func (w *worker) markPNR(r *resident, pnr string) {
	m := w.pnrs[r]
	if m == nil {
		m = map[string]bool{}
		w.pnrs[r] = m
	}
	m[pnr] = true
}

func commandUser(cmd *bookingv1.Command) int64 {
	switch b := cmd.Body.(type) {
	case *bookingv1.Command_Select:
		return b.Select.UserId
	case *bookingv1.Command_Release:
		return b.Release.UserId
	case *bookingv1.Command_Hold:
		return b.Hold.UserId
	case *bookingv1.Command_Cancel:
		return b.Cancel.UserId
	}
	return 0
}

// process decides every command of the batch and runs cart and hold timers.
// It changes only RAM; nothing is acknowledged until the allocator commits.
func (w *worker) process(ctx context.Context, recs []*kgo.Record, now time.Time, extend time.Duration) {
	w.resetOutput()
	w.lag = 0

	if len(w.reload) > 0 {
		ids := w.reload
		w.reload = nil
		for _, id := range ids {
			if _, err := w.journey(ctx, id, now); err != nil {
				w.log.Error("reload failed", "journey", id, "err", err)
				w.reload = append(w.reload, id)
			}
		}
	}
	if extend > 0 { // the log was down: nobody loses a cart or hold because of it (W12)
		for _, r := range w.journeys {
			w.emit(r, r.j.ExtendDeadlines(extend, now))
		}
	}

	for _, rec := range recs {
		if age := now.Sub(rec.Timestamp); age > w.lag {
			w.lag = age
		}
		var cmd bookingv1.Command
		if err := proto.Unmarshal(rec.Value, &cmd); err != nil {
			w.log.Error("undecodable command skipped", "offset", rec.Offset, "err", err)
			continue
		}
		w.handle(ctx, &cmd, now)
	}

	for _, r := range w.journeys {
		w.emit(r, r.j.ExpireDue(now))
	}
}

func (w *worker) handle(ctx context.Context, cmd *bookingv1.Command, now time.Time) {
	if w.a.poisoned(cmd.RequestId) {
		w.log.Error("poison command skipped after repeated panics", "journey", cmd.JourneyId, "request", cmd.RequestId)
		return
	}
	defer func() {
		if p := recover(); p != nil {
			// State may be half-applied: abort the whole batch so every journey
			// reloads from the committed log, and count the panic.
			w.log.Error("panic deciding command; aborting batch", "journey", cmd.JourneyId, "request", cmd.RequestId, "panic", p)
			w.a.notePanic(cmd.RequestId)
			w.abort = true
		}
	}()
	user := commandUser(cmd)
	base := reply{requestID: cmd.RequestId, replyTo: cmd.ReplyTo, userID: user, journeyID: cmd.JourneyId}
	r, err := w.journey(ctx, cmd.JourneyId, now)
	if err != nil {
		w.log.Warn("command for unloadable journey", "journey", cmd.JourneyId, "err", err)
		if ev := unknownJourney(cmd, now); ev != nil && cmd.RequestId != "" {
			base.kind, base.ev = inventory.Kind(ev), ev
			w.ephemeral = append(w.ephemeral, base)
		}
		return
	}
	if cmd.IdemKey != "" {
		if seen := r.j.Seen[cmd.IdemKey]; seen != nil { // retry -> identical answer
			w.replies = append(w.replies, w.answer(base, r, seen.Event))
			return
		}
	}
	if o := cmd.GetOpen(); o != nil {
		if ign := r.j.IgnoredOpenPools(o, now); len(ign) > 0 {
			w.log.Warn("open ignored: pools not due yet", "journey", cmd.JourneyId, "pools", fmt.Sprint(ign))
		}
	}
	evs := r.j.Handle(cmd, now)
	w.emit(r, evs)
	if cmd.RequestId == "" {
		return
	}
	for _, ev := range evs {
		if ev.RequestId == cmd.RequestId {
			w.replies = append(w.replies, w.answer(base, r, ev))
			return
		}
	}
	switch cmd.Body.(type) {
	case *bookingv1.Command_Select, *bookingv1.Command_Release:
		// No event (double click, unselect of a seat not in the cart): answer
		// with the current cart. It still goes out after the commit, because
		// it reflects earlier decisions of the batch.
		base.kind, base.version, base.cart = wire.KindCart, r.j.Version, cartSummary(r, user)
		w.replies = append(w.replies, base)
	}
}

// answer builds the reply for an event, with the cart or hold summary as it
// stands right after the decision.
func (w *worker) answer(base reply, r *resident, ev *bookingv1.Event) reply {
	base.kind, base.ev, base.version = inventory.Kind(ev), ev, ev.Version
	switch ev.Body.(type) {
	case *bookingv1.Event_SeatSelected, *bookingv1.Event_SelectRejected, *bookingv1.Event_SeatReleased:
		base.cart = cartSummary(r, base.userID)
	}
	if pnr := inventory.PNROf(ev); pnr != "" {
		base.hold = holdSummary(r.j, pnr, "")
	}
	return base
}

// unknownJourney is the rejection for a command whose journey cannot be loaded.
func unknownJourney(cmd *bookingv1.Command, now time.Time) *bookingv1.Event {
	ev := &bookingv1.Event{EventId: cmd.RequestId, JourneyId: cmd.JourneyId, At: timestamppb.New(now), RequestId: cmd.RequestId}
	switch b := cmd.Body.(type) {
	case *bookingv1.Command_Select:
		ev.Body = &bookingv1.Event_SelectRejected{SelectRejected: &bookingv1.SelectRejected{UserId: b.Select.UserId, SeatId: b.Select.SeatId, Code: "NOT_FOUND"}}
	case *bookingv1.Command_Hold:
		ev.Body = &bookingv1.Event_HoldRejected{HoldRejected: &bookingv1.HoldRejected{Pnr: b.Hold.Pnr, UserId: b.Hold.UserId, Code: "NOT_FOUND"}}
	case *bookingv1.Command_Cancel:
		ev.Body = &bookingv1.Event_CancelRejected{CancelRejected: &bookingv1.CancelRejected{Pnr: b.Cancel.Pnr, UserId: b.Cancel.UserId, Code: "NOT_FOUND"}}
	case *bookingv1.Command_Release:
		ev.Body = &bookingv1.Event_SeatReleased{SeatReleased: &bookingv1.SeatReleased{UserId: b.Release.UserId}}
	default:
		return nil
	}
	return ev
}
