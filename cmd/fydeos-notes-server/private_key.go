// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"os"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/database"
)

func runEnsurePrivateKey() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	bootstrap, err := auth.LoadAppPrivateKey(ctx, db, cfg, os.Getenv("AFFINE_PRIVATE_KEY"))
	if err != nil {
		return err
	}
	return auth.EnsureStablePrivateKey(ctx, db, auth.PrivateKeyWrappingSecret(), bootstrap)
}
