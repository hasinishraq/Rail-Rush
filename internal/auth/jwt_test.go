package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
)

func TestJWKSRoundTrip(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := parseJWKS(JWKSFor(map[string]*ecdsa.PublicKey{"k1": &key.PublicKey}))
	if err != nil {
		t.Fatal(err)
	}
	if got := keys["k1"]; got == nil || !got.Equal(&key.PublicKey) {
		t.Fatalf("round trip lost the key: %v", keys)
	}
	if keys, _ := parseJWKS([]byte(`{"keys":[{"kty":"EC","crv":"P-256","kid":"bad","x":"AQ","y":"AQ"}]}`)); len(keys) != 0 {
		t.Fatalf("accepted a point that is not on the curve: %v", keys)
	}
}
