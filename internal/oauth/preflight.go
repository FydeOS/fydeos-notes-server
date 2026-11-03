// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/oauth2"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

const (
	statePurpose      = "auth_challenge:oauth_state"
	stateTTL          = 3 * time.Hour
	maxPreflightBody  = 16 << 10
	maxClientNonceLen = 512
	pkceVerifierBytes = 96
)

var allowedClients = map[string]bool{"web": true, "affine": true, "affine-canary": true, "affine-beta": true, "affine-dev": true}

// PreflightInput is a validated-on-use sign-in request.
type PreflightInput struct {
	Provider      string
	RedirectURI   *string
	Client        string
	ClientNonce   string
	ClientVersion *string
}

// stateEnvelope is the JSON sent as the provider `state` parameter.
type stateEnvelope struct {
	State    string     `json:"state"`
	Provider string     `json:"provider"`
	Client   string     `json:"client"`
	PKCE     *pkceProof `json:"pkce,omitempty"`
}

type pkceProof struct {
	CodeChallenge       string `json:"codeChallenge"`
	CodeChallengeMethod string `json:"codeChallengeMethod"`
}

// statePayload is persisted in runtime_states.payload.
type statePayload struct {
	Provider      string  `json:"provider"`
	ProviderLabel string  `json:"providerLabel"`
	RedirectURI   *string `json:"redirectUri"`
	Client        string  `json:"client"`
	ClientNonce   string  `json:"clientNonce"`
	ClientVersion *string `json:"clientVersion"`
	CallbackURL   string  `json:"callbackUrl"`
	PKCEVerifier  *string `json:"pkceVerifier"`
}

type stateWriter interface {
	SaveOAuthState(context.Context, string, []byte, int) error
}

// Service builds authorization URLs and persists one-time states.
type Service struct {
	source OAuthProviderLoader
	states stateWriter
}

func NewServiceWithOAuthProviderSource(source OAuthProviderLoader, states stateWriter) *Service {
	return &Service{source: source, states: states}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Service) Preflight(ctx context.Context, input PreflightInput, callbackURL, redirectBaseURL string, allowedOrigins []string) (string, error) {
	if input.Provider == "" || input.Client == "" || input.ClientNonce == "" || !allowedClients[input.Client] {
		return "", ErrInvalidInput
	}
	if n := utf16Len(input.ClientNonce); n < 1 || n > maxClientNonceLen {
		return "", ErrInvalidInput
	}
	name, _, ok := canonicalProvider(input.Provider)
	if !ok {
		return "", ErrUnknownProvider
	}
	if name == providerFydeOS && input.Client != "web" {
		return "", ErrInvalidInput
	}
	var redirect *string
	if input.RedirectURI != nil {
		resolved, err := resolveRedirect(*input.RedirectURI, redirectBaseURL, allowedOrigins)
		if err != nil {
			return "", err
		}
		redirect = &resolved
	}
	provider, err := s.source.LoadProvider(ctx, name)
	if err != nil {
		return "", err
	}
	if !provider.Enabled {
		return "", ErrProviderDisabled
	}
	applyDefaultEndpoints(&provider)

	token := uuid.NewString()
	envelope := stateEnvelope{State: token, Provider: provider.Label, Client: input.Client}
	var verifier *string
	if name == providerOIDC {
		buf := make([]byte, pkceVerifierBytes)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		v := base64.RawURLEncoding.EncodeToString(buf)
		verifier = &v
		envelope.PKCE = &pkceProof{CodeChallenge: pkceChallenge(v), CodeChallengeMethod: "S256"}
	}
	stateParam, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(statePayload{
		Provider: name, ProviderLabel: provider.Label, RedirectURI: redirect, Client: input.Client,
		ClientNonce: input.ClientNonce, ClientVersion: input.ClientVersion, CallbackURL: callbackURL, PKCEVerifier: verifier,
	})
	if err != nil {
		return "", err
	}
	if err := s.states.SaveOAuthState(ctx, sha256Hex(token), payload, int(stateTTL/time.Second)); err != nil {
		return "", fmt.Errorf("save oauth state: %w", err)
	}

	conf := oauth2Config(provider, callbackURL)
	var opts []oauth2.AuthCodeOption
	for k, v := range provider.Args {
		if (name == providerApple && (k == "privateKey" || k == "keyId" || k == "teamId")) || (name == providerOIDC && strings.HasPrefix(k, "claim_")) {
			continue
		}
		opts = append(opts, oauth2.SetAuthURLParam(k, v))
	}
	switch name {
	case providerFydeOS:
		opts = append(opts, oauth2.SetAuthURLParam("protocol", "oauth2"))
	case providerGoogle:
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "select_account"), oauth2.AccessTypeOffline)
	case providerApple:
		opts = append(opts, oauth2.SetAuthURLParam("response_mode", "form_post"), oauth2.SetAuthURLParam("nonce", input.ClientNonce))
	case providerOIDC:
		opts = append(opts, oauth2.SetAuthURLParam("nonce", token), oauth2.S256ChallengeOption(*verifier))
	}
	return conf.AuthCodeURL(string(stateParam), opts...), nil
}

func providerScopes(name string) []string {
	switch name {
	case providerFydeOS:
		return []string{"profile"}
	case providerGoogle:
		return []string{"openid", "email", "profile"}
	case providerGitHub:
		return []string{"read:user", "user:email"}
	case providerApple:
		return []string{"name", "email"}
	default:
		return []string{"openid", "profile", "email"}
	}
}

func oauth2Config(p OAuthProviderConfig, callbackURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: p.ClientSecret,
		RedirectURL:  callbackURL,
		Scopes:       providerScopes(p.Name),
		Endpoint:     oauth2.Endpoint{AuthURL: p.AuthorizationURL, TokenURL: p.TokenURL, AuthStyle: oauth2.AuthStyleInParams},
	}
}

// StateStore persists OAuth states in runtime_states.
type StateStore struct {
	db interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	}
}

func NewStateStore(db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}) *StateStore {
	return &StateStore{db: db}
}

func (s *StateStore) SaveOAuthState(ctx context.Context, hash string, payload []byte, ttlSeconds int) error {
	_, err := s.db.Exec(ctx, `
WITH purged AS (
  DELETE FROM runtime_states
  WHERE ctid IN (
    SELECT ctid FROM runtime_states
    WHERE purpose = $1 AND expires_at <= now()
    LIMIT 100 FOR UPDATE SKIP LOCKED))
INSERT INTO runtime_states (purpose, token_hash, lookup_key, payload, expires_at)
VALUES ($1, $2, NULL, $3::jsonb, now() + make_interval(secs => $4))`,
		statePurpose, hash, string(payload), ttlSeconds)
	return err
}

// PreflightHandler serves POST /api/oauth/preflight.
type PreflightHandler struct {
	service *Service
	cfg     config.Config
	limiter RateLimiter
}

func NewPreflightHandlerWithLimiter(service *Service, cfg config.Config, limiter RateLimiter) *PreflightHandler {
	if limiter == nil {
		limiter = newMemoryLimiter()
	}
	return &PreflightHandler{service: service, cfg: cfg, limiter: limiter}
}

type preflightBody struct {
	Provider    *string `json:"provider"`
	Client      *string `json:"client"`
	ClientNonce *string `json:"client_nonce"`
	RedirectURI *string `json:"redirect_uri"`
}

func preflightError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func (h *PreflightHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		preflightError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !applyRateLimit(w, r, h.cfg, h.limiter) {
		preflightError(w, http.StatusTooManyRequests, "too_many_requests")
		return
	}
	body, err := readBody(w, r, maxPreflightBody)
	if errors.Is(err, errBodyTooLarge) {
		preflightError(w, http.StatusRequestEntityTooLarge, "request_body_too_large")
		return
	}
	invalid := func() { preflightError(w, http.StatusBadRequest, "invalid_oauth_preflight") }
	if err != nil || !mediaTypeIs(r, "application/json") || !isObjectJSON(body) {
		invalid()
		return
	}
	version := r.Header.Get(versionHeader)
	if len(version) > maxVersionHeaderLen {
		invalid()
		return
	}
	var req preflightBody
	if err := decodeStrictJSON(body, &req); err != nil || req.Provider == nil || req.Client == nil || req.ClientNonce == nil {
		invalid()
		return
	}
	input := PreflightInput{Provider: *req.Provider, Client: *req.Client, ClientNonce: *req.ClientNonce, RedirectURI: req.RedirectURI}
	if version != "" {
		input.ClientVersion = &version
	}
	origin := requestOrigin(r, h.cfg)
	callbackPath := "/oauth/callback"
	if input.Provider == "Apple" {
		callbackPath = "/api/oauth/callback"
	}
	authURL, err := h.service.Preflight(r.Context(), input, origin+callbackPath, origin, allowedRedirectOrigins(h.cfg))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"url": authURL})
	case errors.Is(err, ErrInvalidInput):
		invalid()
	case errors.Is(err, ErrUnknownProvider), errors.Is(err, ErrProviderDisabled):
		preflightError(w, http.StatusBadRequest, "unknown_oauth_provider")
	case errors.Is(err, ErrRedirectNotAllowed):
		preflightError(w, http.StatusForbidden, "action_forbidden")
	default:
		preflightError(w, http.StatusServiceUnavailable, "oauth_temporarily_unavailable")
	}
}
