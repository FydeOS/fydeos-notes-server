// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package calendar

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type calDAVSyncSubscription struct {
	ID, AccountID, ExternalCalendarID string
	Timezone, SyncToken               *string
	RetryCount, RefreshInterval       int
	EncryptedPassword, Username       string
	AuthType, PresetID, ServerURL     *string
	Status                            string
}

type calDAVEvent struct {
	ID, ETag, Status, Summary, Description, Location string
	RecurrenceID, Timezone                           *string
	Start, End                                       time.Time
	AllDay                                           bool
	Updated                                          *time.Time
	Raw                                              []byte
}

type calDAVSyncResponse struct {
	Events []calDAVEvent
	Token  string
}

type calDAVSyncTokenExpired struct{ status int }

func (e *calDAVSyncTokenExpired) Error() string { return "CalDAV sync token expired" }

func (s *Service) SyncDue(ctx context.Context) error {
	return s.syncDue(ctx, "")
}

func (s *Service) SyncAccountNow(ctx context.Context, accountID string) error {
	if accountID == "" {
		return errors.New("calendar account id is required")
	}
	if _, err := s.db.Exec(ctx, `UPDATE calendar_subscriptions SET next_sync_at=clock_timestamp()
		WHERE account_id=$1 AND provider='CalDAV' AND enabled`, accountID); err != nil {
		return err
	}
	return s.syncDue(ctx, accountID)
}

func (s *Service) syncDue(ctx context.Context, accountID string) error {
	if s == nil || s.db == nil || s.loadKey == nil {
		return errors.New("CalDAV sync is unavailable")
	}
	var accountFilter any
	if accountID != "" {
		accountFilter = accountID
	}
	var failures []error
	for batch := 0; batch < calendarSyncMaxBatches; batch++ {
		now := time.Now().UTC()
		rows, err := s.db.Query(ctx, `WITH candidates AS (
			SELECT subscription.id FROM calendar_subscriptions subscription
			JOIN calendar_accounts account ON account.id=subscription.account_id
			WHERE subscription.provider='CalDAV' AND subscription.enabled
			  AND subscription.next_sync_at <= $1
			  AND ($2::text IS NULL OR account.id=$2)
			  AND (subscription.sync_claimed_until IS NULL OR subscription.sync_claimed_until <= $1)
			  AND account.status='active'
			ORDER BY subscription.next_sync_at
			FOR UPDATE OF subscription SKIP LOCKED LIMIT $3
		) UPDATE calendar_subscriptions subscription
		SET next_sync_at=$1+make_interval(secs=>$4),sync_claimed_until=$1+make_interval(secs=>$4),updated_at=$1
		FROM candidates WHERE subscription.id=candidates.id
		RETURNING subscription.id,subscription.sync_claimed_until`, now, accountFilter, calendarSyncMaxBatch, int(calendarSyncClaimDuration.Seconds()))
		if err != nil {
			return errors.Join(append(failures, fmt.Errorf("claim due CalDAV subscriptions: %w", err))...)
		}
		claims := make([]calendarSyncClaim, 0, calendarSyncMaxBatch)
		for rows.Next() {
			var claim calendarSyncClaim
			if err := rows.Scan(&claim.ID, &claim.ClaimedUntil); err != nil {
				rows.Close()
				return errors.Join(append(failures, err)...)
			}
			claims = append(claims, claim)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return errors.Join(append(failures, err)...)
		}
		rows.Close()
		for _, claim := range claims {
			if err := s.syncClaim(ctx, claim); err != nil {
				failures = append(failures, fmt.Errorf("CalDAV subscription %s: %w", claim.ID, err))
			}
		}
		if len(claims) < calendarSyncMaxBatch {
			break
		}
	}
	return errors.Join(failures...)
}

func (s *Service) syncClaim(ctx context.Context, claim calendarSyncClaim) error {
	var sub calDAVSyncSubscription
	err := s.db.QueryRow(ctx, `SELECT subscription.id,subscription.account_id,subscription.external_calendar_id,
		subscription.timezone,subscription.sync_token,subscription.sync_retry_count,account.refresh_interval_minutes,
		account.access_token,COALESCE(account.username,account.email,''),account.auth_type,account.provider_preset_id,
		account.server_url,account.status
		FROM calendar_subscriptions subscription JOIN calendar_accounts account ON account.id=subscription.account_id
		WHERE subscription.id=$1 AND subscription.provider='CalDAV' AND subscription.enabled
		AND subscription.sync_claimed_until=$2`, claim.ID, claim.ClaimedUntil).Scan(
		&sub.ID, &sub.AccountID, &sub.ExternalCalendarID, &sub.Timezone, &sub.SyncToken, &sub.RetryCount,
		&sub.RefreshInterval, &sub.EncryptedPassword, &sub.Username, &sub.AuthType, &sub.PresetID, &sub.ServerURL, &sub.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return s.retryClaim(ctx, claim, sub.RetryCount, err)
	}
	if sub.Status != "active" {
		return nil
	}
	key, err := s.loadKey(ctx)
	if err != nil || strings.TrimSpace(key) == "" {
		return s.retryClaim(ctx, claim, sub.RetryCount, errors.New("CalDAV credential encryption is unavailable"))
	}
	password, err := decryptCredential(key, sub.EncryptedPassword)
	if err != nil {
		return s.retryClaim(ctx, claim, sub.RetryCount, err)
	}
	providerConfig, err := s.providerConfig(ctx)
	if err != nil || !providerConfig.Enabled {
		if err == nil {
			err = errors.New("CalDAV integration is disabled")
		}
		return s.retryClaim(ctx, claim, sub.RetryCount, err)
	}
	var preset *ProviderPreset
	for i := range providerConfig.Providers {
		if sub.PresetID != nil && providerConfig.Providers[i].ID == *sub.PresetID {
			preset = &providerConfig.Providers[i]
			break
		}
	}
	if preset == nil {
		return s.retryClaim(ctx, claim, sub.RetryCount, errors.New("CalDAV provider preset is unavailable"))
	}
	if sub.ServerURL != nil && *sub.ServerURL != "" {
		preset.ServerURL = *sub.ServerURL
	}
	if sub.AuthType != nil && *sub.AuthType != "" {
		preset.AuthType = *sub.AuthType
	}
	client := newClient(providerConfig, *preset, sub.Username, password, s.resolve)
	defer client.client.CloseIdleConnections()
	now := time.Now().UTC()
	response, err := s.listCalDAVEvents(ctx, client, sub, now)
	if tokenErr := new(calDAVSyncTokenExpired); errors.As(err, &tokenErr) {
		if _, resetErr := s.db.Exec(ctx, `UPDATE calendar_subscriptions SET sync_token=NULL WHERE id=$1 AND sync_claimed_until=$2 AND sync_claimed_until>clock_timestamp()`, claim.ID, claim.ClaimedUntil); resetErr != nil {
			return s.retryClaim(ctx, claim, sub.RetryCount, resetErr)
		}
		sub.SyncToken = nil
		response, err = s.listCalDAVEvents(ctx, client, sub, now)
	}
	if err != nil {
		var providerError *Error
		if errors.As(err, &providerError) {
			switch providerError.Status {
			case 401:
				return s.invalidateCalDAVAccount(ctx, claim, sub.AccountID, err)
			case 404:
				return s.disableCalDAVSubscription(ctx, claim)
			}
		}
		return s.retryClaim(ctx, claim, sub.RetryCount, err)
	}
	return s.commitCalDAVSync(ctx, claim, sub, response)
}

func (s *Service) invalidateCalDAVAccount(ctx context.Context, claim calendarSyncClaim, accountID string, cause error) error {
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
	if _, err := tx.Exec(ctx, `DELETE FROM calendar_events WHERE subscription_id IN (SELECT id FROM calendar_subscriptions WHERE account_id=$1)`, accountID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE calendar_subscriptions SET sync_token=NULL,sync_claimed_until=NULL WHERE account_id=$1`, accountID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) disableCalDAVSubscription(ctx context.Context, claim calendarSyncClaim) error {
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
	if _, err := tx.Exec(ctx, `UPDATE calendar_subscriptions SET enabled=false,sync_token=NULL,sync_retry_count=0,sync_claimed_until=NULL,updated_at=clock_timestamp() WHERE id=$1`, claim.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM calendar_events WHERE subscription_id=$1`, claim.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) listCalDAVEvents(ctx context.Context, client *caldavClient, sub calDAVSyncSubscription, now time.Time) (calDAVSyncResponse, error) {
	calendarURL, err := url.Parse(sub.ExternalCalendarID)
	if err != nil || calendarURL.Scheme == "" || calendarURL.Host == "" {
		return calDAVSyncResponse{}, errors.New("CalDAV calendar URL is invalid")
	}
	token := ""
	if sub.SyncToken != nil {
		token = *sub.SyncToken
	}
	var body string
	if token != "" {
		var escaped strings.Builder
		if err := xml.EscapeText(&escaped, []byte(token)); err != nil {
			return calDAVSyncResponse{}, err
		}
		body = `<?xml version="1.0" encoding="UTF-8"?><D:sync-collection xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:sync-token>` + escaped.String() + `</D:sync-token><D:sync-level>1</D:sync-level><D:prop><D:getetag/><C:calendar-data/></D:prop></D:sync-collection>`
	} else {
		start := now.AddDate(0, 0, -90).UTC().Format("20060102T150405Z")
		end := now.AddDate(0, 0, 180).UTC().Format("20060102T150405Z")
		body = fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop><C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT"><C:time-range start="%s" end="%s"/></C:comp-filter></C:comp-filter></C:filter></C:calendar-query>`, start, end)
	}
	response, err := client.request(ctx, "REPORT", calendarURL.String(), "1", body)
	if err != nil {
		return calDAVSyncResponse{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := response.StatusCode
		response.Body.Close()
		if token != "" && (status == 403 || status == 404 || status == 409 || status == 410 || status == 501) {
			return calDAVSyncResponse{}, &calDAVSyncTokenExpired{status: status}
		}
		return calDAVSyncResponse{}, providerError(status)
	}
	root, err := decodeMultiStatus(response)
	if err != nil {
		return calDAVSyncResponse{}, err
	}
	multistatus := root
	if root.XMLName.Local != "multistatus" {
		multistatus = root.find("multistatus")
	}
	result := calDAVSyncResponse{Token: strings.TrimSpace(multistatus.child("sync-token").Text)}
	baseTimezone := "UTC"
	if sub.Timezone != nil && *sub.Timezone != "" {
		baseTimezone = *sub.Timezone
	}
	for _, item := range multistatus.Children {
		if item.XMLName.Local != "response" {
			continue
		}
		href := strings.TrimSpace(item.child("href").Text)
		if href == "" {
			continue
		}
		eventURL, parseErr := url.Parse(href)
		if parseErr != nil {
			continue
		}
		eventURL = calendarURL.ResolveReference(eventURL)
		if strings.Contains(item.child("status").Text, " 404 ") {
			result.Events = append(result.Events, calDAVEvent{ID: eventURL.String(), Status: "cancelled"})
			continue
		}
		prop := item.find("prop")
		ical := strings.TrimSpace(prop.child("calendar-data").Text)
		if ical == "" {
			continue
		}
		etag := strings.TrimSpace(prop.child("getetag").Text)
		events := parseCalDAVICalendar(ical, eventURL.String(), etag, baseTimezone)
		result.Events = append(result.Events, events...)
	}
	return result, nil
}

type icalProperty struct {
	value  string
	params map[string]string
}

func parseCalDAVICalendar(content, href, etag, timezone string) []calDAVEvent {
	lines := strings.FieldsFunc(content, func(r rune) bool { return r == '\r' || r == '\n' })
	unfolded := make([]string, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(unfolded) > 0 {
			unfolded[len(unfolded)-1] += line[1:]
		} else {
			unfolded = append(unfolded, line)
		}
	}
	events := make([]calDAVEvent, 0)
	var properties map[string][]icalProperty
	for _, line := range unfolded {
		if line == "BEGIN:VEVENT" {
			properties = make(map[string][]icalProperty)
			continue
		}
		if line == "END:VEVENT" {
			if event, ok := mapICalendarEvent(properties, href, etag, timezone, content); ok {
				events = append(events, event)
			}
			properties = nil
			continue
		}
		if properties == nil {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		parts := strings.Split(line[:colon], ";")
		name := strings.ToUpper(strings.TrimSpace(parts[0]))
		prop := icalProperty{value: line[colon+1:], params: make(map[string]string)}
		for _, part := range parts[1:] {
			key, value, ok := strings.Cut(part, "=")
			if ok {
				prop.params[strings.ToUpper(strings.TrimSpace(key))] = strings.Trim(strings.TrimSpace(value), `"`)
			}
		}
		properties[name] = append(properties[name], prop)
	}
	return events
}

func mapICalendarEvent(properties map[string][]icalProperty, href, etag, fallbackTimezone, ical string) (calDAVEvent, bool) {
	first := func(name string) icalProperty {
		if values := properties[name]; len(values) > 0 {
			return values[0]
		}
		return icalProperty{}
	}
	startProp := first("DTSTART")
	if startProp.value == "" {
		return calDAVEvent{}, false
	}
	start, allDay, zone, _, ok := parseICalendarTime(startProp, fallbackTimezone)
	if !ok {
		return calDAVEvent{}, false
	}
	endProp := first("DTEND")
	end, endAllDay := start, allDay
	if endProp.value != "" {
		var valid bool
		end, endAllDay, _, _, valid = parseICalendarTime(endProp, fallbackTimezone)
		if !valid {
			return calDAVEvent{}, false
		}
	}
	if end.Before(start) {
		return calDAVEvent{}, false
	}
	var recurrence *string
	if recurrenceProp := first("RECURRENCE-ID"); recurrenceProp.value != "" {
		_, _, _, recurrence, _ = parseICalendarTime(recurrenceProp, fallbackTimezone)
	}
	updatedProp := first("LAST-MODIFIED")
	if updatedProp.value == "" {
		updatedProp = first("DTSTAMP")
	}
	var updated *time.Time
	if updatedProp.value != "" {
		if parsed, _, _, _, valid := parseICalendarTime(updatedProp, "UTC"); valid {
			updated = &parsed
		}
	}
	uid := first("UID").value
	raw, _ := json.Marshal(map[string]any{"ical": ical, "href": href, "etag": etag, "uid": uid})
	status := strings.ToLower(first("STATUS").value)
	return calDAVEvent{
		ID: href, ETag: etag, Status: status,
		Summary:      unescapeICalendarText(first("SUMMARY").value),
		Description:  unescapeICalendarText(first("DESCRIPTION").value),
		Location:     unescapeICalendarText(first("LOCATION").value),
		RecurrenceID: recurrence, Timezone: optionalText(zone), Start: start, End: end,
		AllDay: allDay || endAllDay, Updated: updated, Raw: raw,
	}, true
}

func parseICalendarTime(value icalProperty, fallbackTimezone string) (time.Time, bool, string, *string, bool) {
	zone := value.params["TZID"]
	if zone == "" {
		zone = fallbackTimezone
	}
	if zone == "" {
		zone = "UTC"
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		location = time.UTC
		zone = "UTC"
	}
	if strings.EqualFold(value.params["VALUE"], "DATE") || (len(value.value) == 8 && !strings.Contains(value.value, "T")) {
		parsed, err := time.ParseInLocation("20060102", value.value, location)
		if err != nil {
			return time.Time{}, false, "", nil, false
		}
		recurrence := parsed.Format("2006-01-02")
		return parsed.UTC(), true, zone, &recurrence, true
	}
	formats := []string{"20060102T150405Z", "20060102T1504Z", "20060102T150405-0700", "20060102T1504-0700"}
	for _, format := range formats {
		if parsed, err := time.Parse(format, value.value); err == nil {
			return parsed.UTC(), false, zone, nil, true
		}
	}
	parsed, err := time.ParseInLocation("20060102T150405", value.value, location)
	if err != nil {
		parsed, err = time.ParseInLocation("20060102T1504", value.value, location)
	}
	if err != nil {
		return time.Time{}, false, "", nil, false
	}
	utc := parsed.UTC()
	recurrence := utc.Format(time.RFC3339)
	return utc, false, zone, &recurrence, true
}

func unescapeICalendarText(value string) string {
	replacer := strings.NewReplacer(`\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`)
	return replacer.Replace(value)
}

func (s *Service) commitCalDAVSync(ctx context.Context, claim calendarSyncClaim, sub calDAVSyncSubscription, response calDAVSyncResponse) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var claimUntil time.Time
	var active bool
	if err := tx.QueryRow(ctx, `SELECT sync_claimed_until,sync_claimed_until>clock_timestamp() FROM calendar_subscriptions WHERE id=$1 AND enabled FOR UPDATE`, claim.ID).Scan(&claimUntil, &active); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	if !active || !claimUntil.Equal(claim.ClaimedUntil) {
		return nil
	}
	now := time.Now().UTC()
	for _, event := range response.Events {
		if event.Status == "cancelled" {
			if _, err := tx.Exec(ctx, `DELETE FROM calendar_events WHERE subscription_id=$1 AND external_event_id=$2`, sub.ID, event.ID); err != nil {
				return err
			}
			continue
		}
		var found bool
		err := tx.QueryRow(ctx, `UPDATE calendar_events SET etag=$4,status=NULLIF($5,''),title=NULLIF($6,''),description=NULLIF($7,''),location=NULLIF($8,''),
			start_at_utc=$9,end_at_utc=$10,original_timezone=$11,all_day=$12,provider_updated_at=$13,raw=$14::jsonb,updated_at=$15
			WHERE subscription_id=$1 AND external_event_id=$2 AND recurrence_id IS NOT DISTINCT FROM $3 RETURNING true`,
			sub.ID, event.ID, event.RecurrenceID, nullableString(event.ETag), event.Status, nullableString(event.Summary), nullableString(event.Description),
			nullableString(event.Location), event.Start, event.End, event.Timezone, event.AllDay, event.Updated, event.Raw, now).Scan(&found)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := tx.Exec(ctx, `INSERT INTO calendar_events(id,subscription_id,external_event_id,recurrence_id,etag,status,title,description,location,
				start_at_utc,end_at_utc,original_timezone,all_day,provider_updated_at,raw,created_at,updated_at)
				VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10,$11,$12,$13,$14,$15::jsonb,$16,$16)`,
				uuid.NewString(), sub.ID, event.ID, event.RecurrenceID, nullableString(event.ETag), event.Status, nullableString(event.Summary), nullableString(event.Description),
				nullableString(event.Location), event.Start, event.End, event.Timezone, event.AllDay, event.Updated, event.Raw, now); err != nil {
				return err
			}
		}
	}
	interval := sub.RefreshInterval
	if interval < 1 {
		interval = 30
	}
	var nextToken any
	if response.Token != "" {
		nextToken = response.Token
	} else if sub.SyncToken != nil {
		nextToken = *sub.SyncToken
	}
	if _, err := tx.Exec(ctx, `UPDATE calendar_subscriptions SET sync_token=$3,last_sync_at=$4::timestamptz,next_sync_at=$4::timestamptz+($5::int*interval '1 minute'),
		sync_retry_count=0,sync_claimed_until=NULL,updated_at=$4::timestamptz WHERE id=$1 AND sync_claimed_until=$2`, claim.ID, claim.ClaimedUntil, nextToken, now, interval); err != nil {
		return fmt.Errorf("complete CalDAV sync state: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *Service) retryClaim(ctx context.Context, claim calendarSyncClaim, previous int, cause error) error {
	attempt := previous + 1
	if attempt < 1 {
		attempt = 1
	}
	delay := calendarSyncFailureBase * time.Duration(1<<min(attempt-1, 7))
	if delay > calendarSyncFailureMax {
		delay = calendarSyncFailureMax
	}
	if _, err := s.db.Exec(ctx, `UPDATE calendar_subscriptions SET sync_retry_count=$3,next_sync_at=clock_timestamp()+make_interval(secs=>$4),
		sync_claimed_until=NULL,updated_at=clock_timestamp() WHERE id=$1 AND sync_claimed_until=$2 AND sync_claimed_until>clock_timestamp()`,
		claim.ID, claim.ClaimedUntil, attempt, int(delay.Seconds())); err != nil {
		return fmt.Errorf("reschedule failed CalDAV sync: %w (provider error: %v)", err, cause)
	}
	return cause
}
