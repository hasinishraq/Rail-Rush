// Package admission verifies waiting-room admission tokens (§10.2) and, for local
// development only, issues them from a token bucket (§12.2).
package admission

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"

	"railway/internal/platform"
	"railway/internal/wire"
)

const (
	Issuer   = "waiting-room"
	Audience = "railway-booking"
	Scope    = "booking"
	TokenTTL = 15 * time.Minute
)

var (
	ErrRequired = errors.New("admission required")
	ErrInvalid  = errors.New("admission invalid")
)

// UsedError means the token already produced a successful hold.
type UsedError struct{ PNR string }

func (e *UsedError) Error() string { return "admission used for pnr " + e.PNR }

type Claims struct {
	jwt.RegisteredClaims
	Scope string `json:"scope"`
}

type Verifier struct {
	pub    *ecdsa.PublicKey
	rdb    redis.UniversalClient
	parser *jwt.Parser
}

func NewVerifier(pub *ecdsa.PublicKey, rdb redis.UniversalClient) *Verifier {
	return &Verifier{pub: pub, rdb: rdb, parser: jwt.NewParser(jwt.WithValidMethods([]string{"ES256"}),
		jwt.WithIssuer(Issuer), jwt.WithAudience(Audience), jwt.WithExpirationRequired(), jwt.WithLeeway(5*time.Second))}
}

// Verify checks signature and expiry and binds the token to userID on first
// use (§10.2). Selects do not consume a token; CheckUnused is for holds.
func (v *Verifier) Verify(ctx context.Context, token string, userID int64) (*Claims, error) {
	if token == "" {
		return nil, ErrRequired
	}
	var c Claims
	if _, err := v.parser.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return v.pub, nil }); err != nil || c.Scope != Scope || c.ID == "" {
		return nil, ErrInvalid
	}
	exp := c.ExpiresAt.Time
	uid := strconv.FormatInt(userID, 10)
	ok, err := v.rdb.SetArgs(ctx, wire.AdmitKey(c.ID), uid, redis.SetArgs{Mode: "NX", ExpireAt: exp}).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	if ok != "OK" {
		bound, err := v.rdb.Get(ctx, wire.AdmitKey(c.ID)).Result()
		if err != nil || bound != uid {
			return nil, ErrInvalid // a token cannot be shared
		}
	}
	return &c, nil
}

// CheckUnused rejects a token that already produced a successful hold: one
// admission, one booking.
func (v *Verifier) CheckUnused(ctx context.Context, c *Claims) error {
	pnr, err := v.rdb.Get(ctx, wire.AdmitUsedKey(c.ID)).Result()
	if err == nil {
		return &UsedError{PNR: pnr}
	}
	if !errors.Is(err, redis.Nil) {
		return err
	}
	return nil
}

// MarkUsed consumes the token once a hold succeeds: one admission, one booking.
func (v *Verifier) MarkUsed(ctx context.Context, c *Claims, pnr string) error {
	return v.rdb.SetArgs(ctx, wire.AdmitUsedKey(c.ID), pnr, redis.SetArgs{ExpireAt: c.ExpiresAt.Time}).Err()
}

// LocalIssuer stands in for the bought waiting room in Docker Compose. A token
// bucket shared through Redis refills at rate per second. Never deployed to production.
type LocalIssuer struct {
	key  *ecdsa.PrivateKey
	rdb  redis.UniversalClient
	rate float64
}

func NewLocalIssuer(key *ecdsa.PrivateKey, rdb redis.UniversalClient, rate float64) *LocalIssuer {
	return &LocalIssuer{key: key, rdb: rdb, rate: rate}
}

var bucket = redis.NewScript(`
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local t = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(t[1]) or burst
local ts = tonumber(t[2]) or now
tokens = math.min(burst, tokens + (now - ts) / 1000 * rate)
local ok = 0
if tokens >= 1 then tokens = tokens - 1; ok = 1 end
redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('PEXPIRE', KEYS[1], 60000)
if ok == 1 then return {1, 0} end
return {0, math.ceil((1 - tokens) / rate * 1000)}
`)

type Result struct {
	Admitted  bool
	Token     string
	ExpiresAt time.Time
	Position  int
	RetryIn   time.Duration
}

// reuseMargin is the least lifetime a token must have left to be handed out again.
const reuseMargin = 2 * time.Minute

func userKey(userID int64) string { return "admission:user:" + strconv.FormatInt(userID, 10) }

// Admit lets a user in. The frontend joins the waiting room by itself on every
// visit to a train, so a user who still holds a valid, unused token gets that
// token back instead of spending another slot of the bucket.
func (l *LocalIssuer) Admit(ctx context.Context, userID int64, now time.Time) (Result, error) {
	if prev, ok := l.current(ctx, userID, now); ok {
		return prev, nil
	}
	res, err := bucket.Run(ctx, l.rdb, []string{"admission:bucket"}, l.rate, l.rate, now.UnixMilli()).Int64Slice()
	if err != nil {
		return Result{}, err
	}
	if res[0] == 0 {
		waiting, _ := l.rdb.Incr(ctx, "admission:waiting").Result()
		l.rdb.Expire(ctx, "admission:waiting", 5*time.Second)
		wait := time.Duration(res[1]) * time.Millisecond
		return Result{Position: int(waiting), RetryIn: max(wait, time.Second)}, nil
	}
	exp := now.Add(TokenTTL)
	c := Claims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer: Issuer, Audience: jwt.ClaimStrings{Audience}, Subject: strconv.FormatInt(userID, 10),
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(exp), ID: platform.NewRequestID(),
	}, Scope: Scope}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodES256, c).SignedString(l.key)
	if err != nil {
		return Result{}, err
	}
	// Best effort: without it the next visit just queues again.
	l.rdb.HSet(ctx, userKey(userID), "token", tok, "jti", c.ID, "exp", exp.UnixMilli())
	l.rdb.ExpireAt(ctx, userKey(userID), exp.Add(-reuseMargin))
	return Result{Admitted: true, Token: tok, ExpiresAt: exp}, nil
}

// current returns the user's last token while it has reuseMargin left and has
// not produced a booking yet (one admission, one booking).
func (l *LocalIssuer) current(ctx context.Context, userID int64, now time.Time) (Result, bool) {
	v, err := l.rdb.HGetAll(ctx, userKey(userID)).Result()
	if err != nil || v["token"] == "" {
		return Result{}, false
	}
	ms, err := strconv.ParseInt(v["exp"], 10, 64)
	exp := time.UnixMilli(ms)
	if err != nil || exp.Sub(now) < reuseMargin {
		return Result{}, false
	}
	if n, err := l.rdb.Exists(ctx, wire.AdmitUsedKey(v["jti"])).Result(); err != nil || n > 0 {
		return Result{}, false
	}
	return Result{Admitted: true, Token: v["token"], ExpiresAt: exp}, true
}
