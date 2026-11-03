// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import (
	"context"
	"errors"

	"github.com/fydeos/fydeos-notes-server/internal/users"
)

var errUserSettingsUnavailable = errors.New("user settings reader is unavailable")

func (s *Server) SetUserSettingsReader(reader UserSettingsReader) {
	if s != nil {
		s.userSettingsReader = reader
	}
}

func (s *Server) userSettings(ctx context.Context, userID string) (users.UserSettings, error) {
	if s == nil || s.userSettingsReader == nil {
		return users.UserSettings{}, errUserSettingsUnavailable
	}
	return s.userSettingsReader.GetSettings(ctx, userID)
}
