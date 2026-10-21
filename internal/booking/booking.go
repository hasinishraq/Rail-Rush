// Package booking is the API side of the write path: the command producer, the
// per-pod result waiter (Redis Pub/Sub), the Redis-backed result store and
// inventory view, and PNR generation.
package booking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/bits"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	bookingv1 "railway/gen/booking/v1"
	"railway/internal/platform/kafka"
	"railway/internal/wire"
)

// ErrLogUnavailable means the command could not be made durable (fail closed).
var ErrLogUnavailable = errors.New("log unavailable")

// Producer sends commands to booking-requests: key = journey_id, acks=all, 1 s timeout.
type Producer struct {
	cl      *kgo.Client
	timeout time.Duration
}

func NewProducer(cl *kgo.Client) *Producer { return &Producer{cl: cl, timeout: time.Second} }

func (p *Producer) Produce(ctx context.Context, cmd *bookingv1.Command) error {
	if cmd.SentAt == nil {
		cmd.SentAt = timestamppb.Now()
	}
	b, err := proto.Marshal(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	rec := &kgo.Record{Topic: kafka.TopicBookingRequests, Key: kafka.JourneyKey(cmd.JourneyId), Value: b}
	if err := p.cl.ProduceSync(ctx, rec).FirstErr(); err != nil {
		return fmt.Errorf("%w: %v", ErrLogUnavailable, err)
	}
	return nil
}

// Waiter delivers results published on results:{podId} to the request waiting for them.
type Waiter struct {
	mu         sync.Mutex
	pending    map[string]chan wire.Result
	subscribed atomic.Bool
}

func NewWaiter() *Waiter { return &Waiter{pending: map[string]chan wire.Result{}} }

// Register must be called BEFORE producing the command.
func (w *Waiter) Register(requestID string) <-chan wire.Result {
	ch := make(chan wire.Result, 1)
	w.mu.Lock()
	w.pending[requestID] = ch
	w.mu.Unlock()
	return ch
}

func (w *Waiter) Forget(requestID string) {
	w.mu.Lock()
	delete(w.pending, requestID)
	w.mu.Unlock()
}

func (w *Waiter) Ready() bool { return w.subscribed.Load() }

// Run is the pod's single subscriber goroutine.
func (w *Waiter) Run(ctx context.Context, rdb redis.UniversalClient, podID string, log *slog.Logger) {
	sub := rdb.Subscribe(ctx, wire.ResultsChannel(podID))
	defer sub.Close()
	if _, err := sub.Receive(ctx); err == nil {
		w.subscribed.Store(true)
	}
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			w.subscribed.Store(false)
			return
		case msg, ok := <-ch:
			if !ok {
				w.subscribed.Store(false)
				return
			}
			w.subscribed.Store(true)
			var r wire.Result
			if json.Unmarshal([]byte(msg.Payload), &r) != nil {
				log.Warn("undecodable result", "payload", msg.Payload)
				continue
			}
			w.mu.Lock()
			if c, ok := w.pending[r.RequestID]; ok {
				c <- r
				delete(w.pending, r.RequestID)
			}
			w.mu.Unlock()
		}
	}
}

// Store reads what the allocator writes to Redis.
type Store struct{ rdb redis.UniversalClient }

func NewStore(rdb redis.UniversalClient) *Store { return &Store{rdb: rdb} }

func (s *Store) Result(ctx context.Context, requestID string) (wire.Result, bool, error) {
	var r wire.Result
	b, err := s.rdb.Get(ctx, wire.ResultKey(requestID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	return r, true, json.Unmarshal(b, &r)
}

func (s *Store) SeatMap(ctx context.Context, journeyID int64) ([]byte, error) {
	b, err := s.rdb.Get(ctx, wire.SeatMapKey(journeyID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return b, err
}

// SeatMaps reads several seat maps with one MGET; missing ones are nil.
func (s *Store) SeatMaps(ctx context.Context, ids []int64) ([]*wire.SeatMap, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = wire.SeatMapKey(id)
	}
	vals, err := s.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*wire.SeatMap, len(ids))
	for i, v := range vals {
		str, ok := v.(string)
		if !ok {
			continue
		}
		var sm wire.SeatMap
		if json.Unmarshal([]byte(str), &sm) == nil {
			out[i] = &sm
		}
	}
	return out, nil
}

// SegFree returns free berths per segment for a pool, ok=false if unknown.
func (s *Store) SegFree(ctx context.Context, journeyID int64, class, quota string) ([]int, bool, error) {
	b, err := s.rdb.Get(ctx, wire.SegFreeKey(journeyID, class, quota)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var v []int
	return v, true, json.Unmarshal(b, &v)
}

// Lag is the allocator lag of a partition.
func (s *Store) Lag(ctx context.Context, partition int32) (time.Duration, error) {
	v, err := s.rdb.Get(ctx, wire.LagKey(partition)).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	ms, _ := strconv.ParseInt(v, 10, 64)
	return time.Duration(ms) * time.Millisecond, nil
}

// Cart reads cart:{journey}:{user} (written by the allocator after commit).
func (s *Store) Cart(ctx context.Context, journeyID, userID int64) (*wire.CartSummary, bool, error) {
	b, err := s.rdb.Get(ctx, wire.CartKey(journeyID, userID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var c wire.CartSummary
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, false, err
	}
	return &c, len(c.Seats) > 0, nil
}

// ULimit is the number of seats the user was issued for a travel day
// (ulimit:{user}:{run_date}, mirrored from purchase_counter by the projector).
func (s *Store) ULimit(ctx context.Context, userID int64, runDate string) (int, error) {
	n, err := s.rdb.Get(ctx, wire.ULimitKey(userID, runDate)).Int()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return n, err
}

func (s *Store) Hold(ctx context.Context, pnr string) (*wire.HoldSummary, bool, error) {
	b, err := s.rdb.Get(ctx, wire.HoldKey(pnr)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var h wire.HoldSummary
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, false, err
	}
	return &h, true, nil
}

// MarkPending records who sent a request, so polling can answer PENDING vs 404.
func (s *Store) MarkPending(ctx context.Context, requestID string, userID int64) error {
	return s.rdb.Set(ctx, wire.RequestKey(requestID), userID, wire.ResultTTL).Err()
}

// PendingOwner returns the sender of a request still waiting for its result.
func (s *Store) PendingOwner(ctx context.Context, requestID string) (int64, bool, error) {
	v, err := s.rdb.Get(ctx, wire.RequestKey(requestID)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	return v, err == nil, err
}

// PNRs are 10 digits. A Redis counter is passed through a bijection on
// [0, 9e9) (multiplication by a constant coprime with 9e9), so they are unique
// across pods without looking sequential.
const (
	pnrSpace = 9_000_000_000
	pnrMul   = 7_919_000_003
	pnrAdd   = 1_234_567_891
)

type PNRGen struct{ rdb redis.UniversalClient }

func NewPNRGen(rdb redis.UniversalClient) *PNRGen { return &PNRGen{rdb: rdb} }

func (g *PNRGen) Next(ctx context.Context) (string, error) {
	n, err := g.rdb.Incr(ctx, "pnr:seq").Uint64()
	if err != nil {
		return "", err
	}
	hi, lo := bits.Mul64(n%pnrSpace, pnrMul)
	v := (bits.Rem64(hi, lo, pnrSpace) + pnrAdd) % pnrSpace
	return strconv.FormatUint(v+1_000_000_000, 10), nil
}
