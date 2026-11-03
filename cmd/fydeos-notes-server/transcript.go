// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"os"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/copilot"
	"github.com/fydeos/fydeos-notes-server/internal/database"
	"github.com/fydeos/fydeos-notes-server/internal/mcpcredentials"
	"github.com/fydeos/fydeos-notes-server/internal/search"
	"github.com/fydeos/fydeos-notes-server/internal/storage"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

func newTranscriptRuntime(db *database.Pool, cfg config.Config, objects *storage.Runtime, publisher copilot.TranscriptTaskChangePublisher) (*copilot.TranscriptTasks, *copilot.TranscriptTaskWorker) {
	profiles := byok.NewProfileStore(db, cfg.RuntimeConfigPath, cfg.Deployment, func(ctx context.Context) (string, error) {
		return auth.LoadAppPrivateKey(ctx, db, cfg, os.Getenv("AFFINE_PRIVATE_KEY"))
	})
	feature := mcpcredentials.NewService(db, cfg)
	authorization := workspaces.NewAuthorizationService(workspaces.NewRepositoryWithEntitlementConfig(db, cfg.Deployment))
	authorize := func(ctx context.Context, userID, workspaceID string, personal bool) error {
		enabled, err := feature.CopilotEnabled(ctx)
		if err != nil {
			return err
		}
		if !enabled {
			return errors.New("copilot is disabled")
		}
		var userActive, workspaceExists bool
		if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND disabled=false), EXISTS(SELECT 1 FROM workspaces WHERE id=$2)`, userID, workspaceID).Scan(&userActive, &workspaceExists); err != nil {
			return err
		}
		if !userActive || personal == workspaceExists {
			return workspaces.ErrAccessDenied
		}
		if personal {
			return nil
		}
		access, err := authorization.GetWorkspaceRolePermissions(ctx, userID, workspaceID)
		if err != nil {
			return err
		}
		if !access.Permissions["Workspace.Copilot"] {
			return workspaces.ErrAccessDenied
		}
		return nil
	}
	targets := copilot.NewTranscriptRoutes(db, cfg, profiles)
	executor := copilot.NewTranscriptExecutor(objects, targets, authorize)
	executor.SetUsageRecorder(copilot.NewTranscriptUsageLedger(db))
	tasks := copilot.NewTranscriptTasks(db, objects)
	tasks.SetPreflight(executor.Preflight)
	return tasks, copilot.NewTranscriptTaskWorker(tasks, executor, publisher)
}

func newEmbeddingCoordinator(db *database.Pool, cfg config.Config) *search.EmbeddingCoordinator {
	return search.NewEmbeddingCoordinator(db, newEmbeddingTargetResolver(db, cfg))
}

func newEmbeddingTargetResolver(db *database.Pool, cfg config.Config) *copilot.TranscriptRoutes {
	profiles := byok.NewProfileStore(db, cfg.RuntimeConfigPath, cfg.Deployment, func(ctx context.Context) (string, error) {
		return auth.LoadAppPrivateKey(ctx, db, cfg, os.Getenv("AFFINE_PRIVATE_KEY"))
	})
	return copilot.NewTranscriptRoutes(db, cfg, profiles)
}
