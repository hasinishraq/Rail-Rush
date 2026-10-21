// Package wire holds the formats shared between the allocator (writer) and the
// API, live and search roles (readers): Redis keys, command results, cart and
// hold summaries, the seat map snapshot and seat deltas (§5.7, §11.6, §11.7,
// Appendix A).
package wire

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ---- Redis keys (§5.7)

func SeatMapKey(journeyID int64) string { return fmt.Sprintf("seatmap:%d", journeyID) }
func SegFreeKey(journeyID int64, class, quota string) string {
	return fmt.Sprintf("segfree:%d:%s:%s", journeyID, class, quota)
}
func DeltaChannel(journeyID int64) string    { return fmt.Sprintf("smd:%d", journeyID) }
func DeltaLogKey(journeyID int64) string     { return fmt.Sprintf("smlog:%d", journeyID) }
func CartKey(journeyID, userID int64) string { return fmt.Sprintf("cart:%d:%d", journeyID, userID) }
func HoldKey(pnr string) string              { return "hold:" + pnr }
func ResultKey(requestID string) string      { return "result:" + requestID }
func RequestKey(requestID string) string     { return "req:" + requestID } // owner of a pending request
func ResultsChannel(podID string) string     { return "results:" + podID }
func AdmitKey(jti string) string             { return "admit:" + jti }
func AdmitUsedKey(jti string) string         { return "admit:used:" + jti }
func ULimitKey(userID int64, runDate string) string {
	return fmt.Sprintf("ulimit:%d:%s", userID, runDate)
}
func TakenKey(userID, journeyID int64) string { return fmt.Sprintf("taken:%d:%d", userID, journeyID) }
func CoolKey(userID, journeyID int64) string {
	return fmt.Sprintf("taken:cool:%d:%d", userID, journeyID)
}
func LiveConnKey(userID, journeyID int64) string { return fmt.Sprintf("live:%d:%d", userID, journeyID) }
func OTPRateKey(userID int64) string             { return fmt.Sprintf("otp:rl:%d", userID) }
func LagKey(partition int32) string              { return fmt.Sprintf("lag:%d", partition) }
func JWTDenyKey(jti string) string               { return "jwt:deny:" + jti }
func IdemKey(userID int64, endpoint, key string) string {
	return fmt.Sprintf("idem:%d:%s:%s", userID, endpoint, key)
}

const (
	JWTDenyChannel = "jwt:deny"
	ResultTTL      = 15 * time.Minute
	HoldTTL        = 30 * time.Minute
	LagTTL         = 10 * time.Second
)

// Result is what the allocator writes to result:{id} and publishes to
// results:{pod}. The API maps it to HTTP; the allocator never builds HTTP bodies.
type Result struct {
	RequestID string          `json:"request_id"`
	UserID    int64           `json:"user_id"`
	JourneyID int64           `json:"journey_id"`
	Kind      string          `json:"kind"`  // SeatSelected | SelectRejected | SeatReleased | Cart | SeatHeld | HoldRejected | Cancelled | ...
	Event     json.RawMessage `json:"event"` // protojson of the event body
	Version   int64           `json:"version"`
	Cart      *CartSummary    `json:"cart,omitempty"` // the user's cart after the decision (select, release)
	Hold      *HoldSummary    `json:"hold,omitempty"` // PNR state after the decision (hold, cancel)
}

// KindCart is a Result with no event: the command changed nothing (double
// click, unselect of a seat not in the cart) and the answer is the current cart.
const KindCart = "Cart"

type SeatRef struct {
	SeatIdx int32  `json:"seat_idx"`
	SeatID  string `json:"seat_id"`
}

// CartSummary is cart:{journey}:{user}: the user's locked seats.
type CartSummary struct {
	JourneyID int64     `json:"journey_id"`
	UserID    int64     `json:"user_id"`
	FromIdx   int       `json:"from_idx"`
	ToIdx     int       `json:"to_idx"`
	ClassCode string    `json:"class_code"`
	Quota     string    `json:"quota"`
	Seats     []SeatRef `json:"seats"`
	MaxSeats  int       `json:"max_seats"` // pool limit minus seats already held/bought
	ExpiresAt time.Time `json:"expires_at"`
	Version   int64     `json:"version"`
}

// HoldSummary is hold:{pnr}: enough for OTP, payments and the booking fallback
// to work before the projection catches up.
type HoldSummary struct {
	PNR          string       `json:"pnr"`
	UserID       int64        `json:"user_id"`
	JourneyID    int64        `json:"journey_id"`
	ClassCode    string       `json:"class_code"`
	Quota        string       `json:"quota"`
	FromIdx      int          `json:"from_idx"`
	ToIdx        int          `json:"to_idx"`
	AmountMinor  int64        `json:"amount_minor"`
	Currency     string       `json:"currency"`
	ExpiresAt    time.Time    `json:"expires_at"`
	Status       string       `json:"status"` // PENDING_PAYMENT | CONFIRMED | PARTIALLY_CANCELLED | CANCELLED | EXPIRED | CONFIRM_FAILED
	PaymentID    string       `json:"payment_id,omitempty"`
	Passengers   []PaxSummary `json:"passengers"`
	Late         bool         `json:"late,omitempty"`
	SeatsChanged bool         `json:"seats_changed,omitempty"`
	UpdatedAt    time.Time    `json:"updated_at"`
}

type PaxSummary struct {
	Seq     int    `json:"seq"`
	Name    string `json:"name"`
	Gender  string `json:"gender"`
	Type    string `json:"type"`
	SeatIdx int32  `json:"seat_idx"`
	SeatID  string `json:"seat_id"`
	Status  string `json:"status"` // HELD | ISSUED | CANCELLED | EXPIRED | FAILED
}

// SeatMap is the seat map snapshot (§11.6), stored at seatmap:{journey} and
// served unchanged by GET /v1/journeys/{id}/seatmap.
type SeatMap struct {
	JourneyID   int64            `json:"journey_id"`
	Version     int64            `json:"version"`
	GeneratedAt time.Time        `json:"generated_at"`
	Segments    int              `json:"segments"`
	Encoding    string           `json:"encoding"`  // "u64le-base64"
	Seats       string           `json:"seats"`     // base64(len(seats) x uint64 LE) occupancy
	SeatPool    string           `json:"seat_pool"` // base64(len(seats) x uint8), 255 = not sellable
	Pools       []SeatMapPool    `json:"pools"`
	SegFree     map[string][]int `json:"segfree"` // "S_CHAIR/ONLINE" -> free seats per segment
	LiveURL     string           `json:"live_url"`
}

type SeatMapPool struct {
	ClassCode string `json:"class_code"`
	Quota     string `json:"quota"`
	Open      bool   `json:"open"`
	MaxSeats  int    `json:"max_seats_per_user"`
}

// AnyOpen reports whether any pool of the journey is open.
func (s *SeatMap) AnyOpen() bool {
	for _, p := range s.Pools {
		if p.Open {
			return true
		}
	}
	return false
}

// Bits decodes the occupancy bitmaps.
func (s *SeatMap) Bits() ([]uint64, error) {
	raw, err := base64.StdEncoding.DecodeString(s.Seats)
	if err != nil {
		return nil, err
	}
	out := make([]uint64, len(raw)/8)
	for i := range out {
		out[i] = binary.LittleEndian.Uint64(raw[i*8:])
	}
	return out, nil
}

// EncodeBits is the inverse of Bits.
func EncodeBits(bits []uint64) string {
	raw := make([]byte, 8*len(bits))
	for i, b := range bits {
		binary.LittleEndian.PutUint64(raw[i*8:], b)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// SeatDelta is one coalesced change of a journey's seat map (§11.7), published
// on smd:{journey} and appended to smlog:{journey}.
type SeatDelta struct {
	FromV   int64     `json:"from_v"`
	V       int64     `json:"v"`
	Changes string    `json:"changes"` // base64 of (u32 seat_idx, u64 bits) LE pairs
	At      time.Time `json:"at"`      // publish time, for the delta-age alert
}

type Change struct {
	SeatIdx int32
	Bits    uint64
}

func EncodeChanges(cs []Change) string {
	raw := make([]byte, 12*len(cs))
	for i, c := range cs {
		binary.LittleEndian.PutUint32(raw[i*12:], uint32(c.SeatIdx))
		binary.LittleEndian.PutUint64(raw[i*12+4:], c.Bits)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func DecodeChanges(s string) ([]Change, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(raw)%12 != 0 {
		return nil, errors.New("delta changes: length not a multiple of 12")
	}
	out := make([]Change, len(raw)/12)
	for i := range out {
		out[i] = Change{SeatIdx: int32(binary.LittleEndian.Uint32(raw[i*12:])), Bits: binary.LittleEndian.Uint64(raw[i*12+4:])}
	}
	return out, nil
}
