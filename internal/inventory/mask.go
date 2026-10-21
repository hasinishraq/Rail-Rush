// Package inventory is the pure seat-inventory core (§5.3): journeys, segment
// masks, claims, carts (selection locks), holds, Decide and Apply. It does no
// I/O (no Kafka, Redis or SQL), so it is the part covered by property tests and
// replayed identically after a restart.
package inventory

// Mask returns a bitmap with bits from..to-1 set: the segments a trip occupies.
func Mask(from, to int) uint64 { return ((uint64(1) << (to - from)) - 1) << from }

// Free reports whether no segment of m is taken on seat.
func Free(seat, m uint64) bool { return seat&m == 0 }

// lock / hold / book: j.Seats[i] |= m     release: recompute OR of remaining claims
