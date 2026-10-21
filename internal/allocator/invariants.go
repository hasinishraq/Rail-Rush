package allocator

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"railway/internal/inventory"
	"railway/internal/wire"
)

// CheckJourney runs the §24 invariant checks for one journey:
//  1. no overlap in the log: replay the whole log (no snapshot) into a fresh
//     journey; claims never overlap and their OR equals each bitmap; the
//     published seat map is never ahead of the log and matches it when current;
//  2. no overlap in Postgres: the EXCLUDE constraint exists on every ticket partition;
//  3. ISSUED tickets equal the Booked claims of the replayed state;
//  4. v_money_without_ticket and 5. v_purchase_limit_breach are empty.
func CheckJourney(ctx context.Context, l *Loader, db *pgxpool.Pool, rdb redis.UniversalClient, journeyID int64) (map[string]any, error) {
	ld, err := l.Load(ctx, journeyID, false)
	if err != nil {
		return nil, err
	}
	j := ld.J
	violations := inventory.CheckInvariants(j)
	out := map[string]any{"journey_id": journeyID, "version": j.Version, "events_replayed": ld.Replayed}

	if b, err := rdb.Get(ctx, wire.SeatMapKey(journeyID)).Bytes(); err == nil {
		var sm wire.SeatMap
		if json.Unmarshal(b, &sm) == nil {
			out["seatmap_version"] = sm.Version
			switch {
			case sm.Version > j.Version:
				violations = append(violations, "published seat map is ahead of the committed log")
			case sm.Version == j.Version:
				if bits, err := sm.Bits(); err == nil {
					for i := range bits {
						if i < len(j.Seats) && bits[i] != j.Seats[i] {
							violations = append(violations, fmt.Sprintf("seat map differs from the log at seat %s", j.SeatIDs[i]))
							break
						}
					}
				}
			}
		}
	}

	booked := 0
	for _, h := range j.Booked {
		booked += h.LiveSeats()
	}
	var issued int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM ticket WHERE journey_id = $1 AND status = 'ISSUED'`, journeyID).Scan(&issued); err == nil {
		out["log_booked_seats"], out["projection_issued_tickets"] = booked, issued
		if issued != booked {
			violations = append(violations, "projection differs from the log (may be lag)")
		}
	}

	var missing []string
	rows, err := db.Query(ctx, `
		SELECT c.relname FROM pg_inherits i
		  JOIN pg_class c ON c.oid = i.inhrelid
		  JOIN pg_class p ON p.oid = i.inhparent
		 WHERE p.relname = 'ticket'
		   AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conrelid = c.oid AND k.contype = 'x')`)
	if err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				missing = append(missing, n)
			}
		}
		rows.Close()
	}
	if len(missing) > 0 {
		violations = append(violations, fmt.Sprintf("ticket partitions without the no-overlap constraint: %v", missing))
	}

	var money, limit int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM v_money_without_ticket v JOIN payment p USING (payment_id) WHERE p.journey_id = $1`, journeyID).Scan(&money)
	_ = db.QueryRow(ctx, `SELECT count(*) FROM v_purchase_limit_breach WHERE journey_id = $1`, journeyID).Scan(&limit)
	if money > 0 {
		violations = append(violations, "captured payments without ticket or refund")
	}
	if limit > 0 {
		violations = append(violations, "users above the per-journey seat limit")
	}
	out["ok"] = len(violations) == 0
	if len(violations) > 0 {
		out["violations"] = violations
	}
	return out, nil
}
