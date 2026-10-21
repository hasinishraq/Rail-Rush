// Package fares holds the fare and refund rules for Bangladesh Railway: the
// fare chart per station pair and class, a per-ticket online service charge,
// half fare for children, and the cancellation scale. Price is pure, so the
// allocator can call it from Decide.
package fares

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const DefaultCurrency = "BDT"

type Money struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

type pairKey struct{ Class, From, To string }

type dated struct {
	From   time.Time
	Amount int64
}

// Table is the fare chart (fare) plus the service charge (charge_rule SERVICE),
// each with its validity history.
type Table struct {
	fares  map[pairKey][]dated // sorted by From ascending
	charge []dated
}

func NewTable() *Table { return &Table{fares: map[pairKey][]dated{}} }

// AddFare adds one fare chart row; used by Load and tests.
func (t *Table) AddFare(class, from, to string, validFrom time.Time, amountMinor int64) {
	k := pairKey{class, from, to}
	t.fares[k] = append(t.fares[k], dated{validFrom, amountMinor})
	sort.Slice(t.fares[k], func(a, b int) bool { return t.fares[k][a].From.Before(t.fares[k][b].From) })
}

// AddCharge adds one service charge row.
func (t *Table) AddCharge(validFrom time.Time, perTicketMinor int64) {
	t.charge = append(t.charge, dated{validFrom, perTicketMinor})
	sort.Slice(t.charge, func(a, b int) bool { return t.charge[a].From.Before(t.charge[b].From) })
}

// Load reads fare and charge_rule.
func Load(ctx context.Context, db *pgxpool.Pool) (*Table, error) {
	t := NewTable()
	rows, err := db.Query(ctx, `SELECT class_code, from_station, to_station, valid_from, amount_minor FROM fare`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var class, from, to string
		var vf time.Time
		var amt int64
		if err := rows.Scan(&class, &from, &to, &vf, &amt); err != nil {
			rows.Close()
			return nil, err
		}
		t.AddFare(class, from, to, vf, amt)
	}
	rows.Close()
	rows, err = db.Query(ctx, `SELECT valid_from, per_ticket_minor FROM charge_rule WHERE charge_code = 'SERVICE'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var vf time.Time
		var amt int64
		if err := rows.Scan(&vf, &amt); err != nil {
			return nil, err
		}
		t.AddCharge(vf, amt)
	}
	return t, rows.Err()
}

func at(list []dated, day time.Time) (int64, bool) {
	var v int64
	ok := false
	for _, d := range list {
		if d.From.After(day) {
			break
		}
		v, ok = d.Amount, true
	}
	return v, ok
}

// Adult is the adult fare for a trip on the given travel date. A pair missing
// in one direction uses the other (the chart is symmetric).
func (t *Table) Adult(class, from, to string, day time.Time) Money {
	if v, ok := at(t.fares[pairKey{class, from, to}], day); ok {
		return Money{v, DefaultCurrency}
	}
	if v, ok := at(t.fares[pairKey{class, to, from}], day); ok {
		return Money{v, DefaultCurrency}
	}
	return Money{0, DefaultCurrency}
}

// ServiceCharge is the online service charge per ticket on the given date.
func (t *Table) ServiceCharge(day time.Time) Money {
	v, _ := at(t.charge, day)
	return Money{v, DefaultCurrency}
}

// roundUnit rounds minor units to the nearest whole taka (fares are sold in whole taka).
func roundUnit(minor int64) int64 { return (minor + 50) / 100 * 100 }

// PassengerFare is one passenger's fare: children pay half.
func PassengerFare(adult int64, child bool) int64 {
	if child {
		return roundUnit(adult / 2)
	}
	return adult
}

// Price is fare chart x seats (children at half fare) plus the service charge
// per ticket: the amount of a hold (§13, W7).
func (t *Table) Price(class, from, to string, day time.Time, children []bool) (fare, charge Money) {
	adult := t.Adult(class, from, to, day)
	fare, charge = Money{Currency: adult.Currency}, Money{Currency: adult.Currency}
	per := t.ServiceCharge(day).AmountMinor
	for _, c := range children {
		fare.AmountMinor += PassengerFare(adult.AmountMinor, c)
		charge.AmountMinor += per
	}
	return fare, charge
}

// Refund reasons (refund.reason).
const (
	ReasonCancelled        = "CANCELLED"
	ReasonConfirmFailed    = "CONFIRM_FAILED"
	ReasonSalesClosed      = "SALES_CLOSED"
	ReasonAmountMismatch   = "AMOUNT_MISMATCH"
	ReasonDuplicateCapture = "DUPLICATE_CAPTURE"
)

// CancellationCharge is the share kept when a ticket is returned 24 hours or
// more before departure: 40 % for air-conditioned classes, 30 % for first
// class, 25 % for the others.
func CancellationCharge(class string) (num, den int64) {
	switch class {
	case "AC_B", "AC_S", "SNIGDHA":
		return 40, 100
	case "F_BERTH", "F_SEAT", "F_CHAIR":
		return 30, 100
	}
	return 25, 100
}

// RefundAmount returns what is refunded when nCancelled of nPax passengers
// (paid in total) are cancelled at time at:
//
//	24 h or more before departure  fare minus the class cancellation charge
//	12–24 h                        50 %
//	6–12 h                         25 %
//	under 6 h                      nothing
//
// System failures (booking failed, sales closed, duplicate capture, amount
// mismatch) refund in full.
func RefundAmount(paid int64, nPax, nCancelled int, class string, departsAt, at time.Time, reason string) int64 {
	if reason != ReasonCancelled {
		return paid
	}
	if nPax <= 0 || nCancelled <= 0 {
		return 0
	}
	if nCancelled > nPax {
		nCancelled = nPax
	}
	share := paid / int64(nPax) * int64(nCancelled)
	if nCancelled == nPax {
		share = paid
	}
	left := departsAt.Sub(at)
	switch {
	case left >= 24*time.Hour:
		n, d := CancellationCharge(class)
		return roundUnit(share - share*n/d)
	case left >= 12*time.Hour:
		return roundUnit(share / 2)
	case left >= 6*time.Hour:
		return roundUnit(share / 4)
	}
	return 0
}
