// Package kafka holds topic names, the journey partitioner shared by every
// producer, and client constructors.
package kafka

import (
	"context"
	"strconv"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

const (
	TopicBookingRequests = "booking-requests"
	TopicInventoryEvents = "inventory-events"
	TopicPaymentEvents   = "payment-events"
	TopicNotifications   = "notifications"
)

// JourneyKey is the record key for booking-requests and inventory-events.
func JourneyKey(journeyID int64) []byte { return []byte(strconv.FormatInt(journeyID, 10)) }

// PartitionFor returns the partition a journey's records land on. It matches the
// Kafka default partitioner (murmur2 of the key) that every producer uses via
// kgo.StickyKeyPartitioner; the API uses it to read lag:{partition}.
func PartitionFor(journeyID int64, partitions int) int32 {
	h := murmur2(JourneyKey(journeyID))
	return int32((h & 0x7fffffff) % int32(partitions))
}

func murmur2(data []byte) int32 {
	const (
		seed uint32 = 0x9747b28c
		m    uint32 = 0x5bd1e995
		r           = 24
	)
	length := len(data)
	h := seed ^ uint32(length)
	for i := 0; i+4 <= length; i += 4 {
		k := uint32(data[i]) | uint32(data[i+1])<<8 | uint32(data[i+2])<<16 | uint32(data[i+3])<<24
		k *= m
		k ^= k >> r
		k *= m
		h *= m
		h ^= k
	}
	tail := length &^ 3
	switch length % 4 {
	case 3:
		h ^= uint32(data[tail+2]) << 16
		fallthrough
	case 2:
		h ^= uint32(data[tail+1]) << 8
		fallthrough
	case 1:
		h ^= uint32(data[tail])
		h *= m
	}
	h ^= h >> 13
	h *= m
	h ^= h >> 15
	return int32(h)
}

// NewProducer returns an idempotent producer (acks=all) using the journey partitioner.
func NewProducer(brokers []string, extra ...kgo.Opt) (*kgo.Client, error) {
	opts := append([]kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)),
		kgo.ProducerLinger(time.Millisecond),
		kgo.ProduceRequestTimeout(5 * time.Second),
	}, extra...)
	return kgo.NewClient(opts...)
}

// Ping checks that at least one broker answers.
func Ping(ctx context.Context, cl *kgo.Client) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return cl.Ping(ctx)
}
