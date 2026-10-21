// Package bloom is a small concurrent Bloom filter. It is only ever used to skip
// a Redis read ("definitely not revoked"); a "maybe" always falls through to Redis.
package bloom

import (
	"hash/fnv"
	"math"
	"sync"
)

type Filter struct {
	mu   sync.RWMutex
	bits []uint64
	k    int
	m    uint64
}

// New sizes the filter for n items at false-positive rate p.
func New(n int, p float64) *Filter {
	if n < 1 {
		n = 1
	}
	m := uint64(math.Ceil(-float64(n) * math.Log(p) / (math.Ln2 * math.Ln2)))
	k := int(math.Round(float64(m) / float64(n) * math.Ln2))
	if k < 1 {
		k = 1
	}
	return &Filter{bits: make([]uint64, (m+63)/64), k: k, m: m}
}

func (f *Filter) hashes(s string) (uint64, uint64) {
	h := fnv.New64a()
	h.Write([]byte(s))
	a := h.Sum64()
	h2 := fnv.New64()
	h2.Write([]byte(s))
	return a, h2.Sum64() | 1
}

func (f *Filter) Add(s string) {
	a, b := f.hashes(s)
	f.mu.Lock()
	for i := 0; i < f.k; i++ {
		x := (a + uint64(i)*b) % f.m
		f.bits[x/64] |= 1 << (x % 64)
	}
	f.mu.Unlock()
}

// MayContain is false only if s was definitely never added.
func (f *Filter) MayContain(s string) bool {
	a, b := f.hashes(s)
	f.mu.RLock()
	defer f.mu.RUnlock()
	for i := 0; i < f.k; i++ {
		x := (a + uint64(i)*b) % f.m
		if f.bits[x/64]&(1<<(x%64)) == 0 {
			return false
		}
	}
	return true
}
