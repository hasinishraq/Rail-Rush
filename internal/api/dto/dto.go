// Package dto holds the request and response types of the public API (Part II).
package dto

import (
	"encoding/json"
	"time"

	"railway/internal/otp"
	"railway/internal/wire"
)

// ---- shared (§9.7)

type Money struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"` // "BDT"
}

type PassengerIn struct {
	Name   string `json:"name"`           // 1..40 chars
	Gender string `json:"gender"`         // MALE | FEMALE | OTHER
	Type   string `json:"type,omitempty"` // ADULT (default) | CHILD
}

type SeatRef struct {
	SeatID  string `json:"seat_id"` // "KHA-12"
	SeatIdx int32  `json:"seat_idx"`
	Coach   string `json:"coach"` // "KHA"
	SeatNo  int    `json:"seat_no"`
}

type PassengerSeat struct {
	Seq    int     `json:"seq"`
	Name   string  `json:"name"`
	Gender string  `json:"gender"`
	Type   string  `json:"type"`
	Status string  `json:"status"` // HELD | ISSUED | CANCELLED | EXPIRED | FAILED
	Seat   SeatRef `json:"seat"`
	Fare   Money   `json:"fare"`
}

type StopRef struct {
	Idx         int       `json:"idx"`
	StationCode string    `json:"station_code"`
	StationName string    `json:"station_name"`
	Time        time.Time `json:"time"`
}

type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// ---- errors (§9.5)

type ErrorResponse struct {
	Error APIError `json:"error"`
}

type APIError struct {
	Code      string         `json:"code"`    // stable, machine-readable
	Message   string         `json:"message"` // human-readable, may change
	RequestID string         `json:"request_id"`
	Details   map[string]any `json:"details,omitempty"`
}

// ---- auth (§10)

type RegisterRequest struct {
	Phone    string `json:"phone"` // +8801XXXXXXXXX
	Email    string `json:"email,omitempty"`
	Name     string `json:"name"`
	NID      string `json:"nid,omitempty"` // stored only as HMAC
	Password string `json:"password"`      // >= 10 chars, not breached

	CaptchaToken string `json:"captcha_token,omitempty"` // Turnstile response, when enabled
}

type RegisterResponse struct {
	UserID int64 `json:"user_id"`
}

type LoginRequest struct {
	Login        string `json:"login"` // phone or e-mail
	Password     string `json:"password"`
	DeviceID     string `json:"device_id"`
	CaptchaToken string `json:"captcha_token,omitempty"`
}

type CaptchaConfig struct {
	Provider string `json:"provider,omitempty"` // "turnstile"; empty when disabled
	SiteKey  string `json:"site_key,omitempty"`
}

type TokenResponse struct {
	AccessToken      string   `json:"access_token"`
	TokenType        string   `json:"token_type"` // "Bearer"
	ExpiresIn        int      `json:"expires_in"` // 900
	RefreshToken     string   `json:"refresh_token"`
	RefreshExpiresIn int      `json:"refresh_expires_in"` // 2592000
	User             UserInfo `json:"user"`
}

type UserInfo struct {
	UserID   int64  `json:"user_id"`
	Name     string `json:"name"`
	Phone    string `json:"phone"`
	Verified bool   `json:"verified"`
}

type MeResponse struct {
	UserInfo
	Email string `json:"email,omitempty"`
	Role  string `json:"role"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
	DeviceID     string `json:"device_id"`
}

type LogoutRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type SavedPassenger struct {
	ID int64 `json:"id"`
	PassengerIn
}

// ---- search, journey, layout, seat map (§11)

type Station struct {
	Code   string `json:"code"`
	Name   string `json:"name"`
	NameBN string `json:"name_bn,omitempty"`
	City   string `json:"city"`
}

type SearchResponse struct {
	From        string        `json:"from"`
	To          string        `json:"to"`
	Date        string        `json:"date"`
	GeneratedAt time.Time     `json:"generated_at"`
	Trains      []TrainOption `json:"trains"`
}

type TrainOption struct {
	JourneyID   int64        `json:"journey_id"`
	TrainNo     string       `json:"train_no"`   // "701"
	TrainName   string       `json:"train_name"` // "SUBARNA EXPRESS (701)"
	From        StopRef      `json:"from"`
	To          StopRef      `json:"to"`
	DurationMin int          `json:"duration_min"`
	Classes     []ClassAvail `json:"classes"`
}

type ClassAvail struct {
	ClassCode string     `json:"class_code"` // S_CHAIR | SNIGDHA | AC_S | F_BERTH | AC_B ...
	Fare      Money      `json:"fare"`       // per seat, before service charge
	Status    string     `json:"status"`     // NOT_OPEN | AVAILABLE | SOLD_OUT | CLOSED
	Count     int        `json:"count"`      // free seats for this exact trip
	OpensAt   *time.Time `json:"opens_at,omitempty"`
}

type JourneyResponse struct {
	JourneyID    int64      `json:"journey_id"`
	TrainNo      string     `json:"train_no"`
	TrainName    string     `json:"train_name"`
	RunDate      string     `json:"run_date"`
	Status       string     `json:"status"` // SCHEDULED | CLOSED | DEPARTED | CANCELLED
	LayoutID     int        `json:"layout_id"`
	SalesCloseAt time.Time  `json:"sales_close_at"`
	Route        []StopRef  `json:"route"`
	Pools        []PoolInfo `json:"pools"`
}

type PoolInfo struct {
	ClassCode string    `json:"class_code"`
	Quota     string    `json:"quota"`
	OpensAt   time.Time `json:"opens_at"`
	Seats     int       `json:"seats"`
	MaxSeats  int       `json:"max_seats_per_user"`
}

type LayoutResponse struct {
	LayoutID int     `json:"layout_id"`
	Coaches  []Coach `json:"coaches"`
}

type Coach struct {
	Coach     string `json:"coach"` // "KHA"
	ClassCode string `json:"class_code"`
	Width     int    `json:"width"` // grid units
	Height    int    `json:"height"`
	Seats     []Seat `json:"seats"`
}

type Seat struct {
	SeatIdx  int32  `json:"seat_idx"` // index into seat map arrays
	SeatID   string `json:"seat_id"`  // "KHA-12"
	SeatNo   int    `json:"seat_no"`
	SeatType string `json:"seat_type"` // WINDOW | AISLE | MIDDLE | BERTH_LOWER | BERTH_UPPER
	X        int    `json:"x"`
	Y        int    `json:"y"`
}

// SeatMapResponse is served exactly as the allocator published it.
type SeatMapResponse = wire.SeatMap
type SeatMapPool = wire.SeatMapPool

// ---- waiting room (§12)

type AdmissionRequest struct {
	JourneyID int64 `json:"journey_id"`
}

type AdmissionResponse struct {
	Admitted       bool       `json:"admitted"`
	AdmissionToken string     `json:"admission_token,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	Position       int        `json:"position,omitempty"`
}

// ---- seat selection (§13)

type SelectRequest struct {
	FromIdx   int    `json:"from_idx"`
	ToIdx     int    `json:"to_idx"`
	ClassCode string `json:"class_code"`
	Quota     string `json:"quota,omitempty"` // default "ONLINE"
}

type CartResponse struct {
	JourneyID      int64      `json:"journey_id"`
	FromIdx        int        `json:"from_idx"`
	ToIdx          int        `json:"to_idx"`
	ClassCode      string     `json:"class_code"`
	Seats          []SeatRef  `json:"seats"`      // the whole cart after this click
	MaxSeats       int        `json:"max_seats"`  // 4 minus seats already held/bought
	ExpiresAt      *time.Time `json:"expires_at"` // locks released at this time if no hold
	SeatmapVersion int64      `json:"seatmap_version"`
}

// ---- passenger details and OTP (§14)

type CreateHoldRequest struct {
	JourneyID  int64         `json:"journey_id"`
	Passengers []PassengerIn `json:"passengers"` // one per seat in the cart, in cart order
}

type OTPInfo = otp.Info

type HoldResponse struct {
	RequestID  string          `json:"request_id"`
	PNR        string          `json:"pnr"`
	Status     string          `json:"status"` // "PENDING_OTP"
	JourneyID  int64           `json:"journey_id"`
	TrainName  string          `json:"train_name"`
	From       StopRef         `json:"from"`
	To         StopRef         `json:"to"`
	ClassCode  string          `json:"class_code"`
	Passengers []PassengerSeat `json:"passengers"` // status HELD; seats exactly as locked
	Fare       Money           `json:"fare"`
	Charge     Money           `json:"service_charge"`
	Amount     Money           `json:"amount"`     // total to pay
	ExpiresAt  time.Time       `json:"expires_at"` // OTP + payment before this
	OTP        OTPInfo         `json:"otp"`
}

type VerifyOTPRequest struct {
	OTP string `json:"otp"` // 6 digits
}

type VerifyOTPResponse struct {
	PNR       string    `json:"pnr"`
	Status    string    `json:"status"`     // "PENDING_PAYMENT"
	ExpiresAt time.Time `json:"expires_at"` // hold expiry (unchanged)
}

// ---- request results (§15)

type AcceptedResponse struct {
	RequestID string `json:"request_id"`
	PollURL   string `json:"poll_url"`
	StreamURL string `json:"stream_url"`
}

type RequestStatus struct {
	RequestID  string          `json:"request_id"`
	State      string          `json:"state"`                 // PENDING | DONE
	StatusCode int             `json:"status_code,omitempty"` // HTTP status the original call would have had
	Body       json.RawMessage `json:"body,omitempty"`        // CartResponse | HoldResponse | CancelResponse | ErrorResponse
}

// ---- payments (§16)

type CreatePaymentRequest struct {
	PNR       string `json:"pnr"`
	Method    string `json:"method"`     // BKASH | NAGAD | ROCKET | UPAY | VISA | MASTERCARD | NEXUS
	ReturnURL string `json:"return_url"` // allow-listed app origin
}

type PaymentResponse struct {
	PaymentID     string     `json:"payment_id"`
	PNR           string     `json:"pnr"`
	Status        string     `json:"status"` // INITIATED | CAPTURED | FAILED | REFUNDED
	Method        string     `json:"method"`
	Amount        Money      `json:"amount"`
	RedirectURL   string     `json:"redirect_url,omitempty"` // usable once
	HoldExpires   time.Time  `json:"hold_expires_at"`
	CapturedAt    *time.Time `json:"captured_at,omitempty"`
	BookingStatus string     `json:"booking_status,omitempty"`
}

// ---- bookings (§17)

type BookingResponse struct {
	PNR           string          `json:"pnr"`
	Status        string          `json:"status"` // PENDING_OTP | PENDING_PAYMENT | CONFIRMED | PARTIALLY_CANCELLED | CANCELLED | EXPIRED | CONFIRM_FAILED
	JourneyID     int64           `json:"journey_id"`
	TrainNo       string          `json:"train_no"`
	TrainName     string          `json:"train_name"`
	RunDate       string          `json:"run_date"`
	From          StopRef         `json:"from"`
	To            StopRef         `json:"to"`
	ClassCode     string          `json:"class_code"`
	Passengers    []PassengerSeat `json:"passengers"`
	Amount        Money           `json:"amount"`
	HoldExpiresAt *time.Time      `json:"hold_expires_at,omitempty"`
	Payment       *PaymentBrief   `json:"payment,omitempty"`
	Refunds       []RefundBrief   `json:"refunds,omitempty"`
	SeatsChanged  bool            `json:"seats_changed,omitempty"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type PaymentBrief struct {
	PaymentID string `json:"payment_id"`
	Status    string `json:"status"`
	Method    string `json:"method"`
	Amount    Money  `json:"amount"`
}

type RefundBrief struct {
	RefundID string `json:"refund_id"`
	Reason   string `json:"reason"`
	Amount   Money  `json:"amount"`
	Status   string `json:"status"`
}

type BookingSummary struct {
	PNR     string    `json:"pnr"`
	Status  string    `json:"status"`
	TrainNo string    `json:"train_no"`
	RunDate string    `json:"run_date"`
	From    StopRef   `json:"from"`
	To      StopRef   `json:"to"`
	Seats   []SeatRef `json:"seats"`
}

type CancelRequest struct {
	PassengerSeqs []int  `json:"passenger_seqs,omitempty"` // empty = whole PNR
	Reason        string `json:"reason,omitempty"`
}

type CancelResponse struct {
	RequestID      string          `json:"request_id"`
	PNR            string          `json:"pnr"`
	CancelID       string          `json:"cancel_id"`
	CancelledSeqs  []int           `json:"cancelled_seqs"`
	BookingStatus  string          `json:"booking_status"`
	Passengers     []PassengerSeat `json:"passengers"`
	RefundEstimate Money           `json:"refund_estimate"` // from the cancellation rules
	RefundStatus   string          `json:"refund_status"`   // PENDING (issued asynchronously)
}

// ---- internal (§18)

type OpenRequest struct {
	Pools []struct {
		ClassCode string `json:"class_code"`
		Quota     string `json:"quota"`
	} `json:"pools"`
}

// VerifyTicketRequest carries the scanned e-ticket QR payload.
type VerifyTicketRequest struct {
	QR string `json:"qr"`
}

// VerifyTicketResponse is the gate check of a genuine ticket. Valid is true
// when at least one seat on the ticket is still ISSUED.
type VerifyTicketResponse struct {
	Valid         bool            `json:"valid"`
	PNR           string          `json:"pnr"`
	BookingStatus string          `json:"booking_status"`
	TrainNo       string          `json:"train_no"`
	TrainName     string          `json:"train_name"`
	RunDate       string          `json:"run_date"`
	From          StopRef         `json:"from"`
	To            StopRef         `json:"to"`
	ClassCode     string          `json:"class_code"`
	Passengers    []PassengerSeat `json:"passengers"`
}
