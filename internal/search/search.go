// Package search serves the public read path: stations, train search, journey
// details, layouts and the seat map (§11, W3, W4). It never touches the
// allocator or Kafka; counts come from the cached seat map snapshots.
package search

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"railway/internal/api/dto"
	"railway/internal/booking"
	"railway/internal/catalog"
	"railway/internal/inventory"
	"railway/internal/platform"
	"railway/internal/wire"
)

var ErrNotFound = errors.New("not found")

type Service struct {
	Cat   *catalog.Catalog
	Store *booking.Store
	Clock platform.Clock
}

func (s *Service) Stations(q string, limit int) []dto.Station {
	out := []dto.Station{}
	for _, st := range s.Cat.Stations(q, limit) {
		out = append(out, dto.Station{Code: st.Code, Name: st.Name, NameBN: st.NameBN, City: st.City})
	}
	return out
}

func (s *Service) StopRef(j *catalog.Journey, t *catalog.Train, idx int, arrival bool) dto.StopRef {
	r := dto.StopRef{Idx: idx, Time: s.Cat.StopTime(j, t, idx, arrival)}
	if idx >= 0 && idx < len(t.Stops) {
		r.StationCode = t.Stops[idx].StationCode
		r.StationName = s.Cat.StationName(r.StationCode)
	}
	return r
}

// FreeSeats lists the seats of pool (index into Pools) free for the trip; the
// same logic the client runs on the seat map (§11.6). Selected, held and sold
// seats all count as taken.
func FreeSeats(sm *wire.SeatMap, fromIdx, toIdx int, pool int) ([]int32, error) {
	bits, err := sm.Bits()
	if err != nil {
		return nil, err
	}
	pools, err := base64.StdEncoding.DecodeString(sm.SeatPool)
	if err != nil {
		return nil, err
	}
	m := inventory.Mask(fromIdx, toIdx)
	var free []int32
	for i := 0; i < len(bits) && i < len(pools); i++ {
		if int(pools[i]) == pool && bits[i]&m == 0 {
			free = append(free, int32(i))
		}
	}
	return free, nil
}

// Search: route index -> journeys on date -> one MGET of their seat maps ->
// count seats free for Mask(from, to) in each pool.
func (s *Service) Search(ctx context.Context, from, to string, date time.Time, class string) (*dto.SearchResponse, error) {
	cands, err := s.Cat.Search(ctx, from, to, date)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(cands))
	for i, c := range cands {
		ids[i] = c.Journey.JourneyID
	}
	maps, err := s.Store.SeatMaps(ctx, ids)
	if err != nil {
		maps = make([]*wire.SeatMap, len(ids)) // Redis down: show what the catalog knows
	}
	now := s.Clock.Now()
	resp := &dto.SearchResponse{From: from, To: to, Date: date.Format("2006-01-02"), GeneratedAt: now, Trains: []dto.TrainOption{}}
	for i, c := range cands {
		dep := s.StopRef(c.Journey, c.Train, c.FromIdx, false)
		arr := s.StopRef(c.Journey, c.Train, c.ToIdx, true)
		opt := dto.TrainOption{JourneyID: c.Journey.JourneyID, TrainNo: c.Train.TrainNo,
			TrainName: c.Train.Name + " (" + c.Train.TrainNo + ")",
			From:      dep, To: arr, DurationMin: int(arr.Time.Sub(dep.Time).Minutes()), Classes: []dto.ClassAvail{}}
		for _, p := range c.Journey.Pools {
			if (class != "" && p.ClassCode != class) || p.Quota != inventory.DefaultQuota {
				continue
			}
			fare := s.Cat.Fare(c.Journey, c.Train, p.ClassCode, c.FromIdx, c.ToIdx)
			ca := dto.ClassAvail{ClassCode: p.ClassCode, Fare: dto.Money{AmountMinor: fare.AmountMinor, Currency: fare.Currency}}
			ca.Status, ca.Count = availability(c.Journey, &p, maps[i], c.FromIdx, c.ToIdx, now)
			if ca.Status == "NOT_OPEN" {
				t := p.OpensAt
				ca.OpensAt = &t
			}
			opt.Classes = append(opt.Classes, ca)
		}
		sort.Slice(opt.Classes, func(a, b int) bool { return opt.Classes[a].ClassCode < opt.Classes[b].ClassCode })
		resp.Trains = append(resp.Trains, opt)
	}
	return resp, nil
}

func availability(j *catalog.Journey, p *catalog.Pool, sm *wire.SeatMap, from, to int, now time.Time) (string, int) {
	if j.Status != "SCHEDULED" || !now.Before(j.SalesCloseAt) {
		return "CLOSED", 0
	}
	if sm == nil {
		if p.OpensAt.After(now) {
			return "NOT_OPEN", 0
		}
		return "AVAILABLE", p.Seats // no snapshot yet: nothing has been locked
	}
	idx := -1
	for k, sp := range sm.Pools {
		if sp.ClassCode == p.ClassCode && sp.Quota == p.Quota {
			idx = k
		}
	}
	if idx < 0 {
		return "SOLD_OUT", 0
	}
	if !sm.Pools[idx].Open {
		if p.OpensAt.After(now) {
			return "NOT_OPEN", 0
		}
		return "CLOSED", 0
	}
	free, err := FreeSeats(sm, from, to, idx)
	if err == nil && len(free) > 0 {
		return "AVAILABLE", len(free)
	}
	return "SOLD_OUT", 0
}

func (s *Service) Journey(ctx context.Context, id int64) (*dto.JourneyResponse, error) {
	j, err := s.Cat.Journey(ctx, id)
	if err != nil {
		return nil, ErrNotFound
	}
	t, err := s.Cat.Train(j.TrainNo)
	if err != nil {
		return nil, ErrNotFound
	}
	r := &dto.JourneyResponse{JourneyID: j.JourneyID, TrainNo: t.TrainNo, TrainName: t.Name, RunDate: j.RunDateString(),
		Status: j.Status, LayoutID: j.LayoutID, SalesCloseAt: j.SalesCloseAt, Route: []dto.StopRef{}, Pools: []dto.PoolInfo{}}
	if r.Status == "SCHEDULED" && !s.Clock.Now().Before(j.SalesCloseAt) {
		r.Status = "CLOSED"
	}
	for i := range t.Stops {
		r.Route = append(r.Route, s.StopRef(j, t, i, i == len(t.Stops)-1))
	}
	for _, p := range j.Pools {
		r.Pools = append(r.Pools, dto.PoolInfo{ClassCode: p.ClassCode, Quota: p.Quota, OpensAt: p.OpensAt, Seats: p.Seats, MaxSeats: p.MaxSeats})
	}
	return r, nil
}

func (s *Service) Layout(id int) (*dto.LayoutResponse, error) {
	l, err := s.Cat.Layout(id)
	if err != nil {
		return nil, ErrNotFound
	}
	r := &dto.LayoutResponse{LayoutID: l.LayoutID, Coaches: []dto.Coach{}}
	idx := map[string]int{}
	for _, c := range l.Coaches {
		idx[c.Code] = len(r.Coaches)
		r.Coaches = append(r.Coaches, dto.Coach{Coach: c.Code, ClassCode: c.ClassCode, Width: c.Width, Height: c.Height, Seats: []dto.Seat{}})
	}
	for _, st := range l.Seats {
		if ci, ok := idx[st.Coach]; ok {
			c := &r.Coaches[ci]
			c.Seats = append(c.Seats, dto.Seat{SeatIdx: st.SeatIdx, SeatID: st.SeatID, SeatNo: st.SeatNo, SeatType: st.SeatType, X: st.X, Y: st.Y})
		}
	}
	return r, nil
}

// SeatMap returns the raw snapshot and its decoded form; nil if not published yet.
func (s *Service) SeatMap(ctx context.Context, id int64) ([]byte, *wire.SeatMap, error) {
	raw, err := s.Store.SeatMap(ctx, id)
	if err != nil || raw == nil {
		return nil, nil, err
	}
	var sm wire.SeatMap
	if err := json.Unmarshal(raw, &sm); err != nil {
		return nil, nil, err
	}
	return raw, &sm, nil
}
