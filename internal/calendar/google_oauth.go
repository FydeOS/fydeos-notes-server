// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package calendar

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const calendarOAuthStatePurpose = "calendar_oauth_state"
const calendarOAuthStateTTL = 3 * time.Hour
const maxGoogleCalendarResponseBytes = 16 << 20
const maxCalendarOAuthCallbackBytes = 16 << 10

type GoogleEndpoints struct {
	AuthorizationURL string
	TokenURL         string
	UserInfoURL      string
	CalendarListURL  string
	EventsURL        string
	ChannelsStopURL  string
}

type googleCalendarConfig struct {
	Enabled                  bool   `json:"enabled"`
	AllowNewAccounts         *bool  `json:"allowNewAccounts"`
	ClientID                 string `json:"clientId"`
	ClientSecret             string `json:"clientSecret"`
	ExternalWebhookURL       string `json:"externalWebhookUrl"`
	WebhookVerificationToken string `json:"webhookVerificationToken"`
}

type googleOAuthState struct {
	UserID      string  `json:"userId"`
	RedirectURI *string `json:"redirectUri,omitempty"`
}

type GoogleAccountLinker struct {
	db           Database
	runtimePath  string
	loadKey      PrivateKeyLoader
	baseURL      string
	client       *http.Client
	endpoints    GoogleEndpoints
	webhookMu    sync.Mutex
	webhookToken string
}

func NewGoogleAccountLinker(db Database, runtimePath string, loadKey PrivateKeyLoader, baseURL string) *GoogleAccountLinker {
	return NewGoogleAccountLinkerWithEndpoints(db, runtimePath, loadKey, baseURL, GoogleEndpoints{
		AuthorizationURL: "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL:         "https://oauth2.googleapis.com/token",
		UserInfoURL:      "https://www.googleapis.com/oauth2/v2/userinfo",
		CalendarListURL:  "https://www.googleapis.com/calendar/v3/users/me/calendarList",
		EventsURL:        "https://www.googleapis.com/calendar/v3/calendars",
		ChannelsStopURL:  "https://www.googleapis.com/calendar/v3/channels/stop",
	})
}

func NewGoogleAccountLinkerWithEndpoints(db Database, runtimePath string, loadKey PrivateKeyLoader, baseURL string, endpoints GoogleEndpoints) *GoogleAccountLinker {
	return &GoogleAccountLinker{
		db: db, runtimePath: runtimePath, loadKey: loadKey, baseURL: strings.TrimRight(baseURL, "/"),
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		endpoints: endpoints,
	}
}

func (s *GoogleAccountLinker) CreateAuthorizationURL(ctx context.Context, userID string, redirectURI *string) (string, error) {
	provider, err := s.loadConfig(ctx)
	if err != nil {
		return "", err
	}
	if !provider.Enabled || provider.ClientID == "" || provider.ClientSecret == "" {
		return "", &Error{Code: "unknown_oauth_provider", Message: "Calendar provider is not available."}
	}
	allowNew := true
	if provider.AllowNewAccounts != nil {
		allowNew = *provider.AllowNewAccounts
	}
	if !allowNew {
		var exists bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM calendar_accounts WHERE user_id=$1 AND provider='Google')`, userID).Scan(&exists); err != nil {
			return "", err
		}
		if !exists {
			return "", &Error{Code: "calendar_provider_link_disabled", Message: "Linking new Google Calendar accounts is disabled."}
		}
	}
	if redirectURI != nil {
		canonical, ok := canonicalCalendarRedirect(*redirectURI, s.baseURL)
		if !ok {
			return "", &Error{Code: "action_forbidden", Message: "Calendar redirect URL is not allowed."}
		}
		redirectURI = &canonical
	}
	state, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	stateText := state.String()
	payload, err := json.Marshal(googleOAuthState{UserID: userID, RedirectURI: redirectURI})
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(ctx, `WITH expired AS (
		SELECT ctid FROM runtime_states WHERE purpose=$1 AND expires_at<=clock_timestamp()
		ORDER BY expires_at LIMIT 500 FOR UPDATE SKIP LOCKED
	) DELETE FROM runtime_states WHERE ctid IN (SELECT ctid FROM expired)`, calendarOAuthStatePurpose); err != nil {
		return "", err
	}
	hash := calendarStateHash(stateText)
	if _, err := s.db.Exec(ctx, `INSERT INTO runtime_states(purpose,token_hash,payload,expires_at)
		VALUES($1,$2,$3::jsonb,clock_timestamp()+make_interval(secs=>$4))`, calendarOAuthStatePurpose, hash, payload, int(calendarOAuthStateTTL.Seconds())); err != nil {
		return "", err
	}
	callbackURL := s.baseURL + "/api/calendar/oauth/callback"
	return googleAuthorizationURL(s.endpoints.AuthorizationURL, provider.ClientID, stateText, callbackURL), nil
}

func googleAuthorizationURL(endpoint, clientID, state, callbackURL string) string {
	query := url.Values{
		"client_id":              {clientID},
		"redirect_uri":           {callbackURL},
		"response_type":          {"code"},
		"access_type":            {"offline"},
		"prompt":                 {"consent"},
		"include_granted_scopes": {"true"},
		"scope":                  {"https://www.googleapis.com/auth/calendar.readonly https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile"},
		"state":                  {state},
	}
	return endpoint + "?" + query.Encode()
}

func (s *GoogleAccountLinker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	code, state := r.URL.Query().Get("code"), r.URL.Query().Get("state")
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, maxCalendarOAuthCallbackBytes)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid callback", http.StatusBadRequest)
			return
		}
		if code == "" {
			code = r.Form.Get("code")
		}
		if state == "" {
			state = r.Form.Get("state")
		}
	}
	if code == "" || state == "" || len(state) != 36 {
		http.Error(w, "missing OAuth callback parameters", http.StatusBadRequest)
		return
	}
	redirectURI, err := s.completeCallback(r.Context(), code, state)
	if err != nil {
		http.Error(w, "calendar authorization failed", http.StatusBadGateway)
		return
	}
	if redirectURI != nil {
		http.Redirect(w, r, addCalendarRedirectQuery(*redirectURI, nil), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = io.WriteString(w, `{"ok":true}`)
}

func (s *GoogleAccountLinker) WebhookHandler() http.Handler {
	return http.HandlerFunc(s.handleGoogleWebhook)
}

func (s *GoogleAccountLinker) handleGoogleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	provider, err := s.loadConfig(r.Context())
	if err != nil {
		http.Error(w, "calendar webhook configuration unavailable", http.StatusServiceUnavailable)
		return
	}
	if strings.TrimSpace(provider.ExternalWebhookURL) == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	channelID := strings.TrimSpace(r.Header.Get("x-goog-channel-id"))
	if channelID == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	expectedToken := strings.TrimSpace(provider.WebhookVerificationToken)
	if expectedToken == "" {
		s.webhookMu.Lock()
		if s.webhookToken == "" {
			s.webhookToken = uuid.NewString()
		}
		expectedToken = s.webhookToken
		s.webhookMu.Unlock()
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("x-goog-channel-token")), []byte(expectedToken)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if _, err := s.db.Exec(r.Context(), `UPDATE calendar_subscriptions
		SET next_sync_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE provider='Google' AND enabled AND custom_channel_id=$1`, channelID); err != nil {
		http.Error(w, "calendar webhook processing failed", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *GoogleAccountLinker) completeCallback(ctx context.Context, code, state string) (*string, error) {
	var payload []byte
	err := s.db.QueryRow(ctx, `UPDATE runtime_states SET consumed_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE purpose=$1 AND token_hash=$2 AND consumed_at IS NULL AND expires_at>clock_timestamp()
		RETURNING payload`, calendarOAuthStatePurpose, calendarStateHash(state)).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &Error{Code: "oauth_state_expired", Message: "OAuth state expired, please try again."}
	}
	if err != nil {
		return nil, err
	}
	var saved googleOAuthState
	if err := json.Unmarshal(payload, &saved); err != nil || saved.UserID == "" {
		return nil, &Error{Code: "invalid_oauth_state", Message: "OAuth state is invalid."}
	}
	provider, err := s.loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	if !provider.Enabled || provider.ClientID == "" || provider.ClientSecret == "" {
		return nil, &Error{Code: "unknown_oauth_provider", Message: "Calendar provider is not available."}
	}
	callbackURL := s.baseURL + "/api/calendar/oauth/callback"
	token, err := s.exchangeCode(ctx, provider, code, callbackURL)
	if err != nil {
		return nil, err
	}
	profile, err := s.loadProfile(ctx, token.AccessToken)
	if err != nil {
		return nil, err
	}
	allowNew := true
	if provider.AllowNewAccounts != nil {
		allowNew = *provider.AllowNewAccounts
	}
	if !allowNew {
		var exists bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM calendar_accounts WHERE user_id=$1 AND provider='Google' AND provider_account_id=$2)`, saved.UserID, profile.ID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, &Error{Code: "calendar_provider_link_disabled", Message: "Linking new Google Calendar accounts is disabled."}
		}
	}
	key, err := s.loadKey(ctx)
	if err != nil || strings.TrimSpace(key) == "" {
		return nil, &Error{Code: "calendar_encryption_unavailable", Message: "Calendar credential encryption is unavailable."}
	}
	encryptedAccess, err := encryptCalendarToken(key, token.AccessToken)
	if err != nil {
		return nil, err
	}
	var encryptedRefresh any
	if token.RefreshToken != "" {
		value, err := encryptCalendarToken(key, token.RefreshToken)
		if err != nil {
			return nil, err
		}
		encryptedRefresh = value
	}
	calendars, err := s.listCalendars(ctx, token.AccessToken)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var accountID string
	var expiresAt any
	if token.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	err = tx.QueryRow(ctx, `INSERT INTO calendar_accounts(
		id,user_id,provider,provider_account_id,display_name,email,access_token,refresh_token,expires_at,scope,status,last_error,created_at,updated_at)
		VALUES($1,$2,'Google',$3,$4,$5,$6,$7,$8,$9,'active',NULL,now(),now())
		ON CONFLICT(user_id,provider,provider_account_id) DO UPDATE SET display_name=excluded.display_name,email=excluded.email,
		access_token=excluded.access_token,refresh_token=COALESCE(excluded.refresh_token,calendar_accounts.refresh_token),
		expires_at=excluded.expires_at,scope=excluded.scope,status='active',last_error=NULL,updated_at=now() RETURNING id`,
		uuid.NewString(), saved.UserID, profile.ID, nullableString(profile.Name), nullableString(profile.Email), encryptedAccess,
		encryptedRefresh, expiresAt, nullableString(token.Scope)).Scan(&accountID)
	if err != nil {
		return nil, err
	}
	for _, item := range calendars {
		if _, err := tx.Exec(ctx, `INSERT INTO calendar_subscriptions(
			id,account_id,provider,external_calendar_id,display_name,timezone,color,enabled,next_sync_at,created_at,updated_at)
			VALUES($1,$2,'Google',$3,$4,$5,$6,true,now(),now(),now())
			ON CONFLICT(account_id,external_calendar_id) DO UPDATE SET display_name=excluded.display_name,
			timezone=excluded.timezone,color=excluded.color,enabled=true,next_sync_at=LEAST(calendar_subscriptions.next_sync_at,now()),updated_at=now()`,
			uuid.NewString(), accountID, item.ID, nullableString(item.Summary), nullableString(item.TimeZone), nullableString(item.ColorID)); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if err := s.SyncAccountNow(ctx, accountID); err != nil {
		return saved.RedirectURI, err
	}
	return saved.RedirectURI, nil
}

func (s *GoogleAccountLinker) loadConfig(ctx context.Context) (googleCalendarConfig, error) {
	raw, found, err := config.ReadNodeRuntimeValue(ctx, s.db, s.runtimePath, "calendar.google")
	if err != nil {
		return googleCalendarConfig{}, err
	}
	if !found {
		return googleCalendarConfig{}, nil
	}
	var provider googleCalendarConfig
	if err := json.Unmarshal(raw, &provider); err != nil {
		return googleCalendarConfig{}, &Error{Code: "calendar_config_invalid", Message: "Calendar configuration is invalid."}
	}
	return provider, nil
}

func (s *GoogleAccountLinker) AvailableCalendarProviders(ctx context.Context) ([]string, error) {
	providers := make([]string, 0, 2)
	google, err := s.loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	if google.Enabled && google.ClientID != "" && google.ClientSecret != "" {
		providers = append(providers, "Google")
	}
	raw, found, err := config.ReadNodeRuntimeValue(ctx, s.db, s.runtimePath, "calendar.caldav")
	if err != nil {
		return nil, err
	}
	if found {
		var caldav ProviderConfig
		if err := json.Unmarshal(raw, &caldav); err != nil {
			return nil, &Error{Code: "calendar_config_invalid", Message: "Calendar configuration is invalid."}
		}
		if caldav.Enabled {
			providers = append(providers, "CalDAV")
		}
	}
	return providers, nil
}

type googleTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

func (s *GoogleAccountLinker) exchangeCode(ctx context.Context, provider googleCalendarConfig, code, callbackURL string) (googleTokenResponse, error) {
	form := url.Values{"code": {code}, "client_id": {provider.ClientID}, "client_secret": {provider.ClientSecret}, "redirect_uri": {callbackURL}, "grant_type": {"authorization_code"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoints.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return googleTokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var token googleTokenResponse
	if err := s.doJSON(request, &token); err != nil || token.AccessToken == "" {
		if err == nil {
			err = errors.New("Google token response is missing access_token")
		}
		return googleTokenResponse{}, err
	}
	return token, nil
}

type googleProfile struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

func (s *GoogleAccountLinker) loadProfile(ctx context.Context, accessToken string) (googleProfile, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoints.UserInfoURL, nil)
	if err != nil {
		return googleProfile{}, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	var profile googleProfile
	if err := s.doJSON(request, &profile); err != nil {
		return googleProfile{}, err
	}
	if profile.ID == "" {
		return googleProfile{}, errors.New("Google profile response is missing id")
	}
	return profile, nil
}

type googleCalendarList struct {
	Items         []googleCalendarItem `json:"items"`
	NextPageToken string               `json:"nextPageToken"`
}

type googleCalendarItem struct {
	ID       string `json:"id"`
	Summary  string `json:"summary"`
	TimeZone string `json:"timeZone"`
	ColorID  string `json:"colorId"`
}

func (s *GoogleAccountLinker) listCalendars(ctx context.Context, accessToken string) ([]googleCalendarItem, error) {
	endpoint, err := url.Parse(s.endpoints.CalendarListURL)
	if err != nil {
		return nil, err
	}
	var result []googleCalendarItem
	for pageToken := ""; ; {
		query := endpoint.Query()
		query.Set("maxResults", "250")
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		endpoint.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+accessToken)
		var page googleCalendarList
		if err := s.doJSON(request, &page); err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if item.ID != "" {
				result = append(result, item)
			}
		}
		if page.NextPageToken == "" {
			return result, nil
		}
		pageToken = page.NextPageToken
	}
}

func (s *GoogleAccountLinker) doJSON(request *http.Request, target any) error {
	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("Google calendar request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxGoogleCalendarResponseBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxGoogleCalendarResponseBytes {
		return errors.New("Google calendar response is too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &Error{Code: "calendar_provider_request_error", Message: "Google Calendar request failed.", Status: response.StatusCode}
	}
	if err := json.Unmarshal(body, target); err != nil {
		return errors.New("Google calendar response is invalid")
	}
	return nil
}

func canonicalCalendarRedirect(input, baseURL string) (string, bool) {
	parsed, err := url.Parse(input)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", false
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && parsed.Hostname() == "localhost") {
		return "", false
	}
	base, err := url.Parse(baseURL)
	if err == nil && strings.EqualFold(parsed.Host, base.Host) {
		return parsed.String(), true
	}
	return "", false
}

func addCalendarRedirectQuery(input string, values url.Values) string {
	parsed, err := url.Parse(input)
	if err != nil {
		return input
	}
	for key, items := range values {
		for _, item := range items {
			parsed.Query().Add(key, item)
		}
	}
	return parsed.String()
}

func calendarStateHash(state string) string {
	digest := sha256.Sum256([]byte(state))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
