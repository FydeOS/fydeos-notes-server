// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package users

import (
	"context"
	"errors"
	"testing"
)

func TestDecodeUserSettingsDefaultsMissingFieldsAndDropsUnknownFields(t *testing.T) {
	settings, err := decodeUserSettings([]byte(`{"receiveMentionEmail":false,"legacy":1}`))
	if err != nil {
		t.Fatal(err)
	}
	want := UserSettings{ReceiveInvitationEmail: true, ReceiveMentionEmail: false, ReceiveCommentEmail: true}
	if settings != want {
		t.Fatalf("settings = %+v, want %+v", settings, want)
	}
}

func TestDecodeUserSettingsRejectsInvalidValues(t *testing.T) {
	for _, payload := range []string{`null`, `[]`, `{"receiveMentionEmail":null}`, `{"receiveCommentEmail":1}`} {
		if _, err := decodeUserSettings([]byte(payload)); !errors.Is(err, ErrInvalidSettings) {
			t.Errorf("decodeUserSettings(%s) error = %v, want ErrInvalidSettings", payload, err)
		}
	}
}

type settingsStoreStub struct {
	gotUserID string
	update    UserSettingsUpdate
	settings  UserSettings
	err       error
}

func (s *settingsStoreStub) GetSettings(_ context.Context, userID string) (UserSettings, error) {
	s.gotUserID = userID
	return s.settings, s.err
}

func (s *settingsStoreStub) UpdateSettings(_ context.Context, userID string, update UserSettingsUpdate) (UserSettings, error) {
	s.gotUserID, s.update = userID, update
	return s.settings, s.err
}

func TestServiceDelegatesUserSettingsToStore(t *testing.T) {
	store := &settingsStoreStub{settings: UserSettings{ReceiveInvitationEmail: true}}
	service := NewService(settingsReaderStub{}, nil)
	service.users = combinedSettingsReader{Reader: settingsReaderStub{}, SettingsStore: store}

	settings, err := service.GetSettings(context.Background(), "user-1")
	if err != nil || settings != store.settings || store.gotUserID != "user-1" {
		t.Fatalf("GetSettings() = (%+v, %v), userID = %q", settings, err, store.gotUserID)
	}

	mentioned := false
	update := UserSettingsUpdate{ReceiveMentionEmail: &mentioned}
	settings, err = service.UpdateSettings(context.Background(), "user-2", update)
	if err != nil || settings != store.settings || store.gotUserID != "user-2" || store.update.ReceiveMentionEmail != &mentioned {
		t.Fatalf("UpdateSettings() = (%+v, %v), userID/update = %q/%+v", settings, err, store.gotUserID, store.update)
	}
}

type settingsReaderStub struct{}

func (settingsReaderStub) Get(context.Context, string) (*User, error)        { return nil, nil }
func (settingsReaderStub) GetPublic(context.Context, string) (*User, error)  { return nil, nil }
func (settingsReaderStub) GetByEmail(context.Context, string) (*User, error) { return nil, nil }

type combinedSettingsReader struct {
	Reader
	SettingsStore
}
