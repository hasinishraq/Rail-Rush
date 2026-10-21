package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"railway/internal/api/dto"
)

const turnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

var turnstileClient = &http.Client{Timeout: 5 * time.Second}

// captchaEnabled reports whether register and login require a Turnstile token.
func (s *Server) captchaEnabled() bool { return s.cfg.TurnstileSecret != "" }

// GET /v1/auth/captcha (Public): what the client must render before register/login.
func (s *Server) captchaConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	if !s.captchaEnabled() {
		writeJSON(w, http.StatusOK, dto.CaptchaConfig{})
		return
	}
	writeJSON(w, http.StatusOK, dto.CaptchaConfig{Provider: "turnstile", SiteKey: s.cfg.TurnstileSiteKey})
}

// requireCaptcha verifies a Turnstile token with Cloudflare. It writes the error
// response and returns false when the token is missing or rejected; it always
// passes while Turnstile is not configured. Tokens are single-use.
func (s *Server) requireCaptcha(w http.ResponseWriter, r *http.Request, token string) bool {
	if !s.captchaEnabled() {
		return true
	}
	if token == "" {
		fail(w, r, "CAPTCHA_INVALID", "captcha required", map[string]any{"captcha_required": true})
		return false
	}
	ok, codes, err := s.verifyTurnstile(r.Context(), token, clientIP(r, s.cfg.TrustProxy))
	if err != nil {
		// Fail closed: a bot wave during a Cloudflare outage must not get through.
		s.log.Warn("turnstile verify", "err", err)
		fail(w, r, "UNAVAILABLE", "captcha verification unavailable; try again", nil)
		return false
	}
	if !ok {
		fail(w, r, "CAPTCHA_INVALID", "captcha failed; try again", map[string]any{"captcha_required": true, "error_codes": codes})
		return false
	}
	return true
}

func (s *Server) verifyTurnstile(ctx context.Context, token, ip string) (bool, []string, error) {
	form := url.Values{"secret": {s.cfg.TurnstileSecret}, "response": {token}}
	if ip != "" {
		form.Set("remoteip", ip)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, turnstileVerifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return false, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := turnstileClient.Do(req)
	if err != nil {
		return false, nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Success    bool     `json:"success"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, nil, err
	}
	// internal-error is Cloudflare's own failure, not a bad token.
	for _, c := range out.ErrorCodes {
		if c == "internal-error" {
			return false, out.ErrorCodes, errTurnstileInternal
		}
	}
	return out.Success, out.ErrorCodes, nil
}

var errTurnstileInternal = errors.New("turnstile internal-error")
