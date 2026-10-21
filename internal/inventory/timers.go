package inventory

import (
	"container/heap"
	"time"
)

type timerKind uint8

const (
	cartTimer timerKind = iota + 1
	holdTimer
)

// deadline is one cart or hold deadline. Entries are lazy: a popped entry whose
// cart or hold is gone, or whose deadline moved, is skipped.
type deadline struct {
	At   time.Time
	Kind timerKind
	User int64  // cart
	PNR  string // hold
}

type deadlineHeap []deadline

func (h deadlineHeap) Len() int { return len(h) }
func (h deadlineHeap) Less(a, b int) bool {
	if !h[a].At.Equal(h[b].At) {
		return h[a].At.Before(h[b].At)
	}
	if h[a].Kind != h[b].Kind {
		return h[a].Kind < h[b].Kind
	}
	if h[a].User != h[b].User {
		return h[a].User < h[b].User
	}
	return h[a].PNR < h[b].PNR
}
func (h deadlineHeap) Swap(a, b int) { h[a], h[b] = h[b], h[a] }
func (h *deadlineHeap) Push(x any)   { *h = append(*h, x.(deadline)) }
func (h *deadlineHeap) Pop() any {
	old := *h
	it := old[len(old)-1]
	*h = old[:len(old)-1]
	return it
}

func (j *Journey) armCart(c *Cart) {
	heap.Push(&j.Timers, deadline{At: c.ExpiresAt, Kind: cartTimer, User: c.UserID})
}

func (j *Journey) armHold(h *Hold) {
	heap.Push(&j.Timers, deadline{At: h.ExpiresAt, Kind: holdTimer, PNR: h.PNR})
}

// rebuildTimers re-arms every live cart and hold from its deadline.
func (j *Journey) rebuildTimers() {
	j.Timers = j.Timers[:0]
	for _, c := range j.Carts {
		j.Timers = append(j.Timers, deadline{At: c.ExpiresAt, Kind: cartTimer, User: c.UserID})
	}
	for _, h := range j.Holds {
		j.Timers = append(j.Timers, deadline{At: h.ExpiresAt, Kind: holdTimer, PNR: h.PNR})
	}
	heap.Init(&j.Timers)
}

func (j *Journey) live(d deadline) bool {
	switch d.Kind {
	case cartTimer:
		c := j.Carts[d.User]
		return c != nil && c.ExpiresAt.Equal(d.At)
	case holdTimer:
		h := j.Holds[d.PNR]
		return h != nil && h.ExpiresAt.Equal(d.At)
	}
	return false
}

// NextDeadline returns the earliest live cart or hold deadline, if any.
func (j *Journey) NextDeadline() (time.Time, bool) {
	for j.Timers.Len() > 0 {
		if top := j.Timers[0]; j.live(top) {
			return top.At, true
		}
		heap.Pop(&j.Timers)
	}
	return time.Time{}, false
}
