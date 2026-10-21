// Package ticketsig signs and verifies the QR payload printed on e-tickets, so a
// gate scanner can tell a genuine ticket from a forged one, offline, with the
// public key from GET /v1/tickets/keys.
//
// Payload (one line, '|'-separated, ES256 over everything before the last '|'):
//
//	RAILBD2|<pnr>|<train_no>|<run_date>|<FROM>-<TO>|<seat,seat,...>|<kid>|<base64url(r||s)>
package ticketsig

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"math/big"
	"strings"
)

const version = "RAILBD2"

var (
	ErrMalformed    = errors.New("malformed ticket payload")
	ErrUnknownKey   = errors.New("unknown ticket key")
	ErrBadSignature = errors.New("bad ticket signature")
)

// Payload is what the QR code asserts about a booking.
type Payload struct {
	PNR     string
	TrainNo string
	RunDate string // YYYY-MM-DD
	From    string // station code
	To      string // station code
	Seats   []string
}

func (p Payload) message(kid string) string {
	return strings.Join([]string{version, p.PNR, p.TrainNo, p.RunDate, p.From + "-" + p.To,
		strings.Join(p.Seats, ","), kid}, "|")
}

type Signer struct {
	kid string
	key *ecdsa.PrivateKey
}

func NewSigner(kid string, key *ecdsa.PrivateKey) *Signer { return &Signer{kid: kid, key: key} }

// Kid and Public identify the key for verifiers.
func (s *Signer) Kid() string              { return s.kid }
func (s *Signer) Public() *ecdsa.PublicKey { return &s.key.PublicKey }
func (s *Signer) Verifier() *Verifier {
	return NewVerifier(map[string]*ecdsa.PublicKey{s.kid: s.Public()})
}

// Sign returns the full QR payload.
func (s *Signer) Sign(p Payload) (string, error) {
	msg := p.message(s.kid)
	h := sha256.Sum256([]byte(msg))
	r, sv, err := ecdsa.Sign(rand.Reader, s.key, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sv.FillBytes(sig[32:])
	return msg + "|" + base64.RawURLEncoding.EncodeToString(sig), nil
}

type Verifier struct {
	keys map[string]*ecdsa.PublicKey
}

func NewVerifier(keys map[string]*ecdsa.PublicKey) *Verifier { return &Verifier{keys: keys} }

// Verify checks the signature and returns what the payload asserts.
func (v *Verifier) Verify(qr string) (Payload, error) {
	f := strings.Split(strings.TrimSpace(qr), "|")
	if len(f) != 8 || f[0] != version {
		return Payload{}, ErrMalformed
	}
	route := strings.SplitN(f[4], "-", 2)
	if len(route) != 2 || f[1] == "" {
		return Payload{}, ErrMalformed
	}
	p := Payload{PNR: f[1], TrainNo: f[2], RunDate: f[3], From: route[0], To: route[1]}
	if f[5] != "" {
		p.Seats = strings.Split(f[5], ",")
	}
	key, ok := v.keys[f[6]]
	if !ok {
		return Payload{}, ErrUnknownKey
	}
	sig, err := base64.RawURLEncoding.DecodeString(f[7])
	if err != nil || len(sig) != 64 {
		return Payload{}, ErrBadSignature
	}
	h := sha256.Sum256([]byte(strings.Join(f[:7], "|")))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(key, h[:], r, s) {
		return Payload{}, ErrBadSignature
	}
	return p, nil
}
