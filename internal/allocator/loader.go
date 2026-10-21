package allocator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	bookingv1 "railway/gen/booking/v1"
	"railway/internal/catalog"
	"railway/internal/inventory"
	"railway/internal/platform/config"
	"railway/internal/platform/kafka"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Loader rebuilds journeys: an empty journey from reference data, overlaid with
// the latest snapshot, then the tail of inventory-events replayed with Apply.
// The invariant checker uses it too.
type Loader struct {
	Cat     *catalog.Catalog
	DB      *pgxpool.Pool
	Brokers []string
	Parts   int
	Config  inventory.Config // TTLs; Price is set per journey
}

func NewLoader(cfg config.Config, cat *catalog.Catalog, db *pgxpool.Pool) *Loader {
	return &Loader{Cat: cat, DB: db, Brokers: cfg.KafkaBrokers, Parts: cfg.Partitions, Config: inventory.Config{
		SelectIdleTTL: cfg.SelectIdleTTL, SelectMaxTTL: cfg.SelectMaxTTL, HoldTTL: cfg.HoldTTL, LateGrace: cfg.LatePayGrace}}
}

// Loaded is a rebuilt journey plus where its log ends.
type Loaded struct {
	J          *inventory.Journey
	CJ         *catalog.Journey
	Partition  int32
	LastOffset int64 // offset of the last event applied, -1 if none
	Replayed   int
}

// Load rebuilds one journey. useSnapshot=false replays the whole log (invariants).
func (l *Loader) Load(ctx context.Context, journeyID int64, useSnapshot bool) (*Loaded, error) {
	def, cj, train, err := l.Cat.Definition(ctx, journeyID)
	if err != nil {
		return nil, fmt.Errorf("journey %d: %w", journeyID, err)
	}
	cfg := l.Config
	cfg.Price = l.Cat.PriceFunc(cj, train)
	j, err := inventory.New(def, cfg)
	if err != nil {
		return nil, err
	}
	part := kafka.PartitionFor(journeyID, l.Parts)
	start := kgo.NewOffset().AtStart()
	last := int64(-1)

	if useSnapshot {
		var sp int32
		var off int64
		var state []byte
		err := l.DB.QueryRow(ctx, `SELECT kafka_partition, kafka_offset, state FROM journey_snapshot WHERE journey_id = $1`, journeyID).
			Scan(&sp, &off, &state)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return nil, fmt.Errorf("read snapshot: %w", err)
		case sp == part:
			var snap bookingv1.JourneySnapshot
			if uerr := proto.Unmarshal(state, &snap); uerr == nil {
				if rerr := j.Restore(&snap); rerr == nil {
					start, last = kgo.NewOffset().At(off+1), off
				} else if j, err = inventory.New(def, cfg); err != nil { // layout changed: full replay
					return nil, err
				}
			}
		}
	}

	key := string(kafka.JourneyKey(journeyID))
	n := 0
	end, err := replayPartition(ctx, l.Brokers, kafka.TopicInventoryEvents, part, start, func(r *kgo.Record) error {
		if string(r.Key) != key {
			return nil
		}
		var ev bookingv1.Event
		if err := proto.Unmarshal(r.Value, &ev); err != nil {
			return fmt.Errorf("event at offset %d: %w", r.Offset, err)
		}
		j.Apply(&ev)
		last = r.Offset
		n++
		return nil
	})
	if err != nil {
		return nil, err
	}
	_ = end
	return &Loaded{J: j, CJ: cj, Partition: part, LastOffset: last, Replayed: n}, nil
}

// replayPartition reads one partition from start up to its last stable offset
// (read_committed) and calls fn for every committed data record. Control records
// are kept so the reader can see exactly when it reaches the stable end.
func replayPartition(ctx context.Context, brokers []string, topic string, part int32, start kgo.Offset, fn func(*kgo.Record) error) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	admin, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		return 0, err
	}
	listed, err := kadm.NewClient(admin).ListCommittedOffsets(ctx, topic)
	admin.Close()
	if err != nil {
		return 0, fmt.Errorf("list stable offsets: %w", err)
	}
	lo, ok := listed.Lookup(topic, part)
	if !ok {
		return 0, fmt.Errorf("topic %s partition %d not found", topic, part)
	}
	if lo.Err != nil {
		return 0, lo.Err
	}
	stable := lo.Offset
	if stable <= 0 {
		return stable, nil
	}
	if so := start.EpochOffset().Offset; so >= 0 && so >= stable {
		return stable, nil
	}

	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{topic: {part: start}}),
		kgo.FetchIsolationLevel(kgo.ReadCommitted()),
		kgo.KeepControlRecords(),
		kgo.FetchMaxWait(100*time.Millisecond),
	)
	if err != nil {
		return 0, err
	}
	defer cl.Close()
	for {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			return 0, fmt.Errorf("replay %s/%d stopped before offset %d: %w", topic, part, stable, ctx.Err())
		}
		var ferr error
		fetches.EachError(func(_ string, _ int32, err error) { ferr = err })
		if ferr != nil {
			return 0, ferr
		}
		done := false
		var cbErr error
		fetches.EachRecord(func(r *kgo.Record) {
			if done || cbErr != nil {
				return
			}
			if r.Offset >= stable {
				done = true
				return
			}
			if !r.Attrs.IsControl() {
				cbErr = fn(r)
			}
			if r.Offset+1 >= stable {
				done = true
			}
		})
		if cbErr != nil {
			return 0, cbErr
		}
		if done {
			return stable, nil
		}
	}
}
