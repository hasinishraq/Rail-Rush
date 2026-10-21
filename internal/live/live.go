// Package live is the SSE fan-out of seat deltas to admitted users (§11.7, W4).
// Each pod holds one Redis subscription to smd:{journey} per hot journey and
// writes every delta to all local connections; a slow client whose buffer is
// full is disconnected and resumes from smlog:{journey} on reconnect.
package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// ErrFull means the pod is at LIVE_MAX_CONNS; the client tries another pod.
var ErrFull = errors.New("live: too many connections")

// Sub is one client connection's view of a journey's delta stream.
type Sub struct {
	journeyID int64
	ch        chan wire.SeatDelta
	done      chan struct{}
	once      sync.Once
	slow      atomic.Bool
	reconnect atomic.Bool
}

// C delivers deltas; Done is closed when the hub drops the connection.
func (s *Sub) C() <-chan wire.SeatDelta { return s.ch }
func (s *Sub) Done() <-chan struct{}    { return s.done }

// Slow reports a disconnect for a full buffer; Reconnect one for pod shutdown.
func (s *Sub) Slow() bool      { return s.slow.Load() }
func (s *Sub) Reconnect() bool { return s.reconnect.Load() }

func (s *Sub) close() { s.once.Do(func() { close(s.done) }) }

type topic struct{ subs map[*Sub]struct{} }

type Hub struct {
	rdb      redis.UniversalClient
	log      *slog.Logger
	maxConns int

	mu     sync.Mutex
	topics map[int64]*topic
	ps     *redis.PubSub
	conns  atomic.Int64
	closed bool
}

func NewHub(rdb redis.UniversalClient, log *slog.Logger, maxConns int) *Hub {
	return &Hub{rdb: rdb, log: log, maxConns: maxConns, topics: map[int64]*topic{}}
}

// Full reports whether the pod should report not-ready.
func (h *Hub) Full() bool { return h.maxConns > 0 && h.conns.Load() >= int64(h.maxConns) }

// Attach registers a connection for a journey; the subscription to
// smd:{journey} is created by the first connection on this pod.
func (h *Hub) Attach(ctx context.Context, journeyID int64) (*Sub, error) {
	h.mu.Lock()
	if h.closed || h.ps == nil {
		h.mu.Unlock()
		return nil, errors.New("live: hub not running")
	}
	if h.Full() {
		h.mu.Unlock()
		return nil, ErrFull
	}
	s := &Sub{journeyID: journeyID, ch: make(chan wire.SeatDelta, 64), done: make(chan struct{})}
	t := h.topics[journeyID]
	first := t == nil
	if first {
		t = &topic{subs: map[*Sub]struct{}{}}
		h.topics[journeyID] = t
	}
	t.subs[s] = struct{}{}
	ps := h.ps
	h.mu.Unlock()
	h.conns.Add(1)
	metrics.LiveConnections.Set(float64(h.conns.Load()))
	if first {
		if err := ps.Subscribe(ctx, wire.DeltaChannel(journeyID)); err != nil {
			h.Detach(s)
			return nil, err
		}
	}
	return s, nil
}

// Detach removes a connection; the last one on a journey unsubscribes.
func (h *Hub) Detach(s *Sub) {
	s.close()
	h.mu.Lock()
	t := h.topics[s.journeyID]
	last := false
	if t != nil {
		if _, ok := t.subs[s]; ok {
			delete(t.subs, s)
			h.conns.Add(-1)
		}
		if len(t.subs) == 0 {
			delete(h.topics, s.journeyID)
			last = true
		}
	}
	ps := h.ps
	h.mu.Unlock()
	metrics.LiveConnections.Set(float64(h.conns.Load()))
	if last && ps != nil {
		_ = ps.Unsubscribe(context.Background(), wire.DeltaChannel(s.journeyID))
	}
}

func (h *Hub) fanout(id int64, d wire.SeatDelta) {
	h.mu.Lock()
	t := h.topics[id]
	var slow []*Sub
	if t != nil {
		for s := range t.subs {
			select {
			case s.ch <- d:
			default:
				slow = append(slow, s)
			}
		}
	}
	h.mu.Unlock()
	for _, s := range slow {
		s.slow.Store(true)
		metrics.LiveSlowDisconnects.Inc()
		h.Detach(s)
	}
}

// Run owns the pod's subscription.
func (h *Hub) Run(ctx context.Context) {
	ps := h.rdb.Subscribe(ctx)
	h.mu.Lock()
	h.ps = ps
	h.mu.Unlock()
	defer ps.Close()
	ch := ps.Channel(redis.WithChannelSize(4096))
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
				h.fanout(id, d)
			}
		}
	}
}

// Shutdown tells every client to reconnect (to another pod) before the pod drains.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	h.closed = true
	var all []*Sub
	for _, t := range h.topics {
		for s := range t.subs {
			all = append(all, s)
		}
	}
	h.mu.Unlock()
	for _, s := range all {
		s.reconnect.Store(true)
		s.close()
	}
}

// Missed reads the deltas after version from from smlog:{journey}. resync is
// true when the client's version is older than the stream: it must refetch the
// snapshot.
func (h *Hub) Missed(ctx context.Context, journeyID, from int64) (ds []wire.SeatDelta, resync bool, err error) {
	msgs, err := h.rdb.XRangeN(ctx, wire.DeltaLogKey(journeyID), fmt.Sprintf("(%d-0", from), "+", 2000).Result()
	if err != nil {
		return nil, false, err
	}
	for i, m := range msgs {
		raw, _ := m.Values["d"].(string)
		var d wire.SeatDelta
		if json.Unmarshal([]byte(raw), &d) != nil {
			continue
		}
		if i == 0 && d.FromV > from {
			return nil, true, nil // the gap fell out of smlog
		}
		ds = append(ds, d)
	}
	return ds, false, nil
}

// ClaimConnection makes this connection the only live stream of the user on
// the journey: a new one closes the old one.
func (h *Hub) ClaimConnection(ctx context.Context, userID, journeyID int64, id string, ttl time.Duration) {
	h.rdb.Set(ctx, wire.LiveConnKey(userID, journeyID), id, ttl)
}

// Owns reports whether connection id still holds the claim.
func (h *Hub) Owns(ctx context.Context, userID, journeyID int64, id string) bool {
	v, err := h.rdb.Get(ctx, wire.LiveConnKey(userID, journeyID)).Result()
	return err != nil || v == id // Redis trouble: keep the stream
}
