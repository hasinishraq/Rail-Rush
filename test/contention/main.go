// Command contention is spec test 2 ("same-seat race"): many users click the
// same few seats at once. Select commands go straight into booking-requests
// (bypassing the API's limits and seat mirror, so every click reaches the
// allocator), then the answers are read from result:{id}. It checks that each
// seat-segment went to at most one user and that every other click got
// SEAT_TAKEN, then releases the winners' carts. Run the invariant check after:
//
//	go run ./test/contention --journey 78720261102 --n 5000 --seats 50
//	curl -H "X-Internal-Token: $INTERNAL_TOKEN" localhost:9091/internal/v1/journeys/78720261102/invariants
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	bookingv1 "railway/gen/booking/v1"
	"railway/internal/platform/kafka"
	"railway/internal/wire"
)

func main() {
	brokers := flag.String("brokers", "localhost:19092", "kafka brokers")
	redisAddr := flag.String("redis", "localhost:6379", "redis")
	n := flag.Int("n", 5000, "number of users (one click each)")
	journey := flag.Int64("journey", 0, "journey id (required); its class must be open")
	class := flag.String("class", "S_CHAIR", "class code")
	coach := flag.String("coach", "UMA", "coach whose seats everybody wants")
	seats := flag.Int("seats", 50, "how many seats of the coach are contested")
	to := flag.Int("to", 0, "to_idx of every click (0 = 1, a one-segment trip from the origin)")
	userBase := flag.Int64("user-base", 1_000_000, "first user id")
	flag.Parse()
	if *journey == 0 {
		fmt.Fprintln(os.Stderr, "--journey is required")
		os.Exit(2)
	}
	if *to == 0 {
		*to = 1
	}

	cl, err := kafka.NewProducer(strings.Split(*brokers, ","))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer cl.Close()
	ctx := context.Background()
	ids := make([]string, *n)
	seatOf := make([]string, *n)
	start := time.Now()
	var wg sync.WaitGroup
	produce := func(cmd *bookingv1.Command) {
		b, _ := proto.Marshal(cmd)
		wg.Add(1)
		cl.Produce(ctx, &kgo.Record{Topic: kafka.TopicBookingRequests, Key: kafka.JourneyKey(*journey), Value: b},
			func(_ *kgo.Record, err error) {
				if err != nil {
					fmt.Fprintln(os.Stderr, "produce:", err)
				}
				wg.Done()
			})
	}
	for i := 0; i < *n; i++ {
		ids[i] = uuid.NewString()
		seatOf[i] = fmt.Sprintf("%s-%d", *coach, 1+i%*seats)
		produce(&bookingv1.Command{RequestId: ids[i], JourneyId: *journey, Body: &bookingv1.Command_Select{Select: &bookingv1.Select{
			UserId: *userBase + int64(i), SeatId: seatOf[i], FromIdx: 0, ToIdx: int32(*to), ClassCode: *class, Channel: bookingv1.Channel_WEB}}})
	}
	wg.Wait()
	fmt.Printf("produced %d clicks on %d seats in %v\n", *n, *seats, time.Since(start).Round(time.Millisecond))

	rdb := redis.NewClient(&redis.Options{Addr: *redisAddr})
	var counts map[string]int
	var winners map[string][]int64
	deadline := time.Now().Add(90 * time.Second)
	for {
		counts, winners = map[string]int{}, map[string][]int64{}
		done := 0
		for i := 0; i < *n; i += 500 {
			end := min(i+500, *n)
			keys := make([]string, 0, end-i)
			for _, id := range ids[i:end] {
				keys = append(keys, wire.ResultKey(id))
			}
			vals, _ := rdb.MGet(ctx, keys...).Result()
			for k, v := range vals {
				s, ok := v.(string)
				if !ok {
					continue
				}
				done++
				var r wire.Result
				_ = json.Unmarshal([]byte(s), &r)
				label := r.Kind
				if r.Kind == "SelectRejected" {
					var e struct{ Code string }
					_ = json.Unmarshal(r.Event, &e)
					label += "/" + e.Code
				}
				if r.Kind == "SeatSelected" {
					seat := seatOf[i+k]
					winners[seat] = append(winners[seat], r.UserID)
				}
				counts[label]++
			}
		}
		if done == *n || time.Now().After(deadline) {
			fmt.Printf("answered %d/%d in %v\n", done, *n, time.Since(start).Round(time.Millisecond))
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	labels := make([]string, 0, len(counts))
	for k := range counts {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	for _, k := range labels {
		fmt.Printf("  %-34s %d\n", k, counts[k])
	}

	failed := false
	for seat, users := range winners {
		if len(users) > 1 {
			fmt.Printf("FAIL seat %s locked by %d users: %v\n", seat, len(users), users)
			failed = true
		}
	}
	if lost := *n - counts["SeatSelected"]; counts["SelectRejected/SEAT_TAKEN"] != lost {
		fmt.Printf("FAIL %d losers but %d SEAT_TAKEN answers\n", lost, counts["SelectRejected/SEAT_TAKEN"])
		failed = true
	}

	for _, users := range winners { // release the winners so the train is usable again
		for _, u := range users {
			produce(&bookingv1.Command{RequestId: uuid.NewString(), JourneyId: *journey,
				Body: &bookingv1.Command_Release{Release: &bookingv1.Release{UserId: u}}})
		}
	}
	wg.Wait()
	if failed {
		os.Exit(1)
	}
	fmt.Printf("PASS: %d seats, one owner each; %d clicks refused with SEAT_TAKEN\n", len(winners), counts["SelectRejected/SEAT_TAKEN"])
}
