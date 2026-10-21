package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"

	"railway/internal/platform"
	"railway/internal/platform/bloom"
	"railway/internal/wire"
)

const (
	Issuer   = "railway-auth"
	Audience = "railway-api"
)

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrTokenExpired    = errors.New("token expired")
)

type AccessClaims struct {
	jwt.RegisteredClaims        // iss, aud, sub = user_id, iat, exp (+15 min), jti (UUIDv7)
	Role                 string `json:"role"` // passenger | agent | counter | admin
	Verified             bool   `json:"vfd"`  // identity verified (tatkal-style quotas)
	DeviceID             string `json:"did,omitempty"`
}

// UserID parses the subject.
func (c *AccessClaims) UserID() int64 {
	id, _ := strconv.ParseInt(c.Subject, 10, 64)
	return id
}

// ---- keys

// LoadECKey reads an ES256 private key (SEC1 or PKCS#8 PEM) from a file.
func LoadECKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseECKey(b)
}

func ParseECKey(b []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("no PEM block")
	}
	if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an EC key")
	}
	return ec, nil
}

// ParseECPublic accepts a public key PEM, a private key PEM, or a path to either.
func ParseECPublic(s string) (*ecdsa.PublicKey, error) {
	b := []byte(s)
	if !strings.Contains(s, "-----BEGIN") {
		var err error
		if b, err = os.ReadFile(s); err != nil {
			return nil, err
		}
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("no PEM block")
	}
	if pk, err := x509.ParsePKIXPublicKey(blk.Bytes); err == nil {
		if ec, ok := pk.(*ecdsa.PublicKey); ok {
			return ec, nil
		}
	}
	priv, err := ParseECKey(b)
	if err != nil {
		return nil, err
	}
	return &priv.PublicKey, nil
}

// GenerateKeyFile writes a new P-256 key if the file does not exist yet.
func GenerateKeyFile(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, err
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}

// ---- signing

type Signer struct {
	kid string
	key *ecdsa.PrivateKey
	ttl time.Duration
}

func NewSigner(kid string, key *ecdsa.PrivateKey, ttl time.Duration) *Signer {
	return &Signer{kid: kid, key: key, ttl: ttl}
}

func (s *Signer) Sign(userID int64, role string, verified bool, deviceID string, now time.Time) (string, AccessClaims, error) {
	c := AccessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: Issuer, Audience: jwt.ClaimStrings{Audience}, Subject: strconv.FormatInt(userID, 10),
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl)), ID: platform.NewRequestID(),
		},
		Role: role, Verified: verified, DeviceID: deviceID,
	}
	t := jwt.NewWithClaims(jwt.SigningMethodES256, c)
	t.Header["kid"] = s.kid
	str, err := t.SignedString(s.key)
	return str, c, err
}

// JWKS returns the public key set.
func (s *Signer) JWKS() []byte { return jwksJSON(map[string]*ecdsa.PublicKey{s.kid: &s.key.PublicKey}) }

// JWKSFor renders any set of P-256 public keys as a JWKS document.
func JWKSFor(keys map[string]*ecdsa.PublicKey) []byte { return jwksJSON(keys) }

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func jwksJSON(keys map[string]*ecdsa.PublicKey) []byte {
	var out struct {
		Keys []jwk `json:"keys"`
	}
	for kid, k := range keys {
		raw, err := k.Bytes() // 0x04 || X || Y
		if err != nil {
			continue
		}
		x, y := raw[1:33], raw[33:]
		out.Keys = append(out.Keys, jwk{Kty: "EC", Crv: "P-256", Kid: kid, Use: "sig", Alg: "ES256",
			X: base64.RawURLEncoding.EncodeToString(x), Y: base64.RawURLEncoding.EncodeToString(y)})
	}
	b, _ := json.Marshal(out)
	return b
}

func parseJWKS(b []byte) (map[string]*ecdsa.PublicKey, error) {
	var in struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, err
	}
	out := map[string]*ecdsa.PublicKey{}
	for _, k := range in.Keys {
		if k.Kty != "EC" || k.Crv != "P-256" {
			continue
		}
		x, err1 := base64.RawURLEncoding.DecodeString(k.X)
		y, err2 := base64.RawURLEncoding.DecodeString(k.Y)
		if err1 != nil || err2 != nil {
			continue
		}
		if len(x) != 32 || len(y) != 32 {
			continue
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
		if err != nil {
			continue // not a point on the curve
		}
		out[k.Kid] = pub
	}
	return out, nil
}

// ---- verification

// Verifier checks access tokens with no I/O on the happy path: keys come from an
// in-memory JWKS, revocation from a pod-local Bloom filter of revoked jtis
// ("maybe" falls through to Redis).
type Verifier struct {
	mu     sync.RWMutex
	keys   map[string]*ecdsa.PublicKey
	rdb    redis.UniversalClient
	deny   *bloom.Filter
	log    *slog.Logger
	parser *jwt.Parser
}

func NewVerifier(rdb redis.UniversalClient, log *slog.Logger) *Verifier {
	return &Verifier{
		keys: map[string]*ecdsa.PublicKey{}, rdb: rdb, log: log,
		deny: bloom.New(1_000_000, 0.005),
		parser: jwt.NewParser(jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuer(Issuer),
			jwt.WithAudience(Audience), jwt.WithLeeway(30*time.Second), jwt.WithExpirationRequired()),
	}
}

func (v *Verifier) AddKey(kid string, k *ecdsa.PublicKey) {
	v.mu.Lock()
	v.keys[kid] = k
	v.mu.Unlock()
}

func (v *Verifier) HasKeys() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.keys) > 0
}

// RunJWKS refreshes keys from url every 5 minutes.
func (v *Verifier) RunJWKS(ctx context.Context, url string) {
	fetch := func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			v.log.Warn("jwks fetch failed", "err", err)
			return
		}
		defer resp.Body.Close()
		var b []byte
		if b, err = readAll(resp); err != nil {
			return
		}
		keys, err := parseJWKS(b)
		if err != nil || len(keys) == 0 {
			v.log.Warn("jwks parse failed", "err", err)
			return
		}
		v.mu.Lock()
		for kid, k := range keys {
			v.keys[kid] = k
		}
		v.mu.Unlock()
	}
	fetch()
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fetch()
		}
	}
}

// RunDenylist seeds the Bloom filter from Redis and follows the jwt:deny channel.
func (v *Verifier) RunDenylist(ctx context.Context) {
	iter := v.rdb.Scan(ctx, 0, "jwt:deny:*", 1000).Iterator()
	for iter.Next(ctx) {
		v.deny.Add(strings.TrimPrefix(iter.Val(), "jwt:deny:"))
	}
	sub := v.rdb.Subscribe(ctx, wire.JWTDenyChannel)
	defer sub.Close()
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-ch:
			if !ok {
				return
			}
			v.deny.Add(m.Payload)
		}
	}
}

func (v *Verifier) Verify(ctx context.Context, bearer string) (*AccessClaims, error) {
	var c AccessClaims
	_, err := v.parser.ParseWithClaims(bearer, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		v.mu.RLock()
		k := v.keys[kid]
		v.mu.RUnlock()
		if k == nil {
			return nil, fmt.Errorf("unknown kid %q", kid)
		}
		return k, nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, ErrUnauthenticated
	}
	if v.deny.MayContain(c.ID) {
		n, err := v.rdb.Exists(ctx, wire.JWTDenyKey(c.ID)).Result()
		if err == nil && n > 0 {
			return nil, ErrUnauthenticated
		}
	}
	return &c, nil
}

// Deny revokes an access token until it expires.
func Deny(ctx context.Context, rdb redis.UniversalClient, jti string, exp time.Time) error {
	if jti == "" || time.Until(exp) <= 0 {
		return nil
	}
	if err := rdb.SetArgs(ctx, wire.JWTDenyKey(jti), "1", redis.SetArgs{ExpireAt: exp}).Err(); err != nil {
		return err
	}
	return rdb.Publish(ctx, wire.JWTDenyChannel, jti).Err()
}

func readAll(resp *http.Response) ([]byte, error) {
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
