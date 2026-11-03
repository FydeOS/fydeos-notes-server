// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package mcpcredentials

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

func TestCreateValidationMatchesLegacyContract(t *testing.T) {
	service := NewService(nil, config.Config{NodeEnv: "production", Namespace: "production"})

	for _, test := range []struct {
		name  string
		input CreateInput
		want  error
	}{
		{name: "unsupported expiration", input: CreateInput{UserID: "user", WorkspaceID: "workspace", Name: "desktop", AccessMode: "READ_ONLY", ExpirationDays: 31}, want: ErrInvalidExpiration},
		{name: "blank name", input: CreateInput{UserID: "user", WorkspaceID: "workspace", Name: " \t ", AccessMode: "READ_ONLY", ExpirationDays: 90}, want: ErrInvalidName},
		{name: "name over 64 UTF-16 units", input: CreateInput{UserID: "user", WorkspaceID: "workspace", Name: strings.Repeat("😀", 33), AccessMode: "READ_ONLY", ExpirationDays: 90}, want: ErrInvalidName},
		{name: "invalid access mode", input: CreateInput{UserID: "user", WorkspaceID: "workspace", Name: "desktop", AccessMode: "ADMIN", ExpirationDays: 90}, want: ErrInvalidAccessMode},
		{name: "valid 64 UTF-16 units", input: CreateInput{UserID: "user", WorkspaceID: "workspace", Name: strings.Repeat("😀", 32), AccessMode: "READ_ONLY", ExpirationDays: 90}, want: ErrUnavailable},
		{name: "trim name before limit", input: CreateInput{UserID: "user", WorkspaceID: "workspace", Name: "  desktop  ", AccessMode: "READ_ONLY", ExpirationDays: 90}, want: ErrUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.Create(t.Context(), test.input)
			if !errors.Is(err, test.want) {
				t.Fatalf("Create() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestReadWriteAvailabilityUsesLegacyEnvironmentBoundary(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  config.Config
		want bool
	}{
		{name: "development node env", cfg: config.Config{NodeEnv: "development", Namespace: "production"}, want: true},
		{name: "canary namespace mapping", cfg: config.Config{NodeEnv: "production", Namespace: "dev"}, want: true},
		{name: "production", cfg: config.Config{NodeEnv: "production", Namespace: "production"}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := NewService(nil, test.cfg).ReadWriteAvailable(); got != test.want {
				t.Fatalf("ReadWriteAvailable() = %t, want %t", got, test.want)
			}
		})
	}

	production := NewService(nil, config.Config{NodeEnv: "production", Namespace: "production"})
	_, err := production.Create(t.Context(), CreateInput{UserID: "user", WorkspaceID: "workspace", Name: "write", AccessMode: "READ_WRITE", ExpirationDays: 90})
	if !errors.Is(err, ErrWriteToolsUnavailable) {
		t.Fatalf("production READ_WRITE create error = %v, want %v", err, ErrWriteToolsUnavailable)
	}
}

func TestCredentialStatusPrecedenceAndBoundaries(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	if got := status(Credential{ExpiresAt: now.Add(8 * 24 * time.Hour)}, now); got != "ACTIVE" {
		t.Fatalf("status before expiring window = %q, want ACTIVE", got)
	}
	if got := status(Credential{ExpiresAt: now.Add(7 * 24 * time.Hour)}, now); got != "EXPIRING" {
		t.Fatalf("status at expiring boundary = %q, want EXPIRING", got)
	}
	if got := status(Credential{ExpiresAt: now}, now); got != "EXPIRED" {
		t.Fatalf("status at expiry = %q, want EXPIRED", got)
	}
	revokedAt := now.Add(-time.Minute)
	if got := status(Credential{ExpiresAt: now.Add(time.Hour), RevokedAt: &revokedAt}, now); got != "REVOKED" {
		t.Fatalf("revoked status = %q, want REVOKED", got)
	}
	graceEnd := now.Add(time.Hour)
	if got := status(Credential{ExpiresAt: now.Add(time.Hour), GraceEndsAt: &graceEnd}, now); got != "ROTATING" {
		t.Fatalf("rotating status = %q, want ROTATING", got)
	}
}
