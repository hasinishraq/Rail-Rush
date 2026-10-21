package fares

import (
	"testing"
	"time"
)

var (
	jan  = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	trip = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
)

// Fares as seeded in migrations/seed/bd.sql for Dhaka–Chattogram (seat61).
func bd() *Table {
	t := NewTable()
	t.AddFare("S_CHAIR", "DHK", "CTG", jan, 45000)
	t.AddFare("SNIGDHA", "DHK", "CTG", jan, 85500)
	t.AddFare("AC_B", "DHK", "CTG", jan, 154000)
	t.AddFare("AC_B", "DHK", "CTG", trip.AddDate(0, 0, 1), 160000) // a later revision
	t.AddCharge(jan, 2000)
	return t
}

func TestFareChart(t *testing.T) {
	f := bd()
	for class, want := range map[string]int64{"S_CHAIR": 45000, "SNIGDHA": 85500, "AC_B": 154000} {
		if got := f.Adult(class, "DHK", "CTG", trip).AmountMinor; got != want {
			t.Errorf("%s: got %d, want %d", class, got, want)
		}
	}
	if got := f.Adult("S_CHAIR", "CTG", "DHK", trip).AmountMinor; got != 45000 {
		t.Errorf("reverse direction: got %d", got)
	}
	if got := f.Adult("AC_B", "DHK", "CTG", trip.AddDate(0, 0, 2)).AmountMinor; got != 160000 {
		t.Errorf("revision not applied: got %d", got)
	}
}

func TestPriceWithChildAndServiceCharge(t *testing.T) {
	fare, charge := bd().Price("S_CHAIR", "DHK", "CTG", trip, []bool{false, true})
	if fare.AmountMinor != 45000+22500 || charge.AmountMinor != 4000 || fare.Currency != "BDT" {
		t.Fatalf("got %+v %+v", fare, charge)
	}
}

func TestRefundScale(t *testing.T) {
	dep := time.Date(2026, 10, 29, 7, 0, 0, 0, time.UTC)
	cases := []struct {
		class  string
		before time.Duration
		want   int64
	}{
		{"S_CHAIR", 48 * time.Hour, 33800}, // 25 % cancellation charge
		{"SNIGDHA", 30 * time.Hour, 27000}, // 40 % (AC)
		{"S_CHAIR", 18 * time.Hour, 22500}, // 50 % refund
		{"S_CHAIR", 8 * time.Hour, 11300},  // 25 % refund (rounded to whole taka)
		{"S_CHAIR", 2 * time.Hour, 0},
	}
	for _, c := range cases {
		if got := RefundAmount(45000, 1, 1, c.class, dep, dep.Add(-c.before), ReasonCancelled); got != c.want {
			t.Errorf("%s %v before: got %d, want %d", c.class, c.before, got, c.want)
		}
	}
	if got := RefundAmount(90000, 2, 1, "S_CHAIR", dep, dep.Add(-time.Hour), ReasonConfirmFailed); got != 90000 {
		t.Errorf("a failed confirmation refunds in full: got %d", got)
	}
}
