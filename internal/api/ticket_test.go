package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
	"time"

	"rsc.io/qr"

	"railway/internal/api/dto"
	"railway/internal/ticketsig"
)

func TestSignedTicketQR(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sig := ticketsig.NewSigner("t1", key)
	b := &dto.BookingResponse{PNR: "6910567903", Status: "CONFIRMED", TrainNo: "742", TrainName: "Turna", RunDate: "2026-10-21",
		From: dto.StopRef{StationCode: "BAN"}, To: dto.StopRef{StationCode: "CML"}, ClassCode: "S_CHAIR"}
	for i, id := range []string{"CHA-37", "CHA-38", "CHA-39", "CHA-40"} {
		b.Passengers = append(b.Passengers, dto.PassengerSeat{Seq: i + 1, Name: "Pax", Status: "ISSUED", Seat: dto.SeatRef{SeatID: id}})
	}
	b.Passengers[3].Status = "CANCELLED"

	payload, err := qrPayload(sig, b)
	if err != nil {
		t.Fatal(err)
	}
	p, err := sig.Verifier().Verify(payload)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(p.Seats) != 3 {
		t.Fatalf("want the 3 issued seats, got %v", p.Seats)
	}
	if _, err := qr.Encode(payload, qr.M); err != nil {
		t.Fatalf("payload does not fit a QR code: %v", err)
	}
	if _, err := renderTicket(b, payload, time.Now()); err != nil {
		t.Fatalf("render: %v", err)
	}
}
