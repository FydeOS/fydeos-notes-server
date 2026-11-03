// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package calendar

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCalendarTokenEnvelopeMatchesCryptoHelper(t *testing.T) {
	const key, value = "runtime-private-key", "calendar-access-token"
	encrypted, err := encryptCalendarToken(key, value)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(digest[:])
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCMWithTagSize(block, 12)
	if err != nil {
		t.Fatal(err)
	}
	nonce := encoded[:aead.NonceSize()]
	tag := encoded[aead.NonceSize() : aead.NonceSize()+aead.Overhead()]
	ciphertext := encoded[aead.NonceSize()+aead.Overhead():]
	opened, err := aead.Open(nil, nonce, append(append([]byte(nil), ciphertext...), tag...), nil)
	if err != nil || string(opened) != value {
		t.Fatalf("CryptoHelper envelope opened as %q, err=%v", opened, err)
	}
}

func TestCalendarTokenDecryptReadsPriorGoEnvelope(t *testing.T) {
	const key, value = "runtime-private-key", "old-go-calendar-token"
	encrypted, err := encryptCredential(key, value)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := decryptCalendarToken(key, encrypted)
	if err != nil || decrypted != value {
		t.Fatalf("decrypted=%q err=%v", decrypted, err)
	}
}

func TestGoogleCalendarAllDayUsesEventTimezone(t *testing.T) {
	event, err := mapGoogleCalendarEvent(googleCalendarEvent{
		ID:    "all-day",
		Start: googleCalendarEventTime{Date: "2026-03-08", TimeZone: "America/Los_Angeles"},
		End:   googleCalendarEventTime{Date: "2026-03-09", TimeZone: "America/Los_Angeles"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !event.AllDay || event.Start.Format(time.RFC3339) != "2026-03-08T08:00:00Z" || event.End.Format(time.RFC3339) != "2026-03-09T07:00:00Z" {
		t.Fatalf("all-day event mapped to %#v", event)
	}
}

func TestGoogleCalendarRecurringOverrideKeepsOriginalStartIdentity(t *testing.T) {
	recurrenceID := "2026-03-08T09:30:00-07:00"
	event, err := mapGoogleCalendarEvent(googleCalendarEvent{
		ID:            "occurrence-id",
		OriginalStart: googleCalendarEventTime{DateTime: recurrenceID},
		Start:         googleCalendarEventTime{DateTime: "2026-03-08T10:30:00-07:00"},
		End:           googleCalendarEventTime{DateTime: "2026-03-08T11:30:00-07:00"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if event.RecurrenceID == nil || *event.RecurrenceID != recurrenceID {
		t.Fatalf("recurrence ID = %v, want %q", event.RecurrenceID, recurrenceID)
	}
	if event.Start.UTC().Format(time.RFC3339) != "2026-03-08T17:30:00Z" {
		t.Fatalf("recurring override start = %s", event.Start.UTC())
	}
}

func TestGoogleCalendarListEventsPaginatesAndKeepsRawEvent(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.EscapedPath() != "/calendars/team%2Fprimary@example.test/events" {
			t.Errorf("calendar ID path escaping = %q", r.URL.EscapedPath())
		}
		if r.Header.Get("Authorization") != "Bearer access" || r.URL.Query().Get("singleEvents") != "true" || r.URL.Query().Get("showDeleted") != "true" || r.URL.Query().Get("syncToken") != "cursor" {
			t.Errorf("calendar events request=%v headers=%v", r.URL, r.Header)
		}
		if requests == 1 {
			_, _ = w.Write([]byte(`{"items":[{"id":"one","summary":"First","vendorField":"kept","start":{"dateTime":"2026-01-01T10:00:00Z"},"end":{"dateTime":"2026-01-01T11:00:00Z"}}],"nextPageToken":"page-2"}`))
			return
		}
		if r.URL.Query().Get("pageToken") != "page-2" {
			t.Errorf("second page query=%v", r.URL.Query())
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"two","status":"cancelled","start":{"dateTime":"2026-01-02T10:00:00Z"},"end":{"dateTime":"2026-01-02T11:00:00Z"}}],"nextSyncToken":"next-cursor"}`))
	}))
	defer server.Close()
	linker := NewGoogleAccountLinkerWithEndpoints(nil, "", nil, "", GoogleEndpoints{EventsURL: server.URL + "/calendars"})
	token := "cursor"
	response, err := linker.listEvents(context.Background(), "access", "team/primary@example.test", &token, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(response.Events) != 2 || response.NextSyncToken != "next-cursor" || !strings.Contains(string(response.Events[0].Raw), `"vendorField":"kept"`) {
		t.Fatalf("requests=%d response=%#v", requests, response)
	}
}
