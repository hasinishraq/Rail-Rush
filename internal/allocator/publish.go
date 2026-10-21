package allocator

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"railway/internal/inventory"
	"railway/internal/platform/kafka"
	"railway/internal/wire"
)

var pj = protojson.MarshalOptions{UseProtoNames: true}

func lagKey(p int32) string { return wire.LagKey(p) }

func ttlUntil(departs, now time.Time) time.Duration {
	if d := departs.Add(24 * time.Hour).Sub(now); d > time.Hour {
		return d
	}
	return time.Hour
}

// cartSummary builds cart:{journey}:{user}; an empty cart has no seats.
func cartSummary(r *resident, user int64) *wire.CartSummary {
	j := r.j
	s := &wire.CartSummary{JourneyID: j.ID, UserID: user, Seats: []wire.SeatRef{}, Version: j.Version}
	c := j.Carts[user]
	if c == nil {
		return s
	}
	p := j.Pools[c.Pool]
	s.FromIdx, s.ToIdx, s.ClassCode, s.Quota, s.ExpiresAt = c.From, c.To, p.Key.Class, p.Key.Quota, c.ExpiresAt
	s.MaxSeats = max(p.MaxSeats-j.UserSeats[user], 0)
	for _, idx := range c.Seats {
		s.Seats = append(s.Seats, wire.SeatRef{SeatIdx: idx, SeatID: j.SeatIDs[idx]})
	}
	return s
}

// holdSummary builds hold:{pnr} from the journey's current state.
func holdSummary(j *inventory.Journey, pnr, override string) *wire.HoldSummary {
	h, state := j.Lookup(pnr)
	if h == nil {
		return nil
	}
	p := j.Pools[h.Pool]
	s := &wire.HoldSummary{
		PNR: h.PNR, UserID: h.UserID, JourneyID: j.ID, ClassCode: p.Key.Class, Quota: p.Key.Quota,
		FromIdx: h.From, ToIdx: h.To, AmountMinor: h.AmountMinor, Currency: h.Currency,
		ExpiresAt: h.ExpiresAt, PaymentID: h.PaymentID, Late: h.Late, SeatsChanged: h.SeatsChanged,
		Status: inventory.HoldStatus(h, state), UpdatedAt: j.LastEvent,
	}
	if override != "" {
		s.Status = override
	}
	for i, idx := range h.Seats {
		ps := wire.PaxSummary{Seq: i + 1, SeatIdx: idx, SeatID: j.SeatIDs[idx]}
		switch {
		case state == "HELD":
			ps.Status = "HELD"
		case state == "EXPIRED":
			ps.Status = "EXPIRED"
		case h.Cancelled[i]:
			ps.Status = "CANCELLED"
		default:
			ps.Status = "ISSUED"
		}
		if i < len(h.Pax) {
			px := h.Pax[i]
			ps.Name, ps.Gender, ps.Type = px.Name, px.Gender.String(), px.Type.String()
			if ps.Type == "PASSENGER_TYPE_UNSPECIFIED" {
				ps.Type = "ADULT"
			}
		}
		s.Passengers = append(s.Passengers, ps)
	}
	return s
}

func (a *Allocator) result(rp reply) []byte {
	r := wire.Result{RequestID: rp.requestID, UserID: rp.userID, JourneyID: rp.journeyID, Kind: rp.kind,
		Version: rp.version, Cart: rp.cart, Hold: rp.hold}
	if rp.ev != nil {
		if body := inventory.Body(rp.ev); body != nil {
			if b, err := pj.Marshal(body); err == nil {
				r.Event = b
			}
		}
	}
	b, _ := json.Marshal(r)
	return b
}

func (a *Allocator) queueReply(ctx context.Context, pipe redis.Pipeliner, rp reply) {
	b := a.result(rp)
	pipe.Set(ctx, wire.ResultKey(rp.requestID), b, wire.ResultTTL)
	if rp.replyTo != "" {
		pipe.Publish(ctx, rp.replyTo, b)
	}
}

// publishEphemeral answers commands that changed nothing (unknown journey).
func (a *Allocator) publishEphemeral(ctx context.Context, w *worker) {
	if len(w.ephemeral) == 0 {
		return
	}
	pipe := a.rdb.Pipeline()
	for _, rp := range w.ephemeral {
		a.queueReply(ctx, pipe, rp)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		a.log.Warn("ephemeral publish failed", "err", err)
	}
	w.ephemeral = w.ephemeral[:0]
}

// afterCommit runs only after a successful commit, in this order: results
// (with cart:/hold: keys, so the API never depends on projection lag), seat
// deltas, seat maps and segfree, snapshots.
func (a *Allocator) afterCommit(ctx context.Context, now time.Time) {
	pipe := a.rdb.Pipeline()
	for _, w := range a.workers {
		for r, users := range w.carts {
			for u := range users {
				key := wire.CartKey(r.j.ID, u)
				if c := r.j.Carts[u]; c != nil {
					b, _ := json.Marshal(cartSummary(r, u))
					pipe.Set(ctx, key, b, max(c.ExpiresAt.Sub(now), 0)+5*time.Second)
				} else {
					pipe.Del(ctx, key)
				}
			}
		}
		for r, pnrs := range w.pnrs {
			for pnr := range pnrs {
				if s := holdSummary(r.j, pnr, ""); s != nil {
					b, _ := json.Marshal(s)
					pipe.Set(ctx, wire.HoldKey(pnr), b, wire.HoldTTL)
				}
			}
		}
		// ConfirmFailed leaves an expired PNR where it was; mark its summary.
		for i, ev := range w.out {
			if cf := ev.GetConfirmFailed(); cf != nil && w.outRes[i].j.Expired[cf.Pnr] != nil {
				if s := holdSummary(w.outRes[i].j, cf.Pnr, "CONFIRM_FAILED"); s != nil {
					b, _ := json.Marshal(s)
					pipe.Set(ctx, wire.HoldKey(cf.Pnr), b, wire.HoldTTL)
				}
			}
		}
		for _, rp := range w.replies {
			a.queueReply(ctx, pipe, rp)
		}
		for r := range w.touched { // segfree goes out with every commit, not on the ticker
			ttl := ttlUntil(r.cj.DepartsAt, now)
			for _, p := range r.j.Pools {
				fb, _ := json.Marshal(r.j.FreeOnSeg(p))
				pipe.Set(ctx, wire.SegFreeKey(r.j.ID, p.Key.Class, p.Key.Quota), fb, ttl)
			}
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		a.log.Warn("result publish failed", "err", err)
	}

	a.publishDeltas(ctx, now, false)
	a.publishSeatMaps(ctx, now)

	for _, w := range a.workers {
		counted := map[*resident]int{}
		for _, r := range w.outRes {
			counted[r]++
		}
		for r, n := range counted {
			r.eventsSinceSnap += n
			if r.eventsSinceSnap >= a.cfg.SnapshotEvery || now.Sub(r.lastSnap) >= a.cfg.SnapshotInterval {
				a.snapshot(ctx, r, now, false)
			}
		}
		w.resetOutput()
	}
}

// publishDeltas turns each dirty journey into one delta every LIVE_DELTA_INTERVAL
// (250 ms): XADD smlog:{journey} MAXLEN ~ 2000 and PUBLISH smd:{journey} in one
// pipeline. RAM equals the committed log here: the loop only calls it after a
// commit or when nothing was decided.
func (a *Allocator) publishDeltas(ctx context.Context, now time.Time, force bool) {
	pipe := a.rdb.Pipeline()
	n := 0
	for _, w := range a.workers {
		for _, r := range w.journeys {
			if len(r.dirty) == 0 || (!force && now.Sub(r.lastDelta) < a.cfg.LiveDeltaInterval) {
				continue
			}
			cs := make([]wire.Change, len(r.dirty))
			for i, idx := range r.dirty {
				cs[i] = wire.Change{SeatIdx: idx, Bits: r.j.Seats[idx]}
				delete(r.dirtyMark, idx)
			}
			d := wire.SeatDelta{FromV: r.deltaV, V: r.j.Version, Changes: wire.EncodeChanges(cs), At: now}
			b, _ := json.Marshal(d)
			ttl := ttlUntil(r.cj.DepartsAt, now)
			pipe.XAdd(ctx, &redis.XAddArgs{Stream: wire.DeltaLogKey(r.j.ID), MaxLen: int64(a.cfg.SmlogMaxLen), Approx: true,
				ID: fmt.Sprintf("%d-0", d.V), Values: []any{"d", b}})
			pipe.Expire(ctx, wire.DeltaLogKey(r.j.ID), ttl)
			pipe.Publish(ctx, wire.DeltaChannel(r.j.ID), b)
			r.dirty, r.deltaV, r.lastDelta = r.dirty[:0], r.j.Version, now
			n++
		}
	}
	if n == 0 {
		return
	}
	if _, err := pipe.Exec(ctx); err != nil {
		a.log.Warn("delta publish failed", "err", err) // clients detect the gap and resync
	}
}

// publishSeatMaps writes seatmap:{journey} at most once per interval per
// journey, and at once after Opened / SalesClosed / SoldOut / Available or a load.
func (a *Allocator) publishSeatMaps(ctx context.Context, now time.Time) {
	p := a.rdb.Pipeline()
	n := 0
	for _, w := range a.workers {
		for _, r := range w.journeys {
			if r.j.Version == r.mapVersion && !r.forceMap {
				continue
			}
			if !r.forceMap && now.Sub(r.lastMapPub) < a.cfg.SeatmapPublishInterval {
				continue
			}
			b, err := json.Marshal(buildSeatMap(r, now))
			if err != nil {
				continue
			}
			p.Set(ctx, wire.SeatMapKey(r.j.ID), b, ttlUntil(r.cj.DepartsAt, now))
			r.mapVersion, r.forceMap, r.lastMapPub = r.j.Version, false, now
			n++
		}
	}
	if n > 0 {
		if _, err := p.Exec(ctx); err != nil {
			a.log.Warn("seat map publish failed", "err", err)
		}
	}
}

func buildSeatMap(r *resident, now time.Time) wire.SeatMap {
	j := r.j
	sm := wire.SeatMap{
		JourneyID: j.ID, Version: j.Version, GeneratedAt: now, Segments: j.Segments,
		Encoding: "u64le-base64",
		Seats:    wire.EncodeBits(j.Seats),
		SeatPool: base64.StdEncoding.EncodeToString(j.PoolOf),
		SegFree:  map[string][]int{},
		LiveURL:  fmt.Sprintf("/v1/journeys/%d/seatmap/live", j.ID),
	}
	for _, p := range j.Pools {
		sm.Pools = append(sm.Pools, wire.SeatMapPool{ClassCode: p.Key.Class, Quota: p.Key.Quota, Open: p.Open && !j.Closed, MaxSeats: p.MaxSeats})
		sm.SegFree[p.Key.String()] = j.FreeOnSeg(p)
	}
	return sm
}

// snapshot writes journey_snapshot (state + last committed offset). The state
// is serialised here, in the loop; the write happens in the background unless sync.
func (a *Allocator) snapshot(_ context.Context, r *resident, now time.Time, sync bool) {
	if r.lastOffset < 0 {
		return
	}
	state, err := proto.Marshal(r.j.Snapshot())
	if err != nil {
		a.log.Error("snapshot marshal", "journey", r.j.ID, "err", err)
		return
	}
	id, off := r.j.ID, r.lastOffset
	part := kafka.PartitionFor(id, a.cfg.Partitions)
	r.eventsSinceSnap, r.lastSnap = 0, now
	write := func() {
		wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := a.db.Exec(wctx, `
			INSERT INTO journey_snapshot (journey_id, kafka_partition, kafka_offset, state, taken_at)
			VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (journey_id) DO UPDATE
			   SET kafka_partition = EXCLUDED.kafka_partition, kafka_offset = EXCLUDED.kafka_offset,
			       state = EXCLUDED.state, taken_at = EXCLUDED.taken_at
			 WHERE journey_snapshot.kafka_offset < EXCLUDED.kafka_offset`, id, part, off, state)
		if err != nil {
			a.log.Warn("snapshot write failed", "journey", id, "err", err)
		}
	}
	if sync {
		write()
		return
	}
	select {
	case a.snapSem <- struct{}{}:
		go func() { defer func() { <-a.snapSem }(); write() }()
	default:
		write() // too many in flight: do it inline rather than drop it
	}
}
