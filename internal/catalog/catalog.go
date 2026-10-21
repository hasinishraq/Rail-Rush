// Package catalog is the in-process cache of reference data: stations, trains,
// routes, layouts, quota plans, journeys, pools and fares. It is read-mostly and
// reloaded every few minutes; journeys missing from the cache are fetched on demand.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	bookingv1 "railway/gen/booking/v1"
	"railway/internal/fares"
	"railway/internal/inventory"
)

var ErrNotFound = errors.New("not found")

// Dhaka is the local time zone of every station and of run dates.
var Dhaka = mustLoc("Asia/Dhaka")

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		return time.FixedZone(name, 6*3600)
	}
	return l
}

type Station struct {
	Code, Name, NameBN, City, TZ string
	loc                          *time.Location
}

type Stop struct {
	Idx          int
	StationCode  string
	ArrOffsetMin *int
	DepOffsetMin *int
	Km           int
}

type Train struct {
	TrainNo, Name string
	LayoutID      int
	Stops         []Stop
}

type Coach struct {
	Code      string
	Position  int
	ClassCode string
	Width     int
	Height    int
}

type Seat struct {
	SeatIdx   int32
	SeatID    string
	Coach     string
	SeatNo    int
	SeatType  string // WINDOW | AISLE | MIDDLE | BERTH_LOWER | BERTH_UPPER
	ClassCode string
	X, Y      int
}

type Layout struct {
	LayoutID int
	Coaches  []Coach // in rake order
	Seats    []Seat  // index = seat_idx
	byID     map[string]int
}

// SeatByID resolves "KHA-12".
func (l *Layout) SeatByID(id string) (Seat, bool) {
	if i, ok := l.byID[id]; ok {
		return l.Seats[i], true
	}
	return Seat{}, false
}

type Pool struct {
	ClassCode, Quota string
	OpensAt          time.Time
	MaxSeats         int
	Seats            int
}

type Journey struct {
	JourneyID    int64
	TrainNo      string
	RunDate      time.Time
	DepartsAt    time.Time
	Segments     int
	LayoutID     int
	QuotaPlanID  int
	SalesCloseAt time.Time
	Status       string // SCHEDULED | CLOSED | DEPARTED | CANCELLED
	Pools        []Pool
}

func (j *Journey) Pool(class, quota string) *Pool {
	for i := range j.Pools {
		if j.Pools[i].ClassCode == class && j.Pools[i].Quota == quota {
			return &j.Pools[i]
		}
	}
	return nil
}

// RunDateString is the run date as YYYY-MM-DD.
func (j *Journey) RunDateString() string { return j.RunDate.Format("2006-01-02") }

// Candidate is a journey that serves a searched trip.
type Candidate struct {
	Journey *Journey
	Train   *Train
	FromIdx int
	ToIdx   int
}

type Catalog struct {
	db  *pgxpool.Pool
	log *slog.Logger

	mu         sync.RWMutex
	stations   map[string]*Station
	stationIdx []*Station // sorted by code
	trains     map[string]*Train
	layouts    map[int]*Layout
	quotaPlans map[int]map[int32]string
	journeys   map[int64]*Journey
	byTrainDay map[string]*Journey // train_no|run_date
	fares      *fares.Table
}

func New(db *pgxpool.Pool, log *slog.Logger) *Catalog {
	return &Catalog{db: db, log: log}
}

// Run reloads the catalog every interval until ctx ends.
func (c *Catalog) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Load(ctx); err != nil {
				c.log.Warn("catalog reload failed", "err", err)
			}
		}
	}
}

func scanAll[T any](ctx context.Context, db *pgxpool.Pool, q string, scan func(pgx.Rows) (T, error), args ...any) ([]T, error) {
	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Load (re)reads every reference table.
func (c *Catalog) Load(ctx context.Context) error {
	sts, err := scanAll(ctx, c.db, `SELECT code, name, COALESCE(name_bn,''), COALESCE(city,''), tz FROM station WHERE active`,
		func(r pgx.Rows) (*Station, error) {
			s := &Station{}
			err := r.Scan(&s.Code, &s.Name, &s.NameBN, &s.City, &s.TZ)
			if s.loc, _ = time.LoadLocation(s.TZ); s.loc == nil {
				s.loc = Dhaka
			}
			return s, err
		})
	if err != nil {
		return err
	}
	stations := map[string]*Station{}
	for _, s := range sts {
		stations[s.Code] = s
	}

	trains := map[string]*Train{}
	ts, err := scanAll(ctx, c.db, `SELECT train_no, name, layout_id FROM train WHERE active`, func(r pgx.Rows) (*Train, error) {
		t := &Train{}
		return t, r.Scan(&t.TrainNo, &t.Name, &t.LayoutID)
	})
	if err != nil {
		return err
	}
	for _, t := range ts {
		trains[t.TrainNo] = t
	}
	type trainStop struct {
		no string
		s  Stop
	}
	stops, err := scanAll(ctx, c.db, `SELECT train_no, idx, station_code, arr_offset_min, dep_offset_min, km FROM train_stop ORDER BY train_no, idx`,
		func(r pgx.Rows) (trainStop, error) {
			var ts trainStop
			return ts, r.Scan(&ts.no, &ts.s.Idx, &ts.s.StationCode, &ts.s.ArrOffsetMin, &ts.s.DepOffsetMin, &ts.s.Km)
		})
	if err != nil {
		return err
	}
	for _, ts := range stops {
		if t := trains[ts.no]; t != nil {
			t.Stops = append(t.Stops, ts.s)
		}
	}

	layouts := map[int]*Layout{}
	type layoutCoach struct {
		lid int
		c   Coach
	}
	coaches, err := scanAll(ctx, c.db, `SELECT layout_id, coach_code, position, class_code, width, height FROM layout_coach ORDER BY layout_id, position`,
		func(r pgx.Rows) (layoutCoach, error) {
			var lc layoutCoach
			return lc, r.Scan(&lc.lid, &lc.c.Code, &lc.c.Position, &lc.c.ClassCode, &lc.c.Width, &lc.c.Height)
		})
	if err != nil {
		return err
	}
	coachClass := map[string]string{}
	for _, lc := range coaches {
		l := layouts[lc.lid]
		if l == nil {
			l = &Layout{LayoutID: lc.lid, byID: map[string]int{}}
			layouts[lc.lid] = l
		}
		l.Coaches = append(l.Coaches, lc.c)
		coachClass[fmt.Sprintf("%d|%s", lc.lid, lc.c.Code)] = lc.c.ClassCode
	}
	type layoutSeat struct {
		lid int
		s   Seat
	}
	seats, err := scanAll(ctx, c.db, `SELECT layout_id, seat_idx, seat_id, coach_code, seat_no, seat_type, x, y FROM layout_seat ORDER BY layout_id, seat_idx`,
		func(r pgx.Rows) (layoutSeat, error) {
			var ls layoutSeat
			return ls, r.Scan(&ls.lid, &ls.s.SeatIdx, &ls.s.SeatID, &ls.s.Coach, &ls.s.SeatNo, &ls.s.SeatType, &ls.s.X, &ls.s.Y)
		})
	if err != nil {
		return err
	}
	for _, ls := range seats {
		l := layouts[ls.lid]
		if l == nil {
			return fmt.Errorf("layout %d: seats without coaches", ls.lid)
		}
		if int(ls.s.SeatIdx) != len(l.Seats) {
			return fmt.Errorf("layout %d: seat_idx %d not contiguous", ls.lid, ls.s.SeatIdx)
		}
		ls.s.ClassCode = coachClass[fmt.Sprintf("%d|%s", ls.lid, ls.s.Coach)]
		l.byID[ls.s.SeatID] = len(l.Seats)
		l.Seats = append(l.Seats, ls.s)
	}

	plans := map[int]map[int32]string{}
	type planSeat struct {
		pid   int
		idx   int32
		quota string
	}
	ps, err := scanAll(ctx, c.db, `SELECT quota_plan_id, seat_idx, quota FROM quota_plan_seat`, func(r pgx.Rows) (planSeat, error) {
		var p planSeat
		return p, r.Scan(&p.pid, &p.idx, &p.quota)
	})
	if err != nil {
		return err
	}
	for _, p := range ps {
		if plans[p.pid] == nil {
			plans[p.pid] = map[int32]string{}
		}
		plans[p.pid][p.idx] = p.quota
	}

	ft, err := fares.Load(ctx, c.db)
	if err != nil {
		return err
	}
	journeys, err := c.queryJourneys(ctx, `WHERE j.run_date >= current_date - 2`, nil, layouts, plans)
	if err != nil {
		return err
	}

	idx := make([]*Station, 0, len(stations))
	for _, s := range stations {
		idx = append(idx, s)
	}
	sort.Slice(idx, func(a, b int) bool { return idx[a].Code < idx[b].Code })
	byDay := map[string]*Journey{}
	for _, j := range journeys {
		byDay[j.TrainNo+"|"+j.RunDateString()] = j
	}

	c.mu.Lock()
	c.stations, c.stationIdx, c.trains, c.layouts, c.quotaPlans = stations, idx, trains, layouts, plans
	c.journeys, c.byTrainDay, c.fares = journeys, byDay, ft
	c.mu.Unlock()
	return nil
}

func (c *Catalog) queryJourneys(ctx context.Context, where string, args []any, layouts map[int]*Layout, plans map[int]map[int32]string) (map[int64]*Journey, error) {
	js, err := scanAll(ctx, c.db, `SELECT j.journey_id, j.train_no, j.run_date, j.departs_at, j.n_segments, j.layout_id,
	        j.quota_plan_id, j.sales_close_at, j.status FROM journey j `+where,
		func(r pgx.Rows) (*Journey, error) {
			j := &Journey{}
			err := r.Scan(&j.JourneyID, &j.TrainNo, &j.RunDate, &j.DepartsAt, &j.Segments, &j.LayoutID, &j.QuotaPlanID, &j.SalesCloseAt, &j.Status)
			j.DepartsAt, j.SalesCloseAt = j.DepartsAt.UTC(), j.SalesCloseAt.UTC()
			return j, err
		}, args...)
	if err != nil {
		return nil, err
	}
	out := map[int64]*Journey{}
	ids := make([]int64, 0, len(js))
	for _, j := range js {
		out[j.JourneyID] = j
		ids = append(ids, j.JourneyID)
	}
	if len(ids) == 0 {
		return out, nil
	}
	type jp struct {
		id int64
		p  Pool
	}
	pools, err := scanAll(ctx, c.db, `SELECT journey_id, class_code, quota, opens_at, max_seats_per_user
	        FROM journey_pool WHERE journey_id = ANY($1) ORDER BY journey_id, class_code, quota`,
		func(r pgx.Rows) (jp, error) {
			var x jp
			err := r.Scan(&x.id, &x.p.ClassCode, &x.p.Quota, &x.p.OpensAt, &x.p.MaxSeats)
			x.p.OpensAt = x.p.OpensAt.UTC()
			return x, err
		}, ids)
	if err != nil {
		return nil, err
	}
	for _, x := range pools {
		if j := out[x.id]; j != nil {
			j.Pools = append(j.Pools, x.p)
		}
	}
	for _, j := range out {
		l, plan := layouts[j.LayoutID], plans[j.QuotaPlanID]
		if l == nil {
			continue
		}
		for i := range j.Pools {
			p := &j.Pools[i]
			for _, s := range l.Seats {
				if s.ClassCode == p.ClassCode && plan[s.SeatIdx] == p.Quota {
					p.Seats++
				}
			}
		}
	}
	return out, nil
}

// Journey returns a cached journey, fetching it from Postgres on a miss.
func (c *Catalog) Journey(ctx context.Context, id int64) (*Journey, error) {
	c.mu.RLock()
	j := c.journeys[id]
	layouts, plans := c.layouts, c.quotaPlans
	c.mu.RUnlock()
	if j != nil {
		return j, nil
	}
	if layouts == nil {
		if err := c.Load(ctx); err != nil {
			return nil, err
		}
		c.mu.RLock()
		j, layouts, plans = c.journeys[id], c.layouts, c.quotaPlans
		c.mu.RUnlock()
		if j != nil {
			return j, nil
		}
	}
	m, err := c.queryJourneys(ctx, `WHERE j.journey_id = $1`, []any{id}, layouts, plans)
	if err != nil {
		return nil, err
	}
	if j = m[id]; j == nil {
		return nil, ErrNotFound
	}
	c.mu.Lock()
	c.journeys[id] = j
	c.mu.Unlock()
	return j, nil
}

func (c *Catalog) Train(no string) (*Train, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if t := c.trains[no]; t != nil {
		return t, nil
	}
	return nil, ErrNotFound
}

func (c *Catalog) Layout(id int) (*Layout, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if l := c.layouts[id]; l != nil {
		return l, nil
	}
	return nil, ErrNotFound
}

func (c *Catalog) Station(code string) (*Station, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := c.stations[code]
	return s, s != nil
}

// Stations prefix-matches code or name (English or Bangla).
func (c *Catalog) Stations(q string, limit int) []*Station {
	q = strings.ToLower(strings.TrimSpace(q))
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []*Station
	for _, s := range c.stationIdx {
		if q == "" || strings.HasPrefix(strings.ToLower(s.Code), q) || strings.HasPrefix(strings.ToLower(s.Name), q) ||
			(s.NameBN != "" && strings.HasPrefix(s.NameBN, q)) {
			out = append(out, s)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

// StopTime returns the departure (or, at the last stop, arrival) time of stop idx.
func (c *Catalog) StopTime(j *Journey, t *Train, idx int, arrival bool) time.Time {
	if idx < 0 || idx >= len(t.Stops) {
		return j.DepartsAt
	}
	s := t.Stops[idx]
	off := s.DepOffsetMin
	if arrival || off == nil {
		off = s.ArrOffsetMin
	}
	if off == nil {
		off = s.DepOffsetMin
	}
	if off == nil {
		return j.DepartsAt
	}
	return j.DepartsAt.Add(time.Duration(*off) * time.Minute)
}

// Search finds journeys departing from on the local date that later stop at to.
func (c *Catalog) Search(_ context.Context, from, to string, date time.Time) ([]Candidate, error) {
	c.mu.RLock()
	fromSt := c.stations[from]
	var out []Candidate
	for _, t := range c.trains {
		fi, ti := -1, -1
		for _, s := range t.Stops {
			if s.StationCode == from && fi < 0 {
				fi = s.Idx
			}
			if s.StationCode == to && fi >= 0 {
				ti = s.Idx
			}
		}
		if fi < 0 || ti <= fi {
			continue
		}
		dep := t.Stops[fi].DepOffsetMin
		if dep == nil {
			continue
		}
		// The train may have left its origin up to a few days before reaching from.
		for back := 0; back <= 3; back++ {
			runDate := date.AddDate(0, 0, -back)
			j := c.byTrainDay[t.TrainNo+"|"+runDate.Format("2006-01-02")]
			if j == nil || j.Status == "CANCELLED" {
				continue
			}
			loc := Dhaka
			if fromSt != nil {
				loc = fromSt.loc
			}
			local := j.DepartsAt.Add(time.Duration(*dep) * time.Minute).In(loc)
			if local.Format("2006-01-02") == date.Format("2006-01-02") {
				out = append(out, Candidate{Journey: j, Train: t, FromIdx: fi, ToIdx: ti})
			}
		}
	}
	c.mu.RUnlock()
	sort.Slice(out, func(a, b int) bool {
		return c.StopTime(out[a].Journey, out[a].Train, out[a].FromIdx, false).Before(
			c.StopTime(out[b].Journey, out[b].Train, out[b].FromIdx, false))
	})
	return out, nil
}

func (c *Catalog) faresTable() *fares.Table {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fares
}

func stationsOf(t *Train, from, to int) (string, string, bool) {
	if from < 0 || to >= len(t.Stops) || from >= to {
		return "", "", false
	}
	return t.Stops[from].StationCode, t.Stops[to].StationCode, true
}

// Fare is the adult fare (before service charge) for a trip in a class.
func (c *Catalog) Fare(j *Journey, t *Train, class string, from, to int) fares.Money {
	ft := c.faresTable()
	fs, ts, ok := stationsOf(t, from, to)
	if ft == nil || !ok {
		return fares.Money{Currency: fares.DefaultCurrency}
	}
	return ft.Adult(class, fs, ts, j.RunDate)
}

// Price is the fare and the service charge for passengers on a trip.
func (c *Catalog) Price(j *Journey, t *Train, class string, from, to int, children []bool) (fare, charge fares.Money) {
	ft := c.faresTable()
	fs, ts, ok := stationsOf(t, from, to)
	if ft == nil || !ok {
		return fares.Money{Currency: fares.DefaultCurrency}, fares.Money{Currency: fares.DefaultCurrency}
	}
	return ft.Price(class, fs, ts, j.RunDate, children)
}

func children(pax []*bookingv1.Passenger) []bool {
	out := make([]bool, len(pax))
	for i, p := range pax {
		out[i] = p.GetType() == bookingv1.PassengerType_CHILD
	}
	return out
}

// PriceFunc returns the pure fare function the allocator passes into inventory:
// fare chart x seats + service charge, fixed for the journey's run date.
func (c *Catalog) PriceFunc(j *Journey, t *Train) inventory.PriceFunc {
	ft := c.faresTable()
	codes := make([]string, len(t.Stops))
	for i, s := range t.Stops {
		codes[i] = s.StationCode
	}
	day := j.RunDate
	return func(pool inventory.PoolKey, from, to int, pax []*bookingv1.Passenger) (int64, string) {
		if ft == nil || from < 0 || to >= len(codes) || from >= to {
			return 0, fares.DefaultCurrency
		}
		fare, charge := ft.Price(pool.Class, codes[from], codes[to], day, children(pax))
		return fare.AmountMinor + charge.AmountMinor, fare.Currency
	}
}

// Definition builds the inventory definition of a journey: layout seats with
// their quota, and pools.
func (c *Catalog) Definition(ctx context.Context, journeyID int64) (inventory.Definition, *Journey, *Train, error) {
	j, err := c.Journey(ctx, journeyID)
	if err != nil {
		return inventory.Definition{}, nil, nil, err
	}
	t, err := c.Train(j.TrainNo)
	l, lerr := c.Layout(j.LayoutID)
	if err != nil || lerr != nil { // reference data added since the last reload
		if err := c.Load(ctx); err != nil {
			return inventory.Definition{}, nil, nil, err
		}
		if t, err = c.Train(j.TrainNo); err != nil {
			return inventory.Definition{}, nil, nil, err
		}
		if l, err = c.Layout(j.LayoutID); err != nil {
			return inventory.Definition{}, nil, nil, err
		}
	}
	c.mu.RLock()
	plan := c.quotaPlans[j.QuotaPlanID]
	c.mu.RUnlock()
	def := inventory.Definition{JourneyID: j.JourneyID, Segments: j.Segments}
	for _, s := range l.Seats {
		def.Seats = append(def.Seats, inventory.SeatDef{SeatID: s.SeatID, Coach: s.Coach, SeatNo: s.SeatNo, Class: s.ClassCode, Quota: plan[s.SeatIdx]})
	}
	for _, p := range j.Pools {
		def.Pools = append(def.Pools, inventory.PoolDef{Class: p.ClassCode, Quota: p.Quota, OpensAt: p.OpensAt, MaxSeats: p.MaxSeats})
	}
	return def, j, t, nil
}

// StationName returns the display name of a station code.
func (c *Catalog) StationName(code string) string {
	if s, ok := c.Station(code); ok {
		return s.Name
	}
	return code
}

// IsNoRows reports pgx.ErrNoRows.
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
