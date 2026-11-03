// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	calendarSyncClaimDuration  = 30 * time.Minute
	calendarSyncFailureBase    = 5 * time.Minute
	calendarSyncFailureMax     = 6 * time.Hour
	calendarSyncMaxBatch       = 8
	calendarSyncMaxBatches     = 25
	calendarSyncMaxPages       = 100
	calendarChannelRenewBefore = 24 * time.Hour
	calendarChannelRetryAfter  = 5 * time.Minute
)

var unsupportedPushChannelExpiration = time.Date(9999, 12, 31, 23, 59, 59, 999000000, time.UTC)

type googleCalendarEventTime struct {
	DateTime string `json:"dateTime"`
	Date     string `json:"date"`
	TimeZone string `json:"timeZone"`
}

type googleCalendarEvent struct {
	ID               string                  `json:"id"`
	ETag             string                  `json:"etag"`
	Status           string                  `json:"status"`
	Summary          string                  `json:"summary"`
	Description      string                  `json:"description"`
	Location         string                  `json:"location"`
	Updated          string                  `json:"updated"`
	RecurringEventID string                  `json:"recurringEventId"`
	OriginalStart    googleCalendarEventTime `json:"originalStartTime"`
	Start            googleCalendarEventTime `json:"start"`
	End              googleCalendarEventTime `json:"end"`
	Raw              json.RawMessage         `json:"-"`
}

type googleCalendarEventsPage struct {
	Items         []json.RawMessage `json:"items"`
	NextPageToken string            `json:"nextPageToken"`
	NextSyncToken string            `json:"nextSyncToken"`
}

type googleCalendarSyncEvent struct {
	ID, ETag, Status, Summary, Description, Location string
	RecurrenceID, Timezone                           *string
	Start, End                                       time.Time
	AllDay                                           bool
	ProviderUpdatedAt                                *time.Time
	Raw                                              []byte
}

type googleCalendarSyncResponse struct {
	Events        []googleCalendarSyncEvent
	NextSyncToken string
}

type calendarSyncClaim struct {
	ID           string
	ClaimedUntil time.Time
}

type calendarSyncSubscription struct {
	ID, AccountID, ExternalCalendarID string
	Timezone, SyncToken               *string
	CustomChannelID, CustomResourceID *string
	ChannelExpiration                 pgtype.Timestamptz
	RetryCount, RefreshInterval       int
	AccessToken, RefreshToken         *string
	ExpiresAt                         pgtype.Timestamptz
	Status                            string
}

// SyncDue claims a bounded batch so multiple worker instances can poll safely.
func (s *GoogleAccountLinker) SyncDue(ctx context.Context) error {
	return s.syncSubscriptions(ctx, "")
}

func (s *GoogleAccountLinker) SyncAccountNow(ctx context.Context, accountID string) error {
	if accountID == "" {
		return errors.New("calendar account id is required")
	}
	if _, err := s.db.Exec(ctx, `UPDATE calendar_subscriptions SET next_sync_at=clock_timestamp()
		WHERE account_id=$1 AND enabled`, accountID); err != nil {
		return err
	}
	return s.syncSubscriptions(ctx, accountID)
}

func (s *GoogleAccountLinker) syncSubscriptions(ctx context.Context, accountID string) error {
	if s == nil || s.db == nil || s.loadKey == nil {
		return errors.New("Google Calendar sync is unavailable")
	}
	var failures []error
	for batch := 0; batch < calendarSyncMaxBatches; batch++ {
		now := time.Now().UTC()
		claims, err := s.claimDue(ctx, now, accountID)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		for _, claim := range claims {
			if err := s.syncClaim(ctx, claim, now); err != nil {
				// Each claim is rescheduled independently; one provider failure must not stall the batch.
				failures = append(failures, fmt.Errorf("calendar subscription %s: %w", claim.ID, err))
			}
		}
		if len(claims) < calendarSyncMaxBatch {
			break
		}
	}
	return errors.Join(failures...)
}

func (s *GoogleAccountLinker) claimDue(ctx context.Context, now time.Time, accountID string) ([]calendarSyncClaim, error) {
	var accountFilter any
	if accountID != "" {
		accountFilter = accountID
	}
	rows, err := s.db.Query(ctx, `WITH candidates AS (
		SELECT subscription.id FROM calendar_subscriptions subscription
		JOIN calendar_accounts account ON account.id=subscription.account_id
		WHERE subscription.provider='Google' AND subscription.enabled
		  AND subscription.next_sync_at <= $1
		  AND ($4::text IS NULL OR account.id=$4)
		  AND (subscription.sync_claimed_until IS NULL OR subscription.sync_claimed_until <= $1)
		  AND account.status='active'
		ORDER BY subscription.next_sync_at
		FOR UPDATE OF subscription SKIP LOCKED LIMIT $2
	) UPDATE calendar_subscriptions subscription
	SET next_sync_at=$1+make_interval(secs=>$3), sync_claimed_until=$1+make_interval(secs=>$3), updated_at=$1
	FROM candidates WHERE subscription.id=candidates.id
	RETURNING subscription.id,subscription.sync_claimed_until`, now, calendarSyncMaxBatch, int(calendarSyncClaimDuration.Seconds()), accountFilter)
	if err != nil {
		return nil, fmt.Errorf("claim due Google calendar subscriptions: %w", err)
	}
	defer rows.Close()
	claims := make([]calendarSyncClaim, 0, calendarSyncMaxBatch)
	for rows.Next() {
		var claim calendarSyncClaim
		if err := rows.Scan(&claim.ID, &claim.ClaimedUntil); err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}

func (s *GoogleAccountLinker) syncClaim(ctx context.Context, claim calendarSyncClaim, now time.Time) error {
	var subscription calendarSyncSubscription
	err := s.db.QueryRow(ctx, `SELECT subscription.id,subscription.account_id,subscription.external_calendar_id,
		subscription.timezone,subscription.sync_token,subscription.sync_retry_count,
		subscription.custom_channel_id,subscription.custom_resource_id,subscription.channel_expiration,
		account.refresh_interval_minutes,account.access_token,account.refresh_token,account.expires_at,account.status
		FROM calendar_subscriptions subscription
		JOIN calendar_accounts account ON account.id=subscription.account_id
		WHERE subscription.id=$1 AND subscription.enabled AND subscription.sync_claimed_until=$2`, claim.ID, claim.ClaimedUntil).
		Scan(&subscription.ID, &subscription.AccountID, &subscription.ExternalCalendarID, &subscription.Timezone,
			&subscription.SyncToken, &subscription.RetryCount, &subscription.CustomChannelID, &subscription.CustomResourceID,
			&subscription.ChannelExpiration, &subscription.RefreshInterval, &subscription.AccessToken,
			&subscription.RefreshToken, &subscription.ExpiresAt, &subscription.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return s.retryClaim(ctx, claim, subscription.RetryCount, err)
	}
	if subscription.Status != "active" {
		return nil
	}

	key, err := s.loadKey(ctx)
	if err != nil || strings.TrimSpace(key) == "" {
		if err == nil {
			err = errors.New("calendar encryption key is unavailable")
		}
		return s.retryClaim(ctx, claim, subscription.RetryCount, err)
	}
	accessToken, refreshToken, err := s.calendarTokens(key, subscription)
	if err != nil {
		return s.invalidateAccount(ctx, claim, subscription.AccountID, err)
	}
	provider, err := s.loadConfig(ctx)
	if err != nil || !provider.Enabled || provider.ClientID == "" || provider.ClientSecret == "" {
		if err == nil {
			err = errors.New("Google Calendar provider is disabled")
		}
		return s.retryClaim(ctx, claim, subscription.RetryCount, err)
	}
	if subscription.ExpiresAt.Valid && !subscription.ExpiresAt.Time.After(now.Add(time.Minute)) && refreshToken != "" {
		refreshed, refreshErr := s.refreshGoogleToken(ctx, provider, refreshToken)
		if refreshErr != nil {
			var providerError *Error
			if errors.As(refreshErr, &providerError) && providerError.Status == http.StatusBadRequest {
				return s.invalidateAccount(ctx, claim, subscription.AccountID, refreshErr)
			}
			return s.retryClaim(ctx, claim, subscription.RetryCount, refreshErr)
		}
		accessToken = refreshed.AccessToken
		if refreshed.RefreshToken != "" {
			refreshToken = refreshed.RefreshToken
		}
		if err := s.saveCalendarTokens(ctx, key, subscription.AccountID, claim, refreshed, refreshToken); err != nil {
			return s.retryClaim(ctx, claim, subscription.RetryCount, err)
		}
	}

	response, err := s.listEvents(ctx, accessToken, subscription.ExternalCalendarID, subscription.SyncToken, subscription.Timezone, now)
	if err != nil {
		var providerError *Error
		if errors.As(err, &providerError) && providerError.Status == http.StatusGone && subscription.SyncToken != nil {
			response, err = s.listEvents(ctx, accessToken, subscription.ExternalCalendarID, nil, subscription.Timezone, now)
		}
	}
	if err != nil {
		var providerError *Error
		if errors.As(err, &providerError) && providerError.Status == http.StatusUnauthorized && refreshToken != "" {
			refreshed, refreshErr := s.refreshGoogleToken(ctx, provider, refreshToken)
			if refreshErr == nil {
				accessToken = refreshed.AccessToken
				if refreshed.RefreshToken != "" {
					refreshToken = refreshed.RefreshToken
				}
				if saveErr := s.saveCalendarTokens(ctx, key, subscription.AccountID, claim, refreshed, refreshToken); saveErr == nil {
					response, err = s.listEvents(ctx, accessToken, subscription.ExternalCalendarID, subscription.SyncToken, subscription.Timezone, now)
				}
			}
		}
	}
	if err != nil {
		return s.retryClaim(ctx, claim, subscription.RetryCount, err)
	}
	channelRetry := false
	if err := s.ensureWebhookChannel(ctx, claim, subscription, accessToken, provider); err != nil {
		channelRetry = true
	}
	return s.commitSync(ctx, claim, subscription, response, now, channelRetry)
}

func (s *GoogleAccountLinker) ensureWebhookChannel(ctx context.Context, claim calendarSyncClaim, subscription calendarSyncSubscription, accessToken string, provider googleCalendarConfig) error {
	address := strings.TrimRight(provider.ExternalWebhookURL, "/")
	if address == "" {
		return nil
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return errors.New("Google Calendar webhook URL is invalid")
	}
	parsed.Path = "/api/calendar/webhook/google"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	address = parsed.String()
	if subscription.ChannelExpiration.Valid && subscription.ChannelExpiration.Time.After(time.Now().Add(calendarChannelRenewBefore)) {
		return nil
	}
	if subscription.CustomChannelID != nil && *subscription.CustomChannelID != "" && subscription.CustomResourceID != nil && *subscription.CustomResourceID != "" {
		if err := s.stopWebhookChannel(ctx, accessToken, *subscription.CustomChannelID, *subscription.CustomResourceID); err != nil {
			var providerError *Error
			if !errors.As(err, &providerError) || (providerError.Status != http.StatusNotFound && providerError.Status != http.StatusGone) {
				return err
			}
		}
	}
	token := strings.TrimSpace(provider.WebhookVerificationToken)
	if token == "" {
		s.webhookMu.Lock()
		if s.webhookToken == "" {
			s.webhookToken = uuid.NewString()
		}
		token = s.webhookToken
		s.webhookMu.Unlock()
	}
	channelID := uuid.NewString()
	result, err := s.watchCalendar(ctx, accessToken, subscription.ExternalCalendarID, address, token, channelID)
	if err != nil {
		var providerError *Error
		if errors.As(err, &providerError) && providerError.Status == http.StatusBadRequest && strings.Contains(providerError.Message, "pushNotSupportedForRequestedResource") {
			_, updateErr := s.db.Exec(ctx, `UPDATE calendar_subscriptions SET custom_channel_id=NULL,custom_resource_id=NULL,channel_expiration=$3,updated_at=clock_timestamp()
				WHERE id=$1 AND sync_claimed_until=$2 AND sync_claimed_until>clock_timestamp()`, claim.ID, claim.ClaimedUntil, unsupportedPushChannelExpiration)
			return updateErr
		}
		return err
	}
	if result.ChannelID == "" || result.ResourceID == "" {
		return errors.New("Google Calendar returned an incomplete webhook channel")
	}
	var expiration any
	if result.Expiration != nil {
		expiration = *result.Expiration
	}
	tag, err := s.db.Exec(ctx, `UPDATE calendar_subscriptions SET custom_channel_id=$3,custom_resource_id=$4,channel_expiration=$5,updated_at=clock_timestamp()
		WHERE id=$1 AND sync_claimed_until=$2 AND sync_claimed_until>clock_timestamp()`, claim.ID, claim.ClaimedUntil, result.ChannelID, result.ResourceID, expiration)
	if err != nil {
		_ = s.stopWebhookChannel(context.WithoutCancel(ctx), accessToken, result.ChannelID, result.ResourceID)
		return err
	}
	if tag.RowsAffected() != 1 {
		_ = s.stopWebhookChannel(context.WithoutCancel(ctx), accessToken, result.ChannelID, result.ResourceID)
		return errors.New("calendar sync claim expired before webhook channel was saved")
	}
	return nil
}

type googleWatchChannelResult struct {
	ChannelID  string
	ResourceID string
	Expiration *time.Time
}

func (s *GoogleAccountLinker) watchCalendar(ctx context.Context, accessToken, calendarID, address, token, channelID string) (googleWatchChannelResult, error) {
	endpoint, err := url.Parse(s.endpoints.EventsURL)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return googleWatchChannelResult{}, errors.New("Google Calendar events endpoint is invalid")
	}
	appendGoogleCalendarPath(endpoint, calendarID, "events", "watch")
	body, err := json.Marshal(map[string]string{"id": channelID, "type": "web_hook", "address": address, "token": token})
	if err != nil {
		return googleWatchChannelResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(string(body)))
	if err != nil {
		return googleWatchChannelResult{}, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	var response struct {
		ID         string `json:"id"`
		ResourceID string `json:"resourceId"`
		Expiration string `json:"expiration"`
	}
	if err := s.doJSON(request, &response); err != nil {
		return googleWatchChannelResult{}, err
	}
	result := googleWatchChannelResult{ChannelID: response.ID, ResourceID: response.ResourceID}
	if response.Expiration != "" {
		millis, err := strconv.ParseInt(response.Expiration, 10, 64)
		if err != nil {
			return googleWatchChannelResult{}, errors.New("Google Calendar returned an invalid webhook expiration")
		}
		expiration := time.UnixMilli(millis).UTC()
		result.Expiration = &expiration
	}
	return result, nil
}

func appendGoogleCalendarPath(endpoint *url.URL, segments ...string) {
	escapedPath := strings.TrimRight(endpoint.EscapedPath(), "/")
	for _, segment := range segments {
		escapedPath += "/" + url.PathEscape(segment)
	}
	path, err := url.PathUnescape(escapedPath)
	if err != nil {
		return
	}
	endpoint.Path = path
	endpoint.RawPath = escapedPath
}

func (s *GoogleAccountLinker) stopWebhookChannel(ctx context.Context, accessToken, channelID, resourceID string) error {
	endpoint := s.endpoints.ChannelsStopURL
	if endpoint == "" {
		endpoint = "https://www.googleapis.com/calendar/v3/channels/stop"
	}
	body, err := json.Marshal(map[string]string{"id": channelID, "resourceId": resourceID})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return &Error{Code: "calendar_provider_request_failed", Message: string(data), Status: response.StatusCode}
	}
	return nil
}

func (s *GoogleAccountLinker) calendarTokens(key string, subscription calendarSyncSubscription) (string, string, error) {
	var accessToken, refreshToken string
	if subscription.AccessToken != nil {
		plain, err := decryptCalendarToken(key, *subscription.AccessToken)
		if err != nil {
			return "", "", err
		}
		accessToken = plain
	}
	if subscription.RefreshToken != nil {
		plain, err := decryptCalendarToken(key, *subscription.RefreshToken)
		if err != nil {
			return "", "", err
		}
		refreshToken = plain
	}
	if accessToken == "" && refreshToken == "" {
		return "", "", errors.New("Google Calendar account has no credentials")
	}
	return accessToken, refreshToken, nil
}

func (s *GoogleAccountLinker) refreshGoogleToken(ctx context.Context, provider googleCalendarConfig, refreshToken string) (googleTokenResponse, error) {
	form := url.Values{"client_id": {provider.ClientID}, "client_secret": {provider.ClientSecret}, "grant_type": {"refresh_token"}, "refresh_token": {refreshToken}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoints.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return googleTokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var token googleTokenResponse
	if err := s.doJSON(request, &token); err != nil {
		return googleTokenResponse{}, err
	}
	if token.AccessToken == "" || token.ExpiresIn <= 0 {
		return googleTokenResponse{}, errors.New("Google token refresh response is invalid")
	}
	return token, nil
}

func (s *GoogleAccountLinker) saveCalendarTokens(ctx context.Context, key, accountID string, claim calendarSyncClaim, token googleTokenResponse, fallbackRefresh string) error {
	access, err := encryptCalendarToken(key, token.AccessToken)
	if err != nil {
		return err
	}
	refresh := fallbackRefresh
	if token.RefreshToken != "" {
		refresh = token.RefreshToken
	}
	var encryptedRefresh any
	if refresh != "" {
		encryptedRefresh, err = encryptCalendarToken(key, refresh)
		if err != nil {
			return err
		}
	}
	var expires any
	if token.ExpiresIn > 0 {
		expires = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	tag, err := s.db.Exec(ctx, `UPDATE calendar_accounts SET access_token=$2,refresh_token=COALESCE($3,refresh_token),expires_at=$4,
		status='active',last_error=NULL,updated_at=clock_timestamp() WHERE id=$1 AND EXISTS(
		SELECT 1 FROM calendar_subscriptions WHERE id=$5 AND account_id=$1 AND sync_claimed_until=$6 AND sync_claimed_until>clock_timestamp())`,
		accountID, access, encryptedRefresh, expires, claim.ID, claim.ClaimedUntil)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("calendar sync claim expired before token refresh was saved")
	}
	return nil
}

func (s *GoogleAccountLinker) listEvents(ctx context.Context, accessToken, calendarID string, syncToken, timezone *string, now time.Time) (googleCalendarSyncResponse, error) {
	base, err := url.Parse(s.endpoints.EventsURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return googleCalendarSyncResponse{}, errors.New("Google Calendar events endpoint is invalid")
	}
	appendGoogleCalendarPath(base, calendarID, "events")
	var result googleCalendarSyncResponse
	pageToken := ""
	for page := 0; page < calendarSyncMaxPages; page++ {
		endpoint := *base
		query := endpoint.Query()
		query.Set("singleEvents", "true")
		query.Set("showDeleted", "true")
		query.Set("maxResults", "2500")
		if syncToken != nil && *syncToken != "" {
			query.Set("syncToken", *syncToken)
		} else {
			start := now.AddDate(0, 0, -90).UTC().Format(time.RFC3339)
			end := now.AddDate(0, 0, 180).UTC().Format(time.RFC3339)
			query.Set("timeMin", start)
			query.Set("timeMax", end)
			query.Set("orderBy", "startTime")
		}
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		endpoint.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return googleCalendarSyncResponse{}, err
		}
		request.Header.Set("Authorization", "Bearer "+accessToken)
		var response googleCalendarEventsPage
		if err := s.doJSON(request, &response); err != nil {
			return googleCalendarSyncResponse{}, err
		}
		for _, raw := range response.Items {
			var event googleCalendarEvent
			if err := json.Unmarshal(raw, &event); err != nil || event.ID == "" {
				continue
			}
			event.Raw = raw
			mapped, err := mapGoogleCalendarEvent(event, timezone)
			if err == nil {
				result.Events = append(result.Events, mapped)
			}
		}
		pageToken = response.NextPageToken
		if pageToken == "" {
			result.NextSyncToken = response.NextSyncToken
			return result, nil
		}
	}
	return googleCalendarSyncResponse{}, errors.New("Google Calendar returned too many event pages")
}

func mapGoogleCalendarEvent(event googleCalendarEvent, fallbackTimezone *string) (googleCalendarSyncEvent, error) {
	fallback := "UTC"
	if fallbackTimezone != nil && *fallbackTimezone != "" {
		fallback = *fallbackTimezone
	}
	start, startAllDay, startZone, err := googleCalendarEventDate(event.Start, fallback)
	if err != nil {
		return googleCalendarSyncEvent{}, err
	}
	end, endAllDay, endZone, err := googleCalendarEventDate(event.End, fallback)
	if err != nil {
		return googleCalendarSyncEvent{}, err
	}
	zone := startZone
	if zone == "" {
		zone = endZone
	}
	var recurrence *string
	if event.OriginalStart.DateTime != "" {
		recurrence = &event.OriginalStart.DateTime
	} else if event.OriginalStart.Date != "" {
		recurrence = &event.OriginalStart.Date
	}
	var updated *time.Time
	if event.Updated != "" {
		parsed, err := time.Parse(time.RFC3339Nano, event.Updated)
		if err != nil {
			return googleCalendarSyncEvent{}, err
		}
		updated = &parsed
	}
	var timezone *string
	if zone != "" {
		timezone = &zone
	}
	raw := event.Raw
	if len(raw) == 0 {
		var err error
		raw, err = json.Marshal(event)
		if err != nil {
			return googleCalendarSyncEvent{}, err
		}
	}
	return googleCalendarSyncEvent{ID: event.ID, ETag: event.ETag, Status: event.Status, Summary: event.Summary,
		Description: event.Description, Location: event.Location, RecurrenceID: recurrence, Timezone: timezone,
		Start: start, End: end, AllDay: startAllDay || endAllDay, ProviderUpdatedAt: updated, Raw: raw}, nil
}

func googleCalendarEventDate(value googleCalendarEventTime, fallbackTimezone string) (time.Time, bool, string, error) {
	if value.DateTime != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value.DateTime)
		return parsed.UTC(), false, value.TimeZone, err
	}
	if value.Date == "" {
		return time.Time{}, false, "", errors.New("Google Calendar event time is missing")
	}
	zone := value.TimeZone
	if zone == "" {
		zone = fallbackTimezone
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, false, "", err
	}
	parsed, err := time.ParseInLocation("2006-01-02", value.Date, location)
	return parsed.UTC(), true, zone, err
}

func (s *GoogleAccountLinker) commitSync(ctx context.Context, claim calendarSyncClaim, subscription calendarSyncSubscription, response googleCalendarSyncResponse, now time.Time, channelRetry bool) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var accountID string
	err = tx.QueryRow(ctx, `SELECT account.id FROM calendar_accounts account JOIN calendar_subscriptions subscription ON subscription.account_id=account.id
		WHERE subscription.id=$1 AND account.status='active' FOR UPDATE OF account`, claim.ID).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var currentClaim time.Time
	var claimActive bool
	if err := tx.QueryRow(ctx, `SELECT sync_claimed_until,sync_claimed_until>clock_timestamp() FROM calendar_subscriptions WHERE id=$1 AND enabled FOR UPDATE`, claim.ID).Scan(&currentClaim, &claimActive); err != nil {
		return err
	}
	if !currentClaim.Equal(claim.ClaimedUntil) || !claimActive {
		return nil
	}
	completedAt := time.Now().UTC()
	for _, event := range response.Events {
		if event.Status == "cancelled" {
			if _, err := tx.Exec(ctx, `DELETE FROM calendar_events WHERE subscription_id=$1 AND external_event_id=$2`, subscription.ID, event.ID); err != nil {
				return fmt.Errorf("delete cancelled calendar event %s: %w", event.ID, err)
			}
			continue
		}
		var exists bool
		if err := tx.QueryRow(ctx, `UPDATE calendar_events SET etag=$4,status=NULLIF($5,''),title=NULLIF($6,''),description=NULLIF($7,''),location=NULLIF($8,''),
			start_at_utc=$9,end_at_utc=$10,original_timezone=$11,all_day=$12,provider_updated_at=$13,raw=$14::jsonb,updated_at=$15
			WHERE subscription_id=$1 AND external_event_id=$2 AND recurrence_id IS NOT DISTINCT FROM $3 RETURNING true`,
			subscription.ID, event.ID, event.RecurrenceID, nullableString(event.ETag), event.Status, event.Summary, event.Description,
			event.Location, event.Start, event.End, event.Timezone, event.AllDay, event.ProviderUpdatedAt, event.Raw, completedAt).Scan(&exists); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("update calendar event %s: %w", event.ID, err)
		} else if errors.Is(err, pgx.ErrNoRows) {
			if _, err := tx.Exec(ctx, `INSERT INTO calendar_events(id,subscription_id,external_event_id,recurrence_id,etag,status,title,description,location,
				start_at_utc,end_at_utc,original_timezone,all_day,provider_updated_at,raw,created_at,updated_at)
				VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),$10,$11,$12,$13,$14,$15::jsonb,$16,$16)`,
				newCalendarID(), subscription.ID, event.ID, event.RecurrenceID, nullableString(event.ETag), event.Status, event.Summary,
				event.Description, event.Location, event.Start, event.End, event.Timezone, event.AllDay, event.ProviderUpdatedAt, event.Raw, completedAt); err != nil {
				return fmt.Errorf("insert calendar event %s: %w", event.ID, err)
			}
		}
	}
	interval := subscription.RefreshInterval
	if interval < 1 {
		interval = 30
	}
	nextSyncAt := completedAt.Add(time.Duration(interval) * time.Minute)
	if channelRetry && completedAt.Add(calendarChannelRetryAfter).Before(nextSyncAt) {
		nextSyncAt = completedAt.Add(calendarChannelRetryAfter)
	}
	nextToken := response.NextSyncToken
	if nextToken == "" && subscription.SyncToken != nil {
		nextToken = *subscription.SyncToken
	}
	_, err = tx.Exec(ctx, `UPDATE calendar_subscriptions SET sync_token=$3,last_sync_at=$4::timestamptz,next_sync_at=$5,
		sync_retry_count=0,sync_claimed_until=NULL,updated_at=$4::timestamptz WHERE id=$1 AND sync_claimed_until=$2 AND sync_claimed_until>clock_timestamp()`,
		subscription.ID, claim.ClaimedUntil, nullableString(nextToken), completedAt, nextSyncAt)
	if err != nil {
		return fmt.Errorf("complete calendar sync state: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *GoogleAccountLinker) retryClaim(ctx context.Context, claim calendarSyncClaim, previous int, cause error) error {
	attempt := previous + 1
	if attempt < 1 {
		attempt = 1
	}
	delay := calendarSyncFailureBase * time.Duration(1<<min(attempt-1, 7))
	if delay > calendarSyncFailureMax {
		delay = calendarSyncFailureMax
	}
	_, err := s.db.Exec(ctx, `UPDATE calendar_subscriptions SET sync_retry_count=$3,next_sync_at=clock_timestamp()+make_interval(secs=>$4),
		sync_claimed_until=NULL,updated_at=clock_timestamp() WHERE id=$1 AND sync_claimed_until=$2 AND sync_claimed_until>clock_timestamp()`,
		claim.ID, claim.ClaimedUntil, attempt, int(delay.Seconds()))
	if err != nil {
		return fmt.Errorf("reschedule failed calendar sync: %w (provider error: %v)", err, cause)
	}
	return cause
}

func (s *GoogleAccountLinker) invalidateAccount(ctx context.Context, claim calendarSyncClaim, accountID string, cause error) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM calendar_subscriptions WHERE id=$1 AND sync_claimed_until=$2 AND sync_claimed_until>clock_timestamp())`, claim.ID, claim.ClaimedUntil).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE calendar_accounts SET status='invalid',last_error=$2,updated_at=clock_timestamp() WHERE id=$1`, accountID, cause.Error()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM calendar_subscriptions WHERE account_id=$1`, accountID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func newCalendarID() string {
	return uuid.NewString()
}
