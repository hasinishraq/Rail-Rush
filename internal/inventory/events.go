package inventory

import (
	"google.golang.org/protobuf/proto"

	bookingv1 "railway/gen/booking/v1"
)

// Kind names the event body, e.g. "SeatSelected".
func Kind(ev *bookingv1.Event) string {
	switch ev.Body.(type) {
	case *bookingv1.Event_SeatSelected:
		return "SeatSelected"
	case *bookingv1.Event_SelectRejected:
		return "SelectRejected"
	case *bookingv1.Event_SeatReleased:
		return "SeatReleased"
	case *bookingv1.Event_CartExpired:
		return "CartExpired"
	case *bookingv1.Event_SeatHeld:
		return "SeatHeld"
	case *bookingv1.Event_HoldRejected:
		return "HoldRejected"
	case *bookingv1.Event_Confirmed:
		return "Confirmed"
	case *bookingv1.Event_ConfirmFailed:
		return "ConfirmFailed"
	case *bookingv1.Event_HoldExpired:
		return "HoldExpired"
	case *bookingv1.Event_DeadlinesExtended:
		return "DeadlinesExtended"
	case *bookingv1.Event_Cancelled:
		return "Cancelled"
	case *bookingv1.Event_CancelRejected:
		return "CancelRejected"
	case *bookingv1.Event_SoldOut:
		return "SoldOut"
	case *bookingv1.Event_Available:
		return "Available"
	case *bookingv1.Event_Warmed:
		return "JourneyWarmed"
	case *bookingv1.Event_Opened:
		return "Opened"
	case *bookingv1.Event_SalesClosed:
		return "SalesClosed"
	}
	return "Unknown"
}

// Body returns the message inside the event's oneof.
func Body(ev *bookingv1.Event) proto.Message {
	switch b := ev.Body.(type) {
	case *bookingv1.Event_SeatSelected:
		return b.SeatSelected
	case *bookingv1.Event_SelectRejected:
		return b.SelectRejected
	case *bookingv1.Event_SeatReleased:
		return b.SeatReleased
	case *bookingv1.Event_CartExpired:
		return b.CartExpired
	case *bookingv1.Event_SeatHeld:
		return b.SeatHeld
	case *bookingv1.Event_HoldRejected:
		return b.HoldRejected
	case *bookingv1.Event_Confirmed:
		return b.Confirmed
	case *bookingv1.Event_ConfirmFailed:
		return b.ConfirmFailed
	case *bookingv1.Event_HoldExpired:
		return b.HoldExpired
	case *bookingv1.Event_DeadlinesExtended:
		return b.DeadlinesExtended
	case *bookingv1.Event_Cancelled:
		return b.Cancelled
	case *bookingv1.Event_CancelRejected:
		return b.CancelRejected
	case *bookingv1.Event_SoldOut:
		return b.SoldOut
	case *bookingv1.Event_Available:
		return b.Available
	case *bookingv1.Event_Warmed:
		return b.Warmed
	case *bookingv1.Event_Opened:
		return b.Opened
	case *bookingv1.Event_SalesClosed:
		return b.SalesClosed
	}
	return nil
}

// PNROf returns the PNR an event is about, if any.
func PNROf(ev *bookingv1.Event) string {
	switch b := ev.Body.(type) {
	case *bookingv1.Event_SeatHeld:
		return b.SeatHeld.Pnr
	case *bookingv1.Event_HoldRejected:
		return b.HoldRejected.Pnr
	case *bookingv1.Event_Confirmed:
		return b.Confirmed.Pnr
	case *bookingv1.Event_ConfirmFailed:
		return b.ConfirmFailed.Pnr
	case *bookingv1.Event_HoldExpired:
		return b.HoldExpired.Pnr
	case *bookingv1.Event_Cancelled:
		return b.Cancelled.Pnr
	case *bookingv1.Event_CancelRejected:
		return b.CancelRejected.Pnr
	}
	return ""
}

// CartUserOf returns the user whose cart an event changes, or 0.
func CartUserOf(ev *bookingv1.Event) int64 {
	switch b := ev.Body.(type) {
	case *bookingv1.Event_SeatSelected:
		return b.SeatSelected.UserId
	case *bookingv1.Event_SeatReleased:
		return b.SeatReleased.UserId
	case *bookingv1.Event_CartExpired:
		return b.CartExpired.UserId
	case *bookingv1.Event_SeatHeld:
		return b.SeatHeld.UserId
	}
	return 0
}

// HoldStatus is the booking status of a PNR in memory (§19.2). PENDING_OTP is
// derived later by the API from booking_otp.
func HoldStatus(h *Hold, state string) string {
	switch state {
	case "HELD":
		return "PENDING_PAYMENT"
	case "EXPIRED":
		return "EXPIRED"
	}
	switch live := h.LiveSeats(); {
	case live == 0:
		return "CANCELLED"
	case live < len(h.Seats):
		return "PARTIALLY_CANCELLED"
	}
	return "CONFIRMED"
}
