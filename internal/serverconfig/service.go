// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

// Package serverconfig owns public server settings and process initialization state.
package serverconfig

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

var ErrUnavailable = errors.New("server configuration is unavailable")

type Service struct {
	config      config.Config
	reader      config.RuntimeConfigReader
	mu          sync.Mutex
	initialized bool
}

type CalendarCalDAVProviderPreset struct {
	ID                  string  `json:"id"`
	Label               string  `json:"label"`
	RequiresAppPassword *bool   `json:"requiresAppPassword"`
	DocsURL             *string `json:"docsUrl"`
}

type calDAVConfig struct {
	Enabled   bool                           `json:"enabled"`
	Providers []CalendarCalDAVProviderPreset `json:"providers"`
}

func New(cfg config.Config, reader config.RuntimeConfigReader) *Service {
	return &Service{config: cfg, reader: reader}
}

func (s *Service) Name(ctx context.Context) (string, error) {
	raw, found, err := config.ReadNodeRuntimeValue(ctx, s.reader, s.config.RuntimeConfigPath, "server.name")
	if err != nil {
		return "", ErrUnavailable
	}
	if found {
		var name *string
		if err := json.Unmarshal(raw, &name); err != nil {
			return "", errors.New("invalid server name configuration")
		}
		if name != nil {
			return NormalizeDisplayName(*name), nil
		}
	}
	if s.config.Deployment != "affine" {
		return "FydeOS Notes", nil
	}
	return "FydeOS Notes Cloud", nil
}

func NormalizeDisplayName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "affine self-hosted":
		return "FydeOS Notes"
	case "affine cloud", "affine beta cloud", "affine canary cloud":
		return "FydeOS Notes Cloud"
	default:
		return name
	}
}

// Initialized retries a negative result and latches a positive one for this
// process, matching the legacy server even if its last user is later removed.
func (s *Service) Initialized(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initialized {
		return true, nil
	}
	if s.reader == nil {
		return false, errors.New("server initialization database unavailable")
	}
	var present bool
	if err := s.reader.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users LIMIT 1)`).Scan(&present); err != nil {
		return false, ErrUnavailable
	}
	s.initialized = present
	return present, nil
}

func (s *Service) LocalWorkspaceEnabled(ctx context.Context) (bool, error) {
	raw, found, err := config.ReadNodeRuntimeValue(ctx, s.reader, s.config.RuntimeConfigPath, "flags.allowGuestDemoWorkspace")
	if err != nil {
		return false, ErrUnavailable
	}
	if !found {
		return true, nil
	}
	var enabled bool
	if err := json.Unmarshal(raw, &enabled); err != nil {
		return false, errors.New("invalid guest workspace configuration")
	}
	return enabled, nil
}

func (s *Service) IndexerEnabled(ctx context.Context) (bool, error) {
	raw, found, err := config.ReadRuntimeValue(ctx, s.reader, s.config.RuntimeConfigPath, "indexer.enabled")
	if err != nil {
		return false, ErrUnavailable
	}
	if !found {
		return false, nil
	}
	var enabled bool
	if err := json.Unmarshal(raw, &enabled); err != nil {
		return false, errors.New("invalid indexer configuration")
	}
	return enabled, nil
}

func (s *Service) PasswordLimits(ctx context.Context) (int, int, error) {
	raw, found, err := config.ReadNodeRuntimeValue(ctx, s.reader, s.config.RuntimeConfigPath, "auth.passwordRequirements")
	if err != nil {
		return 0, 0, ErrUnavailable
	}
	if !found {
		return 8, 32, nil
	}
	var limits struct {
		Min *int `json:"min"`
		Max *int `json:"max"`
	}
	if err := json.Unmarshal(raw, &limits); err != nil || limits.Min == nil || limits.Max == nil {
		return 0, 0, errors.New("invalid password requirements configuration")
	}
	return *limits.Min, *limits.Max, nil
}

func (s *Service) CalendarCalDAVProviders(ctx context.Context) ([]CalendarCalDAVProviderPreset, error) {
	raw, found, err := config.ReadNodeRuntimeValue(ctx, s.reader, s.config.RuntimeConfigPath, "calendar.caldav")
	if err != nil {
		return nil, ErrUnavailable
	}
	if !found {
		return []CalendarCalDAVProviderPreset{}, nil
	}
	var settings calDAVConfig
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, errors.New("invalid CalDAV provider configuration")
	}
	if !settings.Enabled {
		return []CalendarCalDAVProviderPreset{}, nil
	}
	if settings.Providers == nil {
		return []CalendarCalDAVProviderPreset{}, nil
	}
	return settings.Providers, nil
}
