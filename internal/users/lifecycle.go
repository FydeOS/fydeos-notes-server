// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package users

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

var (
	ErrAccountLifecycleUnavailable = errors.New("user account lifecycle is unavailable")
	ErrCannotDeleteOwnAccount      = errors.New("cannot delete own account")
	ErrCannotDeleteTeamWorkspace   = errors.New("cannot delete account with owned team workspace")
)

type AccountRecord struct {
	ID              string
	Name            string
	Email           string
	EmailVerifiedAt *time.Time
	AvatarURL       *string
	CreatedAt       time.Time
	Password        *string
	Registered      bool
	Disabled        bool
	AuthEpoch       int
}

type OwnedWorkspace struct {
	ID   string
	Team bool
}

type AccountLifecycleRepository interface {
	GetAccountRecord(context.Context, string) (*AccountRecord, error)
	RevokeSessionsForDeletion(context.Context, string) error
	DeleteAccountForAdmin(context.Context, string) (*AccountRecord, error)
	BanAccountForAdmin(context.Context, string) (*AccountRecord, *User, error)
}

type AccountWorkspaceLifecycle interface {
	DeleteForUser(context.Context, string, string) ([]string, error)
}

type AccountWorkspaceObjectCleaner interface {
	DeleteWorkspaceObjects(context.Context, string, []string) (int64, error)
}

type AccountAvatarCleaner interface {
	DeleteByURL(context.Context, string) error
}

type AccountMailCanceler interface {
	CancelByRecipient(context.Context, string) (int64, error)
	CancelMemberInvitationByActor(context.Context, string) (int64, error)
}

type AccountLifecycleDependencies struct {
	Repository          AccountLifecycleRepository
	Workspaces          AccountWorkspaceLifecycle
	ListOwnedWorkspaces func(context.Context, string) ([]OwnedWorkspace, error)
	Objects             AccountWorkspaceObjectCleaner
	Avatars             AccountAvatarCleaner
	Mail                AccountMailCanceler
}

type AccountLifecycle interface {
	DeleteUser(context.Context, string) error
	BanUser(context.Context, string) (*User, error)
}

type AccountLifecycleService struct {
	dependencies AccountLifecycleDependencies
}

func NewAccountLifecycleService(dependencies AccountLifecycleDependencies) *AccountLifecycleService {
	return &AccountLifecycleService{dependencies: dependencies}
}

func (s *AccountLifecycleService) DeleteUser(ctx context.Context, userID string) error {
	_, _, err := s.remove(ctx, userID, false)
	return err
}

func (s *AccountLifecycleService) BanUser(ctx context.Context, userID string) (*User, error) {
	_, bannedUser, err := s.remove(ctx, userID, true)
	if err != nil {
		return nil, err
	}
	return bannedUser, nil
}

func (s *AccountLifecycleService) remove(ctx context.Context, userID string, ban bool) (*AccountRecord, *User, error) {
	dependencies := s.dependencies
	if dependencies.Repository == nil || dependencies.Workspaces == nil || dependencies.ListOwnedWorkspaces == nil || dependencies.Mail == nil || dependencies.Avatars == nil {
		return nil, nil, ErrAccountLifecycleUnavailable
	}
	if _, err := dependencies.Repository.GetAccountRecord(ctx, userID); err != nil {
		return nil, nil, err
	}
	workspaces, err := dependencies.ListOwnedWorkspaces(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	for _, workspace := range workspaces {
		if workspace.Team {
			return nil, nil, ErrCannotDeleteTeamWorkspace
		}
	}
	if len(workspaces) > 0 && (dependencies.Objects == nil) {
		return nil, nil, ErrAccountLifecycleUnavailable
	}
	if err := dependencies.Repository.RevokeSessionsForDeletion(ctx, userID); err != nil {
		return nil, nil, err
	}
	for _, workspace := range workspaces {
		userIDs, err := dependencies.Workspaces.DeleteForUser(ctx, workspace.ID, userID)
		if err != nil {
			return nil, nil, err
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if _, err := dependencies.Objects.DeleteWorkspaceObjects(cleanupCtx, workspace.ID, userIDs); err != nil {
			slog.WarnContext(ctx, "workspace object cleanup failed after account deletion", "workspace_id", workspace.ID)
		}
		cancel()
	}
	var deleted *AccountRecord
	var bannedUser *User
	if ban {
		deleted, bannedUser, err = dependencies.Repository.BanAccountForAdmin(ctx, userID)
	} else {
		deleted, err = dependencies.Repository.DeleteAccountForAdmin(ctx, userID)
	}
	if err != nil {
		return nil, nil, err
	}
	s.cleanupDeletedAccount(ctx, deleted)
	return deleted, bannedUser, nil
}

func (s *AccountLifecycleService) cleanupDeletedAccount(ctx context.Context, account *AccountRecord) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.dependencies.Mail.CancelByRecipient(cleanupCtx, account.Email); err != nil {
		slog.WarnContext(ctx, "recipient mail cleanup failed after account deletion", "user_id", account.ID)
	}
	if _, err := s.dependencies.Mail.CancelMemberInvitationByActor(cleanupCtx, account.ID); err != nil {
		slog.WarnContext(ctx, "invitation mail cleanup failed after account deletion", "user_id", account.ID)
	}
	if account.AvatarURL != nil {
		if err := s.dependencies.Avatars.DeleteByURL(cleanupCtx, *account.AvatarURL); err != nil {
			slog.WarnContext(ctx, "avatar cleanup failed after account deletion", "user_id", account.ID)
		}
	}
}
