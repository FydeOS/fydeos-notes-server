// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package users

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrInvalidSettings = errors.New("invalid user settings")

type UserSettings struct {
	ReceiveInvitationEmail bool `json:"receiveInvitationEmail"`
	ReceiveMentionEmail    bool `json:"receiveMentionEmail"`
	ReceiveCommentEmail    bool `json:"receiveCommentEmail"`
}

type UserSettingsUpdate struct {
	ReceiveInvitationEmail *bool
	ReceiveMentionEmail    *bool
	ReceiveCommentEmail    *bool
}

func defaultUserSettings() UserSettings {
	return UserSettings{
		ReceiveInvitationEmail: true,
		ReceiveMentionEmail:    true,
		ReceiveCommentEmail:    true,
	}
}

func decodeUserSettings(payload []byte) (UserSettings, error) {
	settings := defaultUserSettings()
	var values map[string]json.RawMessage
	if err := json.Unmarshal(payload, &values); err != nil || values == nil {
		return UserSettings{}, fmt.Errorf("%w: payload must be an object", ErrInvalidSettings)
	}
	for key, target := range map[string]*bool{
		"receiveInvitationEmail": &settings.ReceiveInvitationEmail,
		"receiveMentionEmail":    &settings.ReceiveMentionEmail,
		"receiveCommentEmail":    &settings.ReceiveCommentEmail,
	} {
		value, ok := values[key]
		if !ok {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return UserSettings{}, fmt.Errorf("%w: %s must be a boolean", ErrInvalidSettings, key)
		}
		if err := json.Unmarshal(value, target); err != nil {
			return UserSettings{}, fmt.Errorf("%w: %s must be a boolean", ErrInvalidSettings, key)
		}
	}
	return settings, nil
}
