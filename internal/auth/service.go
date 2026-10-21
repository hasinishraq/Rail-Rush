// Package auth implements register, login, refresh (rotating refresh tokens with
// reuse detection), logout, JWKS and access-token verification (§10).
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"railway/internal/platform/metrics"
	"railway/internal/platform/pg"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrRefreshReused      = errors.New("refresh token reused")
	ErrValidation         = errors.New("validation failed")
)

// FieldErrors carries per-field validation reasons.
type FieldErrors map[string]string

func (f FieldErrors) Error() string { return "validation failed" }

type User struct {
	UserID   int64
	Phone    string
	Email    string
	Name     string
	Hash     string
	Verified bool
	Role     string
	Status   string
}

type TokenPair struct {
	AccessToken      string
	ExpiresIn        int
	RefreshToken     string
	RefreshExpiresIn int
	User             User
}

type Service struct {
	DB         *pgxpool.Pool // primary
	ReadDB     *pgxpool.Pool // replica
	RDB        redis.UniversalClient
	Hasher     *Hasher
	Signer     *Signer
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	Now        func() time.Time
	NIDKey     []byte // HMAC key: the NID is stored only as HMAC(NIDKey, nid)
}

var (
	bdPhone = regexp.MustCompile(`^\+8801[3-9][0-9]{8}$`)
	nidRe   = regexp.MustCompile(`^([0-9]{10}|[0-9]{13}|[0-9]{17})$`)
	email   = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// NIDHash is the stored form of a national ID number.
func (s *Service) NIDHash(nid string) []byte {
	m := hmac.New(sha256.New, s.NIDKey)
	m.Write([]byte(nid))
	return m.Sum(nil)
}

// Register creates a user. One account per phone number and per NID.
// Validation problems come back as FieldErrors.
func (s *Service) Register(ctx context.Context, phone, mail, name, nid, password string) (int64, error) {
	fe := FieldErrors{}
	if !bdPhone.MatchString(phone) {
		fe["phone"] = "must be +8801XXXXXXXXX"
	}
	mail = strings.TrimSpace(mail)
	if mail != "" && !email.MatchString(mail) {
		fe["email"] = "invalid"
	}
	if n := len([]rune(strings.TrimSpace(name))); n < 1 || n > 100 {
		fe["name"] = "1..100 characters"
	}
	nid = strings.TrimSpace(nid)
	if nid != "" && !nidRe.MatchString(nid) {
		fe["nid"] = "10, 13 or 17 digits"
	}
	if err := CheckPassword(password); err != nil {
		fe["password"] = strings.TrimPrefix(err.Error(), ErrWeakPassword.Error()+": ")
	}
	if len(fe) > 0 {
		return 0, fe
	}
	hash, err := s.Hasher.Hash(password)
	if err != nil {
		return 0, err
	}
	var emailArg, nidArg any
	if mail != "" {
		emailArg = mail
	}
	if nid != "" {
		nidArg = s.NIDHash(nid)
	}
	var id int64
	err = s.DB.QueryRow(ctx, `INSERT INTO app_user (phone, email, name, nid_hash, password_hash) VALUES ($1, $2, $3, $4, $5) RETURNING user_id`,
		phone, emailArg, strings.TrimSpace(name), nidArg, hash).Scan(&id)
	if pg.IsUniqueViolation(err) {
		switch c := pg.ConstraintName(err); {
		case strings.Contains(c, "email"):
			return 0, FieldErrors{"email": "taken"}
		case strings.Contains(c, "nid"):
			return 0, FieldErrors{"nid": "taken"}
		}
		return 0, FieldErrors{"phone": "taken"}
	}
	return id, err
}

// ByLogin finds a user by phone or e-mail on the read replica.
func (s *Service) ByLogin(ctx context.Context, login string) (*User, error) {
	login = strings.TrimSpace(login)
	q := `SELECT user_id, COALESCE(phone,''), COALESCE(email,''), name, password_hash, verified, role, status FROM app_user WHERE `
	if strings.HasPrefix(login, "+") {
		q += `phone = $1`
	} else {
		q += `lower(email) = lower($1)`
	}
	u := &User{}
	err := s.ReadDB.QueryRow(ctx, q, login).Scan(&u.UserID, &u.Phone, &u.Email, &u.Name, &u.Hash, &u.Verified, &u.Role, &u.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

func (s *Service) UserByID(ctx context.Context, id int64) (*User, error) {
	u := &User{}
	err := s.ReadDB.QueryRow(ctx, `SELECT user_id, COALESCE(phone,''), COALESCE(email,''), name, verified, role, status
	                                 FROM app_user WHERE user_id = $1`, id).
		Scan(&u.UserID, &u.Phone, &u.Email, &u.Name, &u.Verified, &u.Role, &u.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

// Login verifies the password (a dummy verify for unknown users) and starts a
// new refresh-token family.
func (s *Service) Login(ctx context.Context, login, password, deviceID string) (*TokenPair, error) {
	u, err := s.ByLogin(ctx, login)
	if err != nil {
		return nil, err
	}
	if u == nil {
		s.Hasher.VerifyDummy(password)
		return nil, ErrInvalidCredentials
	}
	if !s.Hasher.Verify(password, u.Hash) || u.Status != "ACTIVE" {
		return nil, ErrInvalidCredentials
	}
	tok, hash, err := newRefreshToken()
	if err != nil {
		return nil, err
	}
	now := s.Now()
	if _, err := s.DB.Exec(ctx, `INSERT INTO refresh_token (token_hash, family_id, user_id, device_id, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		hash, uuid.New(), u.UserID, nullable(deviceID), now.Add(s.RefreshTTL)); err != nil {
		return nil, err
	}
	return s.pair(u, tok, deviceID, now)
}

func (s *Service) pair(u *User, refresh, deviceID string, now time.Time) (*TokenPair, error) {
	at, _, err := s.Signer.Sign(u.UserID, u.Role, u.Verified, deviceID, now)
	if err != nil {
		return nil, err
	}
	return &TokenPair{AccessToken: at, ExpiresIn: int(s.AccessTTL.Seconds()), RefreshToken: refresh,
		RefreshExpiresIn: int(s.RefreshTTL.Seconds()), User: *u}, nil
}

// Refresh rotates a refresh token in one transaction. Replaying a used token
// revokes the whole family.
func (s *Service) Refresh(ctx context.Context, token, deviceID string) (*TokenPair, error) {
	hash := hashToken(token)
	now := s.Now()
	var out *TokenPair
	var reused bool
	err := pg.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var family uuid.UUID
		var userID int64
		var expires time.Time
		var used, revoked *time.Time
		err := tx.QueryRow(ctx, `SELECT family_id, user_id, expires_at, used_at, revoked_at FROM refresh_token WHERE token_hash = $1 FOR UPDATE`, hash).
			Scan(&family, &userID, &expires, &used, &revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnauthenticated
		}
		if err != nil {
			return err
		}
		if revoked != nil || now.After(expires) {
			return ErrUnauthenticated
		}
		if used != nil {
			if _, err := tx.Exec(ctx, `UPDATE refresh_token SET revoked_at = now() WHERE family_id = $1 AND revoked_at IS NULL`, family); err != nil {
				return err
			}
			reused = true
			return nil // commit the revocation
		}
		u := &User{}
		if err := tx.QueryRow(ctx, `SELECT user_id, COALESCE(phone,''), COALESCE(email,''), name, verified, role, status FROM app_user WHERE user_id = $1`, userID).
			Scan(&u.UserID, &u.Phone, &u.Email, &u.Name, &u.Verified, &u.Role, &u.Status); err != nil {
			return err
		}
		if u.Status != "ACTIVE" {
			return ErrUnauthenticated
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_token SET used_at = now() WHERE token_hash = $1`, hash); err != nil {
			return err
		}
		tok, nh, err := newRefreshToken()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO refresh_token (token_hash, family_id, user_id, device_id, expires_at) VALUES ($1, $2, $3, $4, $5)`,
			nh, family, userID, nullable(deviceID), now.Add(s.RefreshTTL)); err != nil {
			return err
		}
		out, err = s.pair(u, tok, deviceID, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	if reused {
		metrics.RefreshReused.Inc()
		return nil, ErrRefreshReused
	}
	return out, nil
}

// Logout revokes the refresh family (if the token belongs to userID) and the
// current access token.
func (s *Service) Logout(ctx context.Context, userID int64, refreshToken, accessJTI string, accessExp time.Time) error {
	if refreshToken != "" {
		if _, err := s.DB.Exec(ctx, `UPDATE refresh_token SET revoked_at = now()
		     WHERE family_id = (SELECT family_id FROM refresh_token WHERE token_hash = $1 AND user_id = $2) AND revoked_at IS NULL`,
			hashToken(refreshToken), userID); err != nil {
			return err
		}
	}
	return Deny(ctx, s.RDB, accessJTI, accessExp)
}

func newRefreshToken() (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	return tok, hashToken(tok), nil
}

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
