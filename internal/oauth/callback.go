// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
)

const (
	maxCallbackBody    = 64 << 10
	maxCodeLen         = 4096
	maxStateLen        = 16384
	maxAppleUserLen    = 16384
	defaultSessionTTL  = 1296000
	appleCallbackRoute = "/api/oauth/callback"
)

type callbackDB interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Begin(context.Context) (pgx.Tx, error)
}

// CallbackService completes an OAuth sign-in.
type CallbackService struct {
	db     callbackDB
	source OAuthProviderLoader
	cfg    config.Config
	// clientFor builds the outbound client for a provider; replaceable in tests.
	clientFor func(OAuthProviderConfig) *http.Client
}

func NewCallbackServiceWithOAuthProviderSourceAndConfig(db callbackDB, source OAuthProviderLoader, cfg config.Config) *CallbackService {
	return &CallbackService{db: db, source: source, cfg: cfg, clientFor: func(p OAuthProviderConfig) *http.Client {
		return newRestrictedClient(providerEgress(p))
	}}
}

type callbackInput struct {
	Code          string
	State         string
	ClientNonce   *string
	AppleUser     string
	ClientVersion *string
	SessionID     string
	Native        bool
}

type callbackResult struct {
	HandoffURL    string
	UserID        string
	ExchangeCode  string
	RedirectURI   *string
	SessionID     string
	ExpiresAt     time.Time
	AppleWebFlow  bool
	clearSessions bool
}

// parseCallbackState accepts a bare UUID or the JSON envelope.
func parseCallbackState(raw string) (string, *stateEnvelope, error) {
	if len(raw) == 36 {
		if _, err := uuid.Parse(raw); err == nil {
			return strings.ToLower(raw), nil, nil
		}
	}
	var env stateEnvelope
	if err := decodeStrictJSON([]byte(raw), &env); err != nil {
		return "", nil, ErrInvalidCallbackState
	}
	if len(env.State) != 36 {
		return "", nil, ErrInvalidCallbackState
	}
	if _, err := uuid.Parse(env.State); err != nil {
		return "", nil, ErrInvalidCallbackState
	}
	return strings.ToLower(env.State), &env, nil
}

func validStoredPayload(p statePayload) bool {
	name, label, ok := canonicalProvider(p.Provider)
	if !ok || name != p.Provider || label != p.ProviderLabel || !allowedClients[p.Client] {
		return false
	}
	if name == providerFydeOS && p.Client != "web" {
		return false
	}
	u, err := url.Parse(p.CallbackURL)
	return err == nil && u.IsAbs() && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

func (s *CallbackService) handle(ctx context.Context, in callbackInput) (callbackResult, error) {
	token, env, err := parseCallbackState(in.State)
	if err != nil {
		return callbackResult{}, err
	}
	hash := sha256Hex(token)
	var raw []byte
	err = s.db.QueryRow(ctx, `SELECT payload FROM runtime_states
		WHERE purpose = $1 AND token_hash = $2 AND consumed_at IS NULL AND expires_at > now()`, statePurpose, hash).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return callbackResult{}, ErrOAuthStateExpired
	}
	if err != nil {
		return callbackResult{}, err
	}
	var stored statePayload
	if err := json.Unmarshal(raw, &stored); err != nil || !validStoredPayload(stored) {
		return callbackResult{}, ErrInvalidCallbackState
	}
	if env != nil && (env.Provider != stored.ProviderLabel || env.Client != stored.Client) {
		return callbackResult{}, ErrInvalidCallbackState
	}
	if stored.Provider == providerOIDC {
		if env == nil || env.PKCE == nil || stored.PKCEVerifier == nil || *stored.PKCEVerifier == "" ||
			env.PKCE.CodeChallengeMethod != "S256" || env.PKCE.CodeChallenge != pkceChallenge(*stored.PKCEVerifier) {
			return callbackResult{}, ErrInvalidCallbackState
		}
	} else if env != nil && env.PKCE != nil {
		return callbackResult{}, ErrInvalidCallbackState
	}
	if stored.Provider == providerApple && stored.Client != "web" && env != nil {
		return callbackResult{HandoffURL: s.appleHandoffURL(in, stored)}, nil
	}
	if stored.Provider != providerApple && (in.ClientNonce == nil || *in.ClientNonce != stored.ClientNonce) {
		return callbackResult{}, ErrInvalidAuthState
	}
	tag, err := s.db.Exec(ctx, `UPDATE runtime_states SET consumed_at = now(), updated_at = now()
		WHERE purpose = $1 AND token_hash = $2 AND consumed_at IS NULL AND expires_at > now()`, statePurpose, hash)
	if err != nil {
		return callbackResult{}, err
	}
	if tag.RowsAffected() != 1 {
		return callbackResult{}, ErrOAuthStateExpired
	}
	provider, err := s.source.LoadProvider(ctx, stored.Provider)
	if errors.Is(err, ErrProviderDisabled) || errors.Is(err, ErrUnknownProvider) || (err == nil && (!provider.Enabled || provider.Name != stored.Provider)) {
		return callbackResult{}, ErrUnknownProvider
	}
	if err != nil {
		return callbackResult{}, err
	}
	applyDefaultEndpoints(&provider)
	profile, err := exchangeProfile(ctx, exchangeRequest{
		provider: provider, code: in.Code, state: stored, stateToken: token, appleUser: in.AppleUser,
		client: s.clientFor(provider), now: time.Now(), callbackURL: stored.CallbackURL,
	})
	if err != nil {
		return callbackResult{}, err
	}
	settings, err := s.readAuthSettings(ctx)
	if err != nil {
		return callbackResult{}, err
	}
	result, err := s.signIn(ctx, provider, profile, stored, in, settings)
	if err != nil {
		return callbackResult{}, err
	}
	result.RedirectURI = stored.RedirectURI
	result.AppleWebFlow = provider.Name == providerApple && stored.Client == "web"
	return result, nil
}

func (s *CallbackService) appleHandoffURL(in callbackInput, stored statePayload) string {
	payload, _ := json.Marshal(map[string]string{"state": in.State, "code": in.Code, "provider": stored.ProviderLabel})
	origin := config.PublicOrigin(s.cfg)
	query := url.Values{"method": {"oauth"}, "payload": {string(payload)}, "server": {origin}}
	clientURL := stored.Client + "://authentication?" + query.Encode()
	base := strings.TrimRight(s.cfg.Path, "/")
	if base != "" && !strings.HasPrefix(base, "/") {
		base = "/" + base
	}
	return origin + base + "/open-app/url?url=" + url.QueryEscape(clientURL)
}

type authSettings struct {
	allowSignup bool
	sessionTTL  time.Duration
}

func (s *CallbackService) readAuthSettings(ctx context.Context) (authSettings, error) {
	settings := authSettings{allowSignup: true, sessionTTL: defaultSessionTTL * time.Second}
	raw, found, err := config.ReadRuntimeValue(ctx, s.db, s.cfg.RuntimeConfigPath, "auth.allowSignupForOauth")
	if err != nil {
		return settings, err
	}
	if found {
		if err := strictUnmarshal(raw, &settings.allowSignup); err != nil {
			return settings, errors.New("invalid runtime configuration auth.allowSignupForOauth")
		}
	}
	raw, found, err = config.ReadRuntimeValue(ctx, s.db, s.cfg.RuntimeConfigPath, "auth.session.ttl")
	if err != nil {
		return settings, err
	}
	if found {
		var ttl float64
		if err := strictUnmarshal(raw, &ttl); err != nil || ttl <= 0 || ttl != math.Trunc(ttl) || ttl > float64(math.MaxInt64/int64(time.Second)) {
			return settings, errors.New("invalid runtime configuration auth.session.ttl")
		}
		settings.sessionTTL = time.Duration(ttl) * time.Second
	}
	return settings, nil
}

// signIn resolves the account and issues a session in one transaction.
func (s *CallbackService) signIn(ctx context.Context, provider OAuthProviderConfig, profile providerProfile, stored statePayload, in callbackInput, settings authSettings) (callbackResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return callbackResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return callbackResult{}, err
	}
	user, err := resolveAccount(ctx, tx, provider, profile, now, settings.allowSignup)
	if err != nil {
		return callbackResult{}, err
	}
	result := callbackResult{UserID: user.id}
	if in.Native {
		var disabled bool
		var epoch int
		if err := tx.QueryRow(ctx, `SELECT disabled, auth_epoch FROM users WHERE id = $1 FOR UPDATE`, user.id).Scan(&disabled, &epoch); err != nil {
			return callbackResult{}, err
		}
		if disabled {
			return callbackResult{}, ErrWrongSignInCredentials
		}
		code, err := auth.IssueNativeExchangeCode(ctx, tx, user.id, epoch, in.ClientVersion, now)
		if err != nil {
			return callbackResult{}, err
		}
		result.ExchangeCode = code
		result.clearSessions = true
	} else {
		sessionID, err := ensureMultiUserSession(ctx, tx, in.SessionID, now)
		if err != nil {
			return callbackResult{}, err
		}
		expires := now.Add(settings.sessionTTL)
		if _, err := tx.Exec(ctx, `INSERT INTO user_sessions (id, session_id, user_id, expires_at, sign_in_client_version, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (session_id, user_id) DO UPDATE
			SET expires_at = excluded.expires_at,
			    sign_in_client_version = COALESCE(excluded.sign_in_client_version, user_sessions.sign_in_client_version)`,
			uuid.NewString(), sessionID, user.id, expires, in.ClientVersion, now); err != nil {
			return callbackResult{}, err
		}
		result.SessionID = sessionID
		result.ExpiresAt = expires
	}
	if err := tx.Commit(ctx); err != nil {
		return callbackResult{}, err
	}
	return result, nil
}

func ensureMultiUserSession(ctx context.Context, tx pgx.Tx, cookieID string, now time.Time) (string, error) {
	if parsed, err := uuid.Parse(cookieID); err == nil && parsed.String() == strings.ToLower(cookieID) {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM multiple_users_sessions WHERE id = $1)`, cookieID).Scan(&exists); err != nil {
			return "", err
		}
		if exists {
			return cookieID, nil
		}
	}
	id := uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO multiple_users_sessions (id, created_at) VALUES ($1, $2)`, id, now); err != nil {
		return "", err
	}
	return id, nil
}

// CallbackHandler serves the OAuth callback routes.
type CallbackHandler struct {
	service *CallbackService
	cfg     config.Config
	limiter RateLimiter
}

func NewCallbackHandler(service *CallbackService, cfg config.Config) *CallbackHandler {
	return &CallbackHandler{service: service, cfg: cfg, limiter: newMemoryLimiter()}
}

type problem struct {
	Status  int            `json:"status"`
	Code    string         `json:"code"`
	Type    string         `json:"type"`
	Name    string         `json:"name"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

func writeProblem(w http.ResponseWriter, p problem) {
	p.Code = http.StatusText(p.Status)
	writeJSON(w, p.Status, p)
}

var (
	problemMissingCode  = problem{Status: 400, Type: "BAD_REQUEST", Name: "MISSING_OAUTH_QUERY_PARAMETER", Message: "Missing query parameter `code`.", Data: map[string]any{"name": "code"}}
	problemMissingState = problem{Status: 400, Type: "BAD_REQUEST", Name: "MISSING_OAUTH_QUERY_PARAMETER", Message: "Missing query parameter `state`.", Data: map[string]any{"name": "state"}}
	problemTooLarge     = problem{Status: 413, Type: "BAD_REQUEST", Name: "BAD_REQUEST", Message: "Bad request."}
	problemRateLimited  = problem{Status: 429, Type: "TOO_MANY_REQUESTS", Name: "TOO_MANY_REQUEST", Message: "Too many requests."}
	problemInternal     = problem{Status: 500, Type: "INTERNAL_SERVER_ERROR", Name: "INTERNAL_SERVER_ERROR", Message: "An internal error occurred."}
)

func invalidResponseProblem(reason string) problem {
	return problem{Status: 400, Type: "BAD_REQUEST", Name: "INVALID_OAUTH_RESPONSE", Message: "Invalid OAuth response: " + reason + ".", Data: map[string]any{"reason": reason}}
}

func problemFor(err error) problem {
	var status *providerStatusError
	switch {
	case errors.As(err, &status):
		return problem{Status: 400, Type: "BAD_REQUEST", Name: "INVALID_OAUTH_CALLBACK_CODE",
			Message: fmt.Sprintf("Invalid callback code parameter, provider response status: %d and body: .", status.status),
			Data:    map[string]any{"status": status.status, "body": ""}}
	case errors.Is(err, ErrInvalidCallbackState), errors.Is(err, ErrInvalidCallback):
		return problem{Status: 400, Type: "BAD_REQUEST", Name: "INVALID_OAUTH_CALLBACK_STATE", Message: "Invalid callback state parameter."}
	case errors.Is(err, ErrInvalidAuthState):
		return problem{Status: 400, Type: "BAD_REQUEST", Name: "INVALID_AUTH_STATE", Message: "Invalid auth state. You might start the auth progress from another device."}
	case errors.Is(err, ErrOAuthStateExpired):
		return problem{Status: 400, Type: "BAD_REQUEST", Name: "OAUTH_STATE_EXPIRED", Message: "OAuth state expired, please try again."}
	case errors.Is(err, ErrUnknownProvider), errors.Is(err, ErrProviderDisabled):
		return problem{Status: 400, Type: "INVALID_INPUT", Name: "UNKNOWN_OAUTH_PROVIDER", Message: "Unknown authentication provider FydeOS.", Data: map[string]any{"name": "FydeOS"}}
	case errors.Is(err, ErrOAuthSignupForbidden):
		return problem{Status: 403, Type: "ACTION_FORBIDDEN", Name: "SIGN_UP_FORBIDDEN", Message: "You are not allowed to sign up."}
	case errors.Is(err, ErrOAuthAccountAlreadyLinked):
		return problem{Status: 400, Type: "BAD_REQUEST", Name: "OAUTH_ACCOUNT_ALREADY_CONNECTED", Message: "The third-party account has already been connected to another user."}
	case errors.Is(err, ErrWrongSignInCredentials):
		return invalidResponseProblem("This account cannot sign in.")
	case errors.Is(err, ErrInvalidOAuthResponse), errors.Is(err, ErrInvalidInput):
		return invalidResponseProblem("Invalid OAuth response.")
	}
	return problemInternal
}

type callbackBody struct {
	Code        *string `json:"code"`
	State       *string `json:"state"`
	ClientNonce *string `json:"client_nonce"`
	User        *string `json:"user"`
}

func (h *CallbackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeProblem(w, problem{Status: 405, Type: "BAD_REQUEST", Name: "BAD_REQUEST", Message: "Bad request."})
		return
	}
	if !applyRateLimit(w, r, h.cfg, h.limiter) {
		writeProblem(w, problemRateLimited)
		return
	}
	version := r.Header.Get(versionHeader)
	body, err := readBody(w, r, maxCallbackBody)
	if errors.Is(err, errBodyTooLarge) {
		writeProblem(w, problemTooLarge)
		return
	}
	if err != nil || len(version) > maxVersionHeaderLen {
		writeProblem(w, problemMissingCode)
		return
	}
	var req callbackBody
	switch {
	case mediaTypeIs(r, "application/json"):
		if !isObjectJSON(body) || decodeStrictJSON(body, &req) != nil {
			writeProblem(w, problemMissingState)
			return
		}
	case mediaTypeIs(r, "application/x-www-form-urlencoded") && strings.HasSuffix(r.URL.Path, appleCallbackRoute):
		values, err := url.ParseQuery(string(body))
		if err != nil || len(values) > 3 {
			writeProblem(w, problemMissingState)
			return
		}
		for key, vals := range values {
			if len(vals) != 1 {
				writeProblem(w, problemMissingState)
				return
			}
			v := vals[0]
			switch key {
			case "code":
				req.Code = &v
			case "state":
				req.State = &v
			case "user":
				req.User = &v
			default:
				writeProblem(w, problemMissingState)
				return
			}
		}
	default:
		writeProblem(w, problemMissingCode)
		return
	}
	if req.Code == nil || len(*req.Code) < 1 || len(*req.Code) > maxCodeLen {
		writeProblem(w, problemMissingCode)
		return
	}
	if req.State == nil || len(*req.State) < 1 || len(*req.State) > maxStateLen ||
		(req.ClientNonce != nil && (utf16Len(*req.ClientNonce) < 1 || utf16Len(*req.ClientNonce) > maxClientNonceLen)) ||
		(req.User != nil && len(*req.User) > maxAppleUserLen) {
		writeProblem(w, problemMissingState)
		return
	}
	in := callbackInput{Code: *req.Code, State: *req.State, ClientNonce: req.ClientNonce, Native: auth.IsNativeClientRequest(r)}
	if req.User != nil {
		in.AppleUser = *req.User
	}
	if version != "" {
		in.ClientVersion = &version
	}
	if cookie, err := r.Cookie(auth.SessionCookieName); err == nil {
		in.SessionID = cookie.Value
	}
	result, err := h.service.handle(r.Context(), in)
	if err != nil {
		writeProblem(w, problemFor(err))
		return
	}
	if result.HandoffURL != "" {
		http.Redirect(w, r, result.HandoffURL, http.StatusFound)
		return
	}
	if result.clearSessions {
		auth.ClearSessionCookies(w)
		writeJSON(w, http.StatusOK, successBody{ID: result.UserID, ExchangeCode: result.ExchangeCode, RedirectURI: result.RedirectURI})
		return
	}
	for _, c := range []*http.Cookie{
		{Name: auth.SessionCookieName, Value: result.SessionID, HttpOnly: true, Secure: h.cfg.HTTPS},
		{Name: auth.UserCookieName, Value: result.UserID},
		{Name: auth.CSRFCookieName, Value: uuid.NewString(), Secure: h.cfg.HTTPS},
	} {
		c.Path = "/"
		c.SameSite = http.SameSiteLaxMode
		c.Expires = result.ExpiresAt
		http.SetCookie(w, c)
	}
	if result.AppleWebFlow {
		target := "/"
		if result.RedirectURI != nil {
			target = *result.RedirectURI
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	writeJSON(w, http.StatusOK, successBody{ID: result.UserID, RedirectURI: result.RedirectURI})
}

type successBody struct {
	ID           string  `json:"id"`
	ExchangeCode string  `json:"exchangeCode,omitempty"`
	RedirectURI  *string `json:"redirectUri,omitempty"`
}
