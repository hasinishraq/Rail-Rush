// Package seatmirror keeps the seat bitmaps of hot journeys in API pod memory,
// fed by smd:{journey}, so a click on a seat that is already taken is refused
// in about 1 ms without touching Kafka (§6.4). It is conservative: it only says
// "taken" for a seat that was taken a moment ago; a seat shown free always
// goes to the allocator, which decides.
package seatmirror

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"railway/internal/platform/metrics"
	"railway/internal/wire"
)

type journey struct {
	mu       sync.RWMutex
	version  int64
	bits     []uint64
	ready    bool
	loading  bool
	lastUsed atomic.Int64 // unix nanoseconds
}

type Mirror struct {
	rdb redis.UniversalClient
	log *slog.Logger

	mu sync.Mutex
	js map[int64]*journey
	ps *redis.PubSub
}

func New(rdb redis.UniversalClient, log *slog.Logger) *Mirror {
	return &Mirror{rdb: rdb, log: log, js: map[int64]*journey{}}
}

// Taken reports whether seatIdx has any of mask's segments taken. known is
// false while the journey is not mirrored yet; the caller then asks the allocator.
func (m *Mirror) Taken(journeyID int64, seatIdx int32, mask uint64) (taken, known bool) {
	m.mu.Lock()
	j := m.js[journeyID]
	m.mu.Unlock()
	if j == nil {
		m.Watch(context.Background(), journeyID)
		return false, false
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	j.lastUsed.Store(time.Now().UnixNano())
	if !j.ready || int(seatIdx) >= len(j.bits) || seatIdx < 0 {
		return false, false
	}
	return j.bits[seatIdx]&mask != 0, true
}

// Watch subscribes smd:{journey} on first use and loads the snapshot.
func (m *Mirror) Watch(ctx context.Context, journeyID int64) {
	m.mu.Lock()
	if m.js[journeyID] != nil || m.ps == nil {
		m.mu.Unlock()
		return
	}
	j := &journey{}
	j.lastUsed.Store(time.Now().UnixNano())
	m.js[journeyID] = j
	ps := m.ps
	metrics.MirrorJourneys.Set(float64(len(m.js)))
	m.mu.Unlock()
	go func() {
		// Subscribe before reading the snapshot, so no delta falls in between.
		if err := ps.Subscribe(context.Background(), wire.DeltaChannel(journeyID)); err != nil {
			m.log.Debug("seat mirror subscribe failed", "journey", journeyID, "err", err)
		}
		m.reload(journeyID, j)
	}()
}

func (m *Mirror) reload(id int64, j *journey) {
	j.mu.Lock()
	if j.loading {
		j.mu.Unlock()
		return
	}
	j.loading = true
	j.mu.Unlock()
	defer func() { j.mu.Lock(); j.loading = false; j.mu.Unlock() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b, err := m.rdb.Get(ctx, wire.SeatMapKey(id)).Bytes()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			m.log.Debug("seat mirror load failed", "journey", id, "err", err)
		}
		return
	}
	var sm wire.SeatMap
	if json.Unmarshal(b, &sm) != nil {
		return
	}
	bits, err := sm.Bits()
	if err != nil {
		return
	}
	j.mu.Lock()
	if !j.ready || sm.Version >= j.version {
		j.bits, j.version, j.ready = bits, sm.Version, true
	}
	j.mu.Unlock()
}

// apply follows the delta rule: a delta covers (from_v, v]; it applies to a
// mirror at version h when from_v <= h < v, because it carries the final bits
// of every seat changed in that range. A gap (from_v > h) reloads the snapshot.
func (m *Mirror) apply(id int64, d wire.SeatDelta) {
	m.mu.Lock()
	j := m.js[id]
	m.mu.Unlock()
	if j == nil {
		return
	}
	j.mu.Lock()
	switch {
	case !j.ready:
		j.mu.Unlock()
		return
	case j.version >= d.V:
		j.mu.Unlock()
		return
	case d.FromV > j.version:
		j.mu.Unlock()
		go m.reload(id, j)
		return
	}
	cs, err := wire.DecodeChanges(d.Changes)
	if err == nil {
		for _, c := range cs {
			if int(c.SeatIdx) < len(j.bits) {
				j.bits[c.SeatIdx] = c.Bits
			}
		}
		j.version = d.V
	}
	j.mu.Unlock()
}

// Run owns the pod's subscription and evicts journeys idle for 10 minutes.
func (m *Mirror) Run(ctx context.Context) {
	ps := m.rdb.Subscribe(ctx)
	m.mu.Lock()
	m.ps = ps
	m.mu.Unlock()
	defer ps.Close()
	ch := ps.Channel(redis.WithChannelSize(1024))
	evict := time.NewTicker(time.Minute)
	defer evict.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			id, err := strconv.ParseInt(strings.TrimPrefix(msg.Channel, "smd:"), 10, 64)
			if err != nil {
				continue
			}
			var d wire.SeatDelta
			if json.Unmarshal([]byte(msg.Payload), &d) == nil {
				m.apply(id, d)
			}
		case <-evict.C:
			var drop []string
			m.mu.Lock()
			for id, j := range m.js {
				if time.Since(time.Unix(0, j.lastUsed.Load())) > 10*time.Minute {
					delete(m.js, id)
					drop = append(drop, wire.DeltaChannel(id))
				}
			}
			metrics.MirrorJourneys.Set(float64(len(m.js)))
			m.mu.Unlock()
			if len(drop) > 0 {
				_ = ps.Unsubscribe(ctx, drop...)
			}
		}
	}
}
