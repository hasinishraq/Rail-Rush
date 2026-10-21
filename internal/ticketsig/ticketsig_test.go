package ticketsig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

func newSigner(t *testing.T, kid string) *Signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return NewSigner(kid, k)
}

var sample = Payload{PNR: "6910567903", TrainNo: "742", RunDate: "2026-10-21", From: "BAN", To: "CML", Seats: []string{"CHA-37", "CHA-38"}}

func TestRoundTrip(t *testing.T) {
	s := newSigner(t, "t1")
	qr, err := s.Sign(sample)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Verifier().Verify(qr)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.PNR != sample.PNR || got.From != "BAN" || got.To != "CML" || strings.Join(got.Seats, ",") != "CHA-37,CHA-38" {
		t.Fatalf("payload mismatch: %+v", got)
	}
}

func TestTamperedFieldsFail(t *testing.T) {
	s := newSigner(t, "t1")
	qr, _ := s.Sign(sample)
	for _, tampered := range []string{
		strings.Replace(qr, "CHA-38", "CHA-39", 1),         // extra seat
		strings.Replace(qr, "6910567903", "6910567904", 1), // other PNR
		strings.Replace(qr, "2026-10-21", "2026-10-22", 1), // other date
	} {
		if _, err := s.Verifier().Verify(tampered); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("tampered payload accepted: %v", err)
		}
	}
}

func TestOtherKeyFails(t *testing.T) {
	qr, _ := newSigner(t, "t1").Sign(sample)
	if _, err := newSigner(t, "t1").Verifier().Verify(qr); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("signature from another key accepted: %v", err)
	}
	if _, err := newSigner(t, "t2").Verifier().Verify(qr); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown kid accepted: %v", err)
	}
}

func TestMalformed(t *testing.T) {
	v := newSigner(t, "t1").Verifier()
	for _, qr := range []string{"", "RAILBD1|6910567903|742|2026-10-21|BAN-CML|CHA-37", "RAILBD2|x"} {
		if _, err := v.Verify(qr); !errors.Is(err, ErrMalformed) {
			t.Fatalf("%q: want ErrMalformed, got %v", qr, err)
		}
	}
}
