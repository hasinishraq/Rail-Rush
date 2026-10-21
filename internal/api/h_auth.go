package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"railway/internal/api/dto"
	"railway/internal/auth"
)

func tokenResponse(p *auth.TokenPair) dto.TokenResponse {
	return dto.TokenResponse{AccessToken: p.AccessToken, TokenType: "Bearer", ExpiresIn: p.ExpiresIn,
		RefreshToken: p.RefreshToken, RefreshExpiresIn: p.RefreshExpiresIn,
		User: dto.UserInfo{UserID: p.User.UserID, Name: p.User.Name, Phone: p.User.Phone, Verified: p.User.Verified}}
}

// POST /v1/auth/register (Public, Idempotency-Key)
func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if !s.ipLimit(w, r, "register", 20) {
		return
	}
	key, ok := requireIdemKey(w, r)
	if !ok {
		return
	}
	var req dto.RegisterRequest
	if _, ok := decode(w, r, &req); !ok {
		return
	}
	if !s.requireCaptcha(w, r, req.CaptchaToken) {
		return
	}
	// Tokens are single-use, so a retry carries a new one: keep it out of the fingerprint.
	req.CaptchaToken = ""
	body, _ := json.Marshal(req)
	s.idempotent(w, r, 0, "POST /v1/auth/register", key, body, func() outcome {
		id, err := s.authSvc.Register(r.Context(), strings.TrimSpace(req.Phone), req.Email, req.Name, req.NID, req.Password)
		var fe auth.FieldErrors
		if errors.As(err, &fe) {
			return errOutcome(r, "VALIDATION_FAILED", "invalid fields", map[string]any{"fields": map[string]string(fe)})
		}
		if err != nil {
			s.log.Error("register", "err", err)
			return errOutcome(r, "INTERNAL", "internal error", nil)
		}
		return outcome{http.StatusCreated, dto.RegisterResponse{UserID: id}}
	})
}

// POST /v1/auth/login (Public): rate-limit IP and account, verify, new refresh family.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.ipLimit(w, r, "login", 20) {
		return
	}
	var req dto.LoginRequest
	if _, ok := decode(w, r, &req); !ok {
		return
	}
	if !s.requireCaptcha(w, r, req.CaptchaToken) {
		return
	}
	account := strings.ToLower(strings.TrimSpace(req.Login))
	failKey := "rl:loginfail:" + account
	// With Turnstile on, every attempt is already challenged; without it, lock the account for 15 min.
	if n, _ := s.rdb.Get(r.Context(), failKey).Int(); n >= 5 && !s.captchaEnabled() {
		fail(w, r, "RATE_LIMITED", "too many failed logins; try again later", map[string]any{"retry_after": 900})
		return
	}
	pair, err := s.authSvc.Login(r.Context(), req.Login, req.Password, req.DeviceID)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		pipe := s.rdb.Pipeline()
		pipe.Incr(r.Context(), failKey)
		pipe.Expire(r.Context(), failKey, 15*time.Minute)
		_, _ = pipe.Exec(r.Context())
		fail(w, r, "INVALID_CREDENTIALS", "invalid login or password", nil)
		return
	}
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	s.rdb.Del(r.Context(), failKey)
	writeJSON(w, http.StatusOK, tokenResponse(pair))
}

// POST /v1/auth/refresh (Public, refresh token)
func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	if !s.ipLimit(w, r, "refresh", 60) {
		return
	}
	var req dto.RefreshRequest
	if _, ok := decode(w, r, &req); !ok {
		return
	}
	pair, err := s.authSvc.Refresh(r.Context(), req.RefreshToken, req.DeviceID)
	switch {
	case errors.Is(err, auth.ErrRefreshReused):
		fail(w, r, "REFRESH_REUSED", "refresh token reused; all sessions of this login revoked", nil)
	case errors.Is(err, auth.ErrUnauthenticated):
		fail(w, r, "UNAUTHENTICATED", "invalid or expired refresh token", nil)
	case err != nil:
		internalErr(w, r, s, err)
	default:
		writeJSON(w, http.StatusOK, tokenResponse(pair))
	}
}

// POST /v1/auth/logout (User)
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	var req dto.LogoutRequest
	if _, ok := decode(w, r, &req); !ok {
		return
	}
	c := claims(r)
	if err := s.authSvc.Logout(r.Context(), c.UserID(), req.RefreshToken, c.ID, c.ExpiresAt.Time); err != nil {
		internalErr(w, r, s, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /.well-known/jwks.json
func (s *Server) jwks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeRaw(w, http.StatusOK, s.signer.JWKS())
}

// GET /v1/me
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	c := claims(r)
	var out dto.MeResponse
	var phone, email *string
	err := s.readDB.QueryRow(r.Context(), `SELECT user_id, name, verified, phone, email, role FROM app_user WHERE user_id = $1`, c.UserID()).
		Scan(&out.UserID, &out.Name, &out.Verified, &phone, &email, &out.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		fail(w, r, "NOT_FOUND", "user not found", nil)
		return
	}
	if err != nil {
		internalErr(w, r, s, err)
		return
	}
	if phone != nil {
		out.Phone = *phone
	}
	if email != nil {
		out.Email = *email
	}
	writeJSON(w, http.StatusOK, out)
}
