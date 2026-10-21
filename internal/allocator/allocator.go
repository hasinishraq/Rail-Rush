// Package allocator runs the stateful inventory owner (§6): one worker per
// owned partition of booking-requests, decisions committed to inventory-events
// in one Kafka transaction together with the consumer offsets, then results,
// cart and hold summaries, seat deltas, seat maps and snapshots published.
package allocator

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	bookingv1 "railway/gen/booking/v1"
	"railway/internal/catalog"
	"railway/internal/inventory"
	"railway/internal/platform"
	"railway/internal/platform/config"
	"railway/internal/platform/kafka"
	"railway/internal/platform/metrics"
)

type partOp struct {
	part int32
	add  bool
}

type Allocator struct {
	cfg    config.Config
	log    *slog.Logger
	cat    *catalog.Catalog
	db     *pgxpool.Pool
	rdb    redis.UniversalClient
	clock  platform.Clock
	loader *Loader

	sess *kgo.GroupTransactSession

	opsMu sync.Mutex
	ops   []partOp

	workers       map[int32]*worker
	pendingReload map[int32][]int64 // journeys to reload when a partition comes back
	failingSince  time.Time
	backoff       time.Duration
	snapSem       chan struct{}
	lastLagPub    time.Time
	lastEvict     time.Time

	panicMu sync.Mutex
	panics  map[string]int

	ready atomic.Bool
}

func New(cfg config.Config, log *slog.Logger, cat *catalog.Catalog, db *pgxpool.Pool, rdb redis.UniversalClient) *Allocator {
	return &Allocator{
		cfg: cfg, log: log, cat: cat, db: db, rdb: rdb, clock: platform.SystemClock{},
		loader:        NewLoader(cfg, cat, db),
		workers:       map[int32]*worker{},
		pendingReload: map[int32][]int64{},
		snapSem:       make(chan struct{}, 8),
		panics:        map[string]int{},
	}
}

// Ready reports whether the transactional session is up.
func (a *Allocator) Ready() bool { return a.ready.Load() }

func (a *Allocator) newSession() (*kgo.GroupTransactSession, error) {
	return kgo.NewGroupTransactSession(
		kgo.SeedBrokers(a.cfg.KafkaBrokers...),
		kgo.ConsumerGroup("allocator"),
		kgo.ConsumeTopics(kafka.TopicBookingRequests),
		kgo.TransactionalID("allocator-"+a.cfg.PodName),
		kgo.InstanceID(a.cfg.PodName), // static membership
		kgo.FetchIsolationLevel(kgo.ReadCommitted()),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)),
		kgo.FetchMaxWait(10*time.Millisecond), // the spec's 5 ms is below franz-go's 10 ms floor
		kgo.SessionTimeout(10*time.Second),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.OnPartitionsAssigned(a.onAssigned), // create workers, lazy-load journeys
		kgo.OnPartitionsRevoked(a.onRevoked),   // drop state for lost partitions
		kgo.OnPartitionsLost(a.onRevoked),
	)
}

func (a *Allocator) onAssigned(_ context.Context, _ *kgo.Client, m map[string][]int32) {
	a.opsMu.Lock()
	for _, p := range m[kafka.TopicBookingRequests] {
		a.ops = append(a.ops, partOp{p, true})
	}
	a.opsMu.Unlock()
	a.log.Info("partitions assigned", "partitions", m[kafka.TopicBookingRequests])
}

func (a *Allocator) onRevoked(_ context.Context, _ *kgo.Client, m map[string][]int32) {
	a.opsMu.Lock()
	for _, p := range m[kafka.TopicBookingRequests] {
		a.ops = append(a.ops, partOp{p, false})
	}
	a.opsMu.Unlock()
	a.log.Info("partitions revoked", "partitions", m[kafka.TopicBookingRequests])
}

func (a *Allocator) applyPartitionOps() {
	a.opsMu.Lock()
	ops := a.ops
	a.ops = nil
	a.opsMu.Unlock()
	for _, op := range ops {
		if op.add {
			if a.workers[op.part] == nil {
				w := newWorker(a, op.part)
				w.reload, a.pendingReload[op.part] = a.pendingReload[op.part], nil
				a.workers[op.part] = w
			}
		} else {
			delete(a.workers, op.part)
		}
	}
	if len(ops) > 0 {
		a.updateResidentGauge()
	}
}

func (a *Allocator) notePanic(requestID string) {
	a.panicMu.Lock()
	a.panics[requestID]++
	a.panicMu.Unlock()
}

func (a *Allocator) poisoned(requestID string) bool {
	a.panicMu.Lock()
	defer a.panicMu.Unlock()
	return a.panics[requestID] >= 3
}

// Run is the processing loop: poll, decide in parallel per partition, commit
// events and offsets in one transaction, then publish.
func (a *Allocator) Run(ctx context.Context) error {
	var err error
	if a.sess, err = a.newSession(); err != nil {
		return err
	}
	defer func() { a.sess.Close() }()
	a.ready.Store(true)

	for ctx.Err() == nil {
		a.applyPartitionOps()

		pctx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
		fetches := a.sess.PollRecords(pctx, 5000) // short wait so timers and deltas keep running
		cancel()
		if ctx.Err() != nil {
			break
		}
		fetches.EachError(func(t string, p int32, err error) {
			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				a.log.Warn("fetch error", "topic", t, "partition", p, "err", err)
			}
		})
		a.applyPartitionOps()

		now := a.clock.Now()
		byPart := map[int32][]*kgo.Record{}
		nrec := 0
		fetches.EachRecord(func(r *kgo.Record) {
			byPart[r.Partition] = append(byPart[r.Partition], r)
			nrec++
		})
		extend := time.Duration(0)
		if !a.failingSince.IsZero() && now.Sub(a.failingSince) > a.cfg.OutageExtendAfter {
			extend = now.Sub(a.failingSince)
		}

		var wg sync.WaitGroup
		for p, w := range a.workers { // also runs idle partitions: timers
			wg.Add(1)
			go func(w *worker, recs []*kgo.Record) {
				defer wg.Done()
				w.process(ctx, recs, now, extend)
			}(w, byPart[p])
		}
		wg.Wait()

		var out []*bookingv1.Event
		var outRes []*resident
		abort := false
		for _, w := range a.workers {
			out = append(out, w.out...)
			outRes = append(outRes, w.outRes...)
			abort = abort || w.abort
			a.publishEphemeral(ctx, w)
		}
		if len(out) == 0 && nrec == 0 && !abort {
			a.publishDeltas(ctx, now, false)
			a.publishSeatMaps(ctx, now)
			a.housekeeping(ctx, now)
			continue
		}

		if err := a.sess.Begin(); err != nil {
			return err
		}
		for i, ev := range out {
			b, err := proto.Marshal(ev)
			if err != nil {
				return err
			}
			res := outRes[i]
			rec := &kgo.Record{Topic: kafka.TopicInventoryEvents, Key: kafka.JourneyKey(ev.JourneyId), Value: b}
			a.sess.Produce(ctx, rec, func(r *kgo.Record, err error) {
				if err == nil {
					for {
						cur := res.pendingOffset.Load()
						if r.Offset <= cur || res.pendingOffset.CompareAndSwap(cur, r.Offset) {
							break
						}
					}
				}
			})
		}
		start := time.Now()
		try := kgo.TryCommit
		if abort {
			try = kgo.TryAbort
		}
		committed, err := a.sess.End(ctx, try) // events + offsets, atomically
		metrics.AllocatorCommitSeconds.Observe(time.Since(start).Seconds())
		if err != nil || !committed {
			if !abort {
				metrics.AllocatorCommitFailures.Inc()
				a.log.Warn("commit failed; dropping resident state", "err", err, "events", len(out), "records", nrec)
				if a.failingSince.IsZero() {
					a.failingSince = now
				}
			}
			a.dropResidentState() // RAM may be ahead of the log
			if err != nil {
				// No End error is retryable on this session: start a new one.
				// The new producer epoch fences anything the old one left open.
				a.sess.Close()
				a.ready.Store(false)
				a.waitBackoff(ctx)
				if a.sess, err = a.newSession(); err != nil {
					return err
				}
				a.ready.Store(true)
				for p, w := range a.workers {
					a.pendingReload[p] = append(a.pendingReload[p], w.reload...)
				}
				a.workers = map[int32]*worker{}
				continue
			}
			a.waitBackoff(ctx)
			continue
		}
		a.backoff = 0
		a.failingSince = time.Time{}
		for _, w := range a.workers {
			for r := range w.touched {
				r.lastOffset = r.pendingOffset.Load()
			}
		}
		for _, ev := range out {
			metrics.AllocatorEvents.WithLabelValues(inventory.Kind(ev)).Inc()
		}
		a.afterCommit(ctx, now)
		a.housekeeping(ctx, now)
	}
	return ctx.Err()
}

func (a *Allocator) waitBackoff(ctx context.Context) {
	switch {
	case a.backoff == 0:
		a.backoff = 100 * time.Millisecond
	case a.backoff < 5*time.Second:
		a.backoff *= 2
	}
	if a.backoff > 5*time.Second {
		a.backoff = 5 * time.Second
	}
	select {
	case <-ctx.Done():
	case <-time.After(a.backoff):
	}
}

// dropResidentState forgets every journey after a failed commit; they reload
// from the committed log, eagerly on the next iteration.
func (a *Allocator) dropResidentState() {
	for _, w := range a.workers {
		for id := range w.journeys {
			w.reload = append(w.reload, id)
		}
		w.journeys = map[int64]*resident{}
		w.resetOutput()
	}
	a.updateResidentGauge()
}

func (a *Allocator) updateResidentGauge() {
	n := 0
	for _, w := range a.workers {
		n += len(w.journeys)
	}
	metrics.AllocatorResident.Set(float64(n))
}

// housekeeping: partition lag every second; idle eviction and overdue snapshots every 30 s.
func (a *Allocator) housekeeping(ctx context.Context, now time.Time) {
	if now.Sub(a.lastLagPub) >= time.Second {
		a.lastLagPub = now
		pipe := a.rdb.Pipeline()
		for p, w := range a.workers {
			ms := w.lag.Milliseconds()
			pipe.Set(ctx, lagKey(p), ms, 10*time.Second)
			metrics.AllocatorLagMS.WithLabelValues(strconv.Itoa(int(p))).Set(float64(ms))
		}
		if _, err := pipe.Exec(ctx); err != nil {
			a.log.Debug("lag publish failed", "err", err)
		}
	}
	if now.Sub(a.lastEvict) < 30*time.Second {
		return
	}
	a.lastEvict = now
	for _, w := range a.workers {
		for id, r := range w.journeys {
			if r.eventsSinceSnap > 0 && now.Sub(r.lastSnap) >= a.cfg.SnapshotInterval {
				a.snapshot(ctx, r, now, false)
			}
			idle := now.Sub(r.lastUsed) > a.cfg.IdleEvictAfter
			departed := now.After(r.cj.DepartsAt.Add(6 * time.Hour))
			if (idle && len(r.j.Holds) == 0 && len(r.j.Carts) == 0) || departed {
				if r.eventsSinceSnap > 0 {
					a.snapshot(ctx, r, now, true)
				}
				delete(w.journeys, id)
				w.log.Info("journey evicted", "journey", id)
			}
		}
	}
	a.updateResidentGauge()
}
