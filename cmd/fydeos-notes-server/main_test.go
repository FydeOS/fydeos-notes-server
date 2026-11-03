// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

func TestValidateServerFlavorSupportsImplementedRolesAndFailsClosedOtherwise(t *testing.T) {
	for flavor, wantError := range map[string]bool{
		"allinone": false,
		"graphql":  false,
		"sync":     false,
		"front":    false,
		"worker":   false,
		"script":   true,
		"renderer": false,
	} {
		t.Run(flavor, func(t *testing.T) {
			err := validateServerFlavor(flavor)
			if (err != nil) != wantError {
				t.Fatalf("validateServerFlavor(%q) error = %v, wantError %v", flavor, err, wantError)
			}
		})
	}
}

func TestRuntimeStaticRootUsesExecutableThenWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	executableDir := filepath.Join(root, "bin")
	workingDir := filepath.Join(root, "work")
	if err := os.MkdirAll(filepath.Join(executableDir, "static"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workingDir, "static"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := runtimeStaticRoot(filepath.Join(executableDir, "fydeos-notes-server"), workingDir); got != filepath.Join(executableDir, "static") {
		t.Fatalf("runtime static root = %q, want executable sibling", got)
	}
	if err := os.RemoveAll(filepath.Join(executableDir, "static")); err != nil {
		t.Fatal(err)
	}
	if got := runtimeStaticRoot(filepath.Join(executableDir, "fydeos-notes-server"), workingDir); got != filepath.Join(workingDir, "static") {
		t.Fatalf("runtime static root = %q, want working directory fallback", got)
	}
}

func TestLoadAvatarPublicPathUsesDefaultAndRuntimeConfig(t *testing.T) {
	ctx := context.Background()
	if got, err := loadAvatarPublicPath(ctx, nil, config.Config{}); err != nil || got != "/api/avatars/" {
		t.Fatalf("default avatar public path = %q, %v", got, err)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"storages":{"avatar.publicPath":"/assets/avatars/"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadAvatarPublicPath(ctx, nil, config.Config{RuntimeConfigPath: configPath})
	if err != nil || got != "/assets/avatars/" {
		t.Fatalf("configured avatar public path = %q, %v", got, err)
	}
}
