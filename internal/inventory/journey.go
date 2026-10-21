package inventory

import (
	"errors"
	"fmt"
	"math/bits"
	"sort"
	"time"

	bookingv1 "railway/gen/booking/v1"
)

// NoPool marks a seat that belongs to no sellable pool.
const NoPool = 255

// DefaultQuota is the quota used when a request names none.
const DefaultQuota = "ONLINE"

type PoolKey struct{ Class, Quota string } // e.g. {"S_CHAIR","ONLINE"}

func (k PoolKey) String() string { return k.Class + "/" + k.Quota }

type ClaimKind uint8

const (
	Selected ClaimKind = iota + 1
	Held
	Booked
)

// Claim is one user's hold on some segments of one seat.
type Claim struct {
	Mask  uint64
	Kind  ClaimKind
	Owner int64  // user_id
	PNR   string // "" while Selected
}

type Pool struct {
	Key       PoolKey
	Index     uint8
	OpensAt   time.Time
	Open      bool // set only by applying an Opened event
	MaxSeats  int  // per user per journey, default 4
	SoldOut   bool
	SeatIdx   []int32
	FreeOnSeg []int32 // free seats per segment (for segfree / sold-out)
}

// Cart is the user's locked seats, before passenger details.
type Cart struct {
	UserID    int64
	Pool      uint8
	From, To  int
	Mask      uint64
	Seats     []int32 // in selection order, <= Pool.MaxSeats
	FirstAt   time.Time
	ExpiresAt time.Time // min(last activity + 2 min, FirstAt + 5 min)
	Channel   bookingv1.Channel
}

// Hold is a PNR in memory: live and unpaid in Journey.Holds, paid in
// Journey.Booked, expired in Journey.Expired. Seats[i] is passenger seq i+1.
type Hold struct {
	PNR          string
	UserID       int64
	Pool         uint8
	From, To     int
	Mask         uint64
	Seats        []int32 // exactly the cart's seats (or the late-confirm seats)
	Pax          []*bookingv1.Passenger
	ExpiresAt    time.Time
	AmountMinor  int64
	Currency     string
	Channel      bookingv1.Channel
	PaymentID    string    // set by Confirmed
	ExpiredAt    time.Time // set by HoldExpired
	CreatedAt    time.Time
	Cancelled    []bool // per passenger, set by Cancelled
	Late         bool
	SeatsChanged bool
}

// Booking is a paid hold. The same struct moves from Holds to Booked on Confirmed.
type Booking = Hold

// LiveSeats is the number of passengers not cancelled.
func (h *Hold) LiveSeats() int {
	n := 0
	for i := range h.Seats {
		if !h.Cancelled[i] {
			n++
		}
	}
	return n
}

// SeenEntry is the answer to an idempotency key: a retried command gets the same event.
type SeenEntry struct {
	UserID int64
	Event  *bookingv1.Event
}

// PriceFunc computes the amount of a hold (fare chart x seats + service charge).
// It is pure and called from Decide, so the amount ends up in SeatHeld and
// replay is exact.
type PriceFunc func(pool PoolKey, from, to int, pax []*bookingv1.Passenger) (amountMinor int64, currency string)

type Config struct {
	SelectIdleTTL time.Duration // 2 min after the last select/unselect
	SelectMaxTTL  time.Duration // 5 min after the first seat
	HoldTTL       time.Duration // 10 min
	LateGrace     time.Duration // on-time payer grace for a late Confirm (W10), 30 s
	Price         PriceFunc
}

func (c *Config) defaults() {
	if c.SelectIdleTTL == 0 {
		c.SelectIdleTTL = 2 * time.Minute
	}
	if c.SelectMaxTTL == 0 {
		c.SelectMaxTTL = 5 * time.Minute
	}
	if c.HoldTTL == 0 {
		c.HoldTTL = 10 * time.Minute
	}
	if c.LateGrace == 0 {
		c.LateGrace = 30 * time.Second
	}
	if c.Price == nil {
		c.Price = func(PoolKey, int, int, []*bookingv1.Passenger) (int64, string) { return 0, "BDT" }
	}
}

type Journey struct {
	ID        int64
	Segments  int               // stops - 1, at most 64
	Seats     []uint64          // occupancy per seat: OR of all claim masks
	Claims    [][]Claim         // per seat; usually 0 or 1 entries, more on segment trips
	SeatIDs   []string          // seat_idx -> "KHA-12"
	SeatByID  map[string]int32  //
	Coach     []uint16          // seat_idx -> coach ordinal
	CoachName []string          // coach ordinal -> "KHA"
	SeatNo    []int16           // seat_idx -> seat number in its coach
	PoolOf    []uint8           // seat_idx -> index into Pools (NoPool = not sellable)
	Pools     []*Pool           // sorted by (class, quota); index = Pool.Index
	PoolByKey map[PoolKey]*Pool //
	Carts     map[int64]*Cart   // user_id -> live selection on this journey
	Holds     map[string]*Hold  // pnr -> unpaid hold
	Booked    map[string]*Hold  // pnr -> paid booking
	Expired   map[string]*Hold  // pnr -> expired hold, kept 24 h for a late Confirm
	UserSeats map[int64]int     // seats held + booked per user on this journey
	UserHold  map[int64]string  // user_id -> pnr of the live hold (not for counters)
	Timers    deadlineHeap      // carts and holds, by deadline
	Version   int64             // events applied = seat map version
	Seen      map[string]*SeenEntry
	Closed    bool
	LastEvent time.Time

	cfg        Config
	dirty      []int32 // seats changed since the last TakeDirty (not persisted)
	dirtyMark  []bool
	dirtyPools map[uint8]bool
}

// SeatDef is one row of layout_seat joined with quota_plan_seat.
type SeatDef struct {
	SeatID string
	Coach  string
	SeatNo int
	Class  string
	Quota  string
}

// PoolDef is one row of journey_pool.
type PoolDef struct {
	Class, Quota string
	OpensAt      time.Time
	MaxSeats     int
}

// Definition is everything needed to build an empty journey; Seats[i] is seat_idx i.
type Definition struct {
	JourneyID int64
	Segments  int
	Seats     []SeatDef
	Pools     []PoolDef
}

// New builds an empty journey (nothing locked or sold, every pool closed).
func New(def Definition, cfg Config) (*Journey, error) {
	cfg.defaults()
	if def.Segments < 1 || def.Segments > 64 {
		return nil, fmt.Errorf("journey %d: segments %d out of range 1..64", def.JourneyID, def.Segments)
	}
	if len(def.Seats) == 0 {
		return nil, errors.New("journey has no seats")
	}
	n := len(def.Seats)
	j := &Journey{
		ID: def.JourneyID, Segments: def.Segments,
		Seats: make([]uint64, n), Claims: make([][]Claim, n),
		SeatIDs: make([]string, n), SeatByID: make(map[string]int32, n),
		Coach: make([]uint16, n), SeatNo: make([]int16, n), PoolOf: make([]uint8, n),
		PoolByKey: map[PoolKey]*Pool{},
		Carts:     map[int64]*Cart{}, Holds: map[string]*Hold{}, Booked: map[string]*Hold{}, Expired: map[string]*Hold{},
		UserSeats: map[int64]int{}, UserHold: map[int64]string{}, Seen: map[string]*SeenEntry{},
		cfg: cfg, dirtyMark: make([]bool, n), dirtyPools: map[uint8]bool{},
	}

	pdefs := append([]PoolDef(nil), def.Pools...)
	sort.Slice(pdefs, func(a, b int) bool {
		if pdefs[a].Class != pdefs[b].Class {
			return pdefs[a].Class < pdefs[b].Class
		}
		return pdefs[a].Quota < pdefs[b].Quota
	})
	if len(pdefs) >= NoPool {
		return nil, errors.New("too many pools")
	}
	for i, pd := range pdefs {
		max := pd.MaxSeats
		if max <= 0 {
			max = 4
		}
		p := &Pool{Key: PoolKey{pd.Class, pd.Quota}, Index: uint8(i), OpensAt: pd.OpensAt, MaxSeats: max}
		j.Pools = append(j.Pools, p)
		j.PoolByKey[p.Key] = p
	}

	coachOrd := map[string]uint16{}
	for i, s := range def.Seats {
		if _, dup := j.SeatByID[s.SeatID]; dup {
			return nil, fmt.Errorf("duplicate seat id %q", s.SeatID)
		}
		j.SeatIDs[i] = s.SeatID
		j.SeatByID[s.SeatID] = int32(i)
		ord, ok := coachOrd[s.Coach]
		if !ok {
			ord = uint16(len(j.CoachName))
			coachOrd[s.Coach] = ord
			j.CoachName = append(j.CoachName, s.Coach)
		}
		j.Coach[i] = ord
		j.SeatNo[i] = int16(s.SeatNo)
		j.PoolOf[i] = NoPool
		if p := j.PoolByKey[PoolKey{s.Class, s.Quota}]; p != nil {
			j.PoolOf[i] = p.Index
			p.SeatIdx = append(p.SeatIdx, int32(i))
		}
	}
	for _, p := range j.Pools {
		p.FreeOnSeg = make([]int32, j.Segments)
		for s := range p.FreeOnSeg {
			p.FreeOnSeg[s] = int32(len(p.SeatIdx))
		}
		p.SoldOut = len(p.SeatIdx) == 0
	}
	return j, nil
}

// SetConfig replaces the runtime config (fares, TTLs).
func (j *Journey) SetConfig(cfg Config) { cfg.defaults(); j.cfg = cfg }

// Config returns the runtime config.
func (j *Journey) Config() Config { return j.cfg }

func (j *Journey) seatRef(idx int32) *bookingv1.SeatRef {
	return &bookingv1.SeatRef{SeatIdx: idx, SeatId: j.SeatIDs[idx]}
}

func (j *Journey) seatRefs(idx []int32) []*bookingv1.SeatRef {
	out := make([]*bookingv1.SeatRef, len(idx))
	for i, s := range idx {
		out[i] = j.seatRef(s)
	}
	return out
}

func seatIdxs(refs []*bookingv1.SeatRef) []int32 {
	out := make([]int32, len(refs))
	for i, r := range refs {
		out[i] = r.SeatIdx
	}
	return out
}

func (j *Journey) validSeat(idx int32) bool { return idx >= 0 && int(idx) < len(j.Seats) }

// Lookup finds a PNR in any state: HELD, BOOKED or EXPIRED.
func (j *Journey) Lookup(pnr string) (h *Hold, state string) {
	if h = j.Holds[pnr]; h != nil {
		return h, "HELD"
	}
	if h = j.Booked[pnr]; h != nil {
		return h, "BOOKED"
	}
	if h = j.Expired[pnr]; h != nil {
		return h, "EXPIRED"
	}
	return nil, ""
}

// PoolOfHold returns the pool of a hold or cart index.
func (j *Journey) Pool(idx uint8) *Pool {
	if int(idx) < len(j.Pools) {
		return j.Pools[idx]
	}
	return nil
}

// FreeOnSeg returns a copy of the free seats per segment of p.
func (j *Journey) FreeOnSeg(p *Pool) []int {
	out := make([]int, len(p.FreeOnSeg))
	for i, n := range p.FreeOnSeg {
		out[i] = int(n)
	}
	return out
}

func full(p *Pool) bool {
	for _, n := range p.FreeOnSeg {
		if n > 0 {
			return false
		}
	}
	return true
}

// TakeDirty returns the seats whose bitmap changed since the last call.
func (j *Journey) TakeDirty() []int32 {
	out := j.dirty
	for _, i := range out {
		j.dirtyMark[i] = false
	}
	j.dirty = nil
	return out
}

// ---- claims and bitmaps

func (j *Journey) setBits(i int32, nb uint64) {
	old := j.Seats[i]
	if old == nb {
		return
	}
	j.Seats[i] = nb
	if pi := j.PoolOf[i]; pi != NoPool {
		p := j.Pools[pi]
		for diff := old ^ nb; diff != 0; diff &= diff - 1 {
			seg := bits.TrailingZeros64(diff)
			if seg >= len(p.FreeOnSeg) {
				continue
			}
			if nb&(uint64(1)<<seg) != 0 {
				p.FreeOnSeg[seg]--
			} else {
				p.FreeOnSeg[seg]++
			}
		}
		j.dirtyPools[pi] = true
	}
	if !j.dirtyMark[i] {
		j.dirtyMark[i] = true
		j.dirty = append(j.dirty, i)
	}
}

func (j *Journey) recompute(i int32) {
	var or uint64
	for _, c := range j.Claims[i] {
		or |= c.Mask
	}
	j.setBits(i, or)
}

func (j *Journey) addClaim(i int32, c Claim) {
	j.Claims[i] = append(j.Claims[i], c)
	j.setBits(i, j.Seats[i]|c.Mask)
}

// removeClaims drops the claims on seat i for which match returns true.
func (j *Journey) removeClaims(i int32, match func(c *Claim) bool) {
	cs := j.Claims[i][:0]
	for _, c := range j.Claims[i] {
		if !match(&c) {
			cs = append(cs, c)
		}
	}
	for k := len(cs); k < len(j.Claims[i]); k++ {
		j.Claims[i][k] = Claim{}
	}
	j.Claims[i] = cs
	if len(cs) == 0 {
		j.Claims[i] = nil
	}
	j.recompute(i)
}

// setKind changes matching claims of seat i to kind/pnr; it adds one if none matched.
func (j *Journey) setKind(i int32, mask uint64, owner int64, from ClaimKind, fromPNR string, to ClaimKind, pnr string) {
	for k := range j.Claims[i] {
		c := &j.Claims[i][k]
		if c.Owner == owner && c.Kind == from && c.PNR == fromPNR && c.Mask == mask {
			c.Kind, c.PNR = to, pnr
			return
		}
	}
	j.addClaim(i, Claim{Mask: mask, Kind: to, Owner: owner, PNR: pnr})
}

func removeInt32(s []int32, v int32) []int32 {
	out := make([]int32, 0, len(s))
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func containsInt32(s []int32, v int32) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
