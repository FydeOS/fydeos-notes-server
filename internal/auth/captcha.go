// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"context"
	"crypto/sha3"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/google/uuid"
)

const captchaChallengeTTL = 5 * time.Minute

var (
	turnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	captchaHTTPClient  = &http.Client{Timeout: 10 * time.Second}
	captchaActionRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
)

type CaptchaService struct {
	db  PasswordLoginDatabase
	cfg config.Config
}

func NewCaptchaService(db PasswordLoginDatabase, cfg config.Config) *CaptchaService {
	return &CaptchaService{db: db, cfg: cfg}
}

type captchaSettings struct {
	enabled bool
	secret  string
	siteKey string
	action  string
	bits    int
}

func (s *CaptchaService) settings(ctx context.Context) (captchaSettings, error) {
	out := captchaSettings{action: "auth-sign-in", bits: 20}
	enabled, err := readConfigBool(ctx, s.db, s.cfg, "captcha.enabled", false)
	if err != nil {
		return out, errCaptchaConfigInvalid
	}
	out.enabled = enabled
	raw, found, err := config.ReadRuntimeValue(ctx, s.db, s.cfg.RuntimeConfigPath, "captcha.config")
	if err != nil {
		return out, errCaptchaConfigInvalid
	}
	if found {
		var stored struct {
			Turnstile *struct {
				Secret  *string `json:"secret"`
				SiteKey *string `json:"siteKey"`
				Action  *string `json:"action"`
			} `json:"turnstile"`
			Challenge *struct {
				Bits *int `json:"bits"`
			} `json:"challenge"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&stored); err != nil {
			return out, errCaptchaConfigInvalid
		}
		if t := stored.Turnstile; t != nil {
			if t.Secret != nil {
				out.secret = *t.Secret
			}
			if t.SiteKey != nil {
				out.siteKey = *t.SiteKey
			}
			if t.Action != nil {
				out.action = *t.Action
			}
		}
		if c := stored.Challenge; c != nil && c.Bits != nil {
			out.bits = *c.Bits
		}
	}
	if out.bits < 16 || out.bits > 30 || !captchaActionRe.MatchString(out.action) || len(out.secret) > 4096 || len(out.siteKey) > 256 {
		return out, errCaptchaConfigInvalid
	}
	if out.enabled && (out.secret == "" || out.siteKey == "") {
		return out, errCaptchaConfigInvalid
	}
	return out, nil
}

func singleHeader(r *http.Request, name string) (string, bool) {
	values := r.Header.Values(name)
	if len(values) > 1 {
		return "", false
	}
	if len(values) == 0 {
		return "", true
	}
	return values[0], true
}

// Verify checks the captcha headers of a sign-in request.
func (s *CaptchaService) Verify(ctx context.Context, r *http.Request) error {
	settings, err := s.settings(ctx)
	if err != nil {
		return err
	}
	if !settings.enabled {
		return nil
	}
	token, ok1 := singleHeader(r, "x-captcha-token")
	provider, ok2 := singleHeader(r, "x-captcha-provider")
	challenge, ok3 := singleHeader(r, "x-captcha-challenge")
	if !ok1 || !ok2 || !ok3 || token == "" || provider == "" || len(token) > 2048 {
		return errCaptchaCredential
	}
	_, hasChallenge := r.Header[http.CanonicalHeaderKey("x-captcha-challenge")]
	switch provider {
	case "hashcash":
		if challenge == "" || len(challenge) > 128 {
			return errCaptchaCredential
		}
		return s.verifyHashcash(ctx, settings, challenge, token)
	case "turnstile":
		if hasChallenge {
			return errCaptchaCredential
		}
		return s.verifyTurnstile(ctx, settings, r, token)
	default:
		return errCaptchaCredential
	}
}

func (s *CaptchaService) verifyHashcash(ctx context.Context, settings captchaSettings, challenge, stamp string) error {
	var raw []byte
	err := s.db.QueryRow(ctx, `UPDATE runtime_states SET consumed_at=clock_timestamp(), updated_at=clock_timestamp()
		WHERE purpose=$1 AND token_hash=$2 AND consumed_at IS NULL AND expires_at > clock_timestamp() RETURNING payload`,
		purposeCaptcha, sha256hex(challenge)).Scan(&raw)
	if err != nil {
		return errCaptchaRejected
	}
	var payload struct {
		Resource string `json:"resource"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Resource == "" {
		return errCaptchaRejected
	}
	if !validStamp(stamp, payload.Resource, settings.bits, time.Now()) {
		return errCaptchaRejected
	}
	return nil
}

func validStamp(stamp, resource string, minBits int, now time.Time) bool {
	parts := strings.Split(stamp, ":")
	if len(parts) != 7 || parts[0] != "1" {
		return false
	}
	for i, p := range parts {
		if p == "" && i != 4 {
			return false
		}
	}
	bits, err := strconv.Atoi(parts[1])
	if err != nil || bits < minBits || bits > 256 {
		return false
	}
	issued, err := time.Parse("20060102150405", parts[2])
	if err != nil || now.Sub(issued) > captchaChallengeTTL || issued.Sub(now) > captchaChallengeTTL {
		return false
	}
	if parts[3] != resource {
		return false
	}
	sum := sha3.Sum256([]byte(stamp))
	digest := hex.EncodeToString(sum[:])
	return strings.HasPrefix(digest, strings.Repeat("0", bits/4))
}

func (s *CaptchaService) verifyTurnstile(ctx context.Context, settings captchaSettings, r *http.Request, token string) error {
	form := url.Values{"secret": {settings.secret}, "response": {token}, "idempotency_key": {uuid.NewString()}}
	if ip := requestClientIP(r, s.cfg); ip != "" {
		form.Set("remoteip", ip)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, turnstileVerifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return errCaptchaUnavailable
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := captchaHTTPClient.Do(req)
	if err != nil {
		return errCaptchaUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errCaptchaUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || len(body) >= 64<<10 {
		return errCaptchaUnavailable
	}
	var result struct {
		Success  bool   `json:"success"`
		Action   string `json:"action"`
		Hostname string `json:"hostname"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return errCaptchaUnavailable
	}
	if !result.Success || result.Action != settings.action {
		return errCaptchaRejected
	}
	if s.cfg.NodeEnv != "development" && s.cfg.NodeEnv != "test" {
		ok := false
		for _, h := range configuredHosts(s.cfg) {
			if result.Hostname == h {
				ok = true
			}
		}
		if !ok {
			return errCaptchaRejected
		}
	}
	return nil
}

func captchaAPIError(err error) apiError {
	switch {
	case errors.Is(err, errCaptchaCredential):
		return apiCaptchaHeaders
	case errors.Is(err, errCaptchaRejected):
		return apiCaptchaRejected
	case errors.Is(err, errCaptchaUnavailable):
		return apiCaptchaDown
	default:
		return apiInternal
	}
}

type CaptchaHTTPHandler struct {
	service *CaptchaService
	limiter PasswordLoginLimiter
}

func NewCaptchaHTTPHandler(service *CaptchaService, limiter PasswordLoginLimiter) *CaptchaHTTPHandler {
	if limiter == nil {
		limiter = newMemoryLimiter()
	}
	return &CaptchaHTTPHandler{service: service, limiter: limiter}
}

func (h *CaptchaHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setNoStore(w, false)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, apiMethodNotAllowed)
		return
	}
	ctx := r.Context()
	if !h.limiter.AllowStrict(ctx, requestClientIP(r, h.service.cfg), time.Now()) {
		writeAPIError(w, apiTooManyRequests)
		return
	}
	settings, err := h.service.settings(ctx)
	if err != nil {
		writeAPIError(w, apiInternal)
		return
	}
	if r.Header.Get("x-affine-client-kind") == "native" {
		challenge, resource := uuid.NewString(), uuid.NewString()
		tx, err := h.service.db.Begin(ctx)
		if err != nil {
			writeAPIError(w, apiInternal)
			return
		}
		defer rollback(tx)
		now, err := dbNow(ctx, tx)
		if err == nil {
			err = insertState(ctx, tx, purposeCaptcha, sha256hex(challenge), &challenge, map[string]string{"resource": resource}, now.Add(captchaChallengeTTL))
		}
		if err != nil || tx.Commit(ctx) != nil {
			writeAPIError(w, apiInternal)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"provider": "hashcash", "challenge": challenge, "resource": resource})
		return
	}
	out := map[string]any{"provider": "turnstile"}
	if settings.siteKey != "" {
		out["siteKey"] = settings.siteKey
	}
	if settings.action != "" {
		out["action"] = settings.action
	}
	writeJSON(w, http.StatusOK, out)
}
