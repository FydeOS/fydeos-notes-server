// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package users

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/fydeos/fydeos-notes-server/internal/password"
)

type Reader interface {
	Get(context.Context, string) (*User, error)
	GetPublic(context.Context, string) (*User, error)
	GetByEmail(context.Context, string) (*User, error)
}

type ProfileUpdater interface {
	UpdateProfile(context.Context, string, string) (*User, error)
}

type AvatarUpdater interface {
	UpdateAvatar(context.Context, string, *string) (*User, error)
}

type SettingsStore interface {
	GetSettings(context.Context, string) (UserSettings, error)
	UpdateSettings(context.Context, string, UserSettingsUpdate) (UserSettings, error)
}

type Visibility interface {
	CanViewUser(context.Context, string, string) (bool, error)
}

type Service struct {
	users      Reader
	visibility Visibility
	lifecycle  AccountLifecycle
}

type UserManagementReader interface {
	GetByEmailIncludingDisabled(context.Context, string) (*User, error)
	List(context.Context, UserListFilter) ([]*User, error)
	Count(context.Context, UserListFilter) (int, error)
}

type UserFeatureManager interface {
	UpdateFeatures(context.Context, string, []string) (bool, error)
}

type UserStatusManager interface {
	Enable(context.Context, string) (*User, error)
}

type AdminEmailUpdater interface {
	UpdateEmail(context.Context, string, string) (*User, error)
}

type UserCreator interface {
	Create(context.Context, string, string, *string) (*User, error)
}

type CreateInput struct {
	Email    string
	Name     *string
	Password *string
}

type AdminUpdateInput struct {
	Email *string
	Name  *string
}

var adminEmailPattern = regexp.MustCompile(`(?i)^[a-z0-9_'+.-]*[a-z0-9_+-]@([a-z0-9][a-z0-9-]*\.)+[a-z]{2,}$`)

func NewService(users Reader, visibility Visibility, lifecycle ...AccountLifecycle) *Service {
	service := &Service{users: users, visibility: visibility}
	if len(lifecycle) > 0 {
		service.lifecycle = lifecycle[0]
	}
	return service
}

func (s *Service) Get(ctx context.Context, id string) (*User, error) {
	return s.users.Get(ctx, id)
}

func (s *Service) GetAdminByID(ctx context.Context, id string) (*User, error) {
	return s.users.Get(ctx, id)
}

func (s *Service) GetAdminByEmail(ctx context.Context, email string) (*User, error) {
	reader, ok := s.users.(UserManagementReader)
	if !ok {
		return nil, ErrUserManagementUnavailable
	}
	return reader.GetByEmailIncludingDisabled(ctx, email)
}

func (s *Service) ListUsers(ctx context.Context, filter UserListFilter) ([]*User, error) {
	reader, ok := s.users.(UserManagementReader)
	if !ok {
		return nil, ErrUserManagementUnavailable
	}
	return reader.List(ctx, filter)
}

func (s *Service) CountUsers(ctx context.Context, filter UserListFilter) (int, error) {
	reader, ok := s.users.(UserManagementReader)
	if !ok {
		return 0, ErrUserManagementUnavailable
	}
	return reader.Count(ctx, filter)
}

func (s *Service) UpdateUserFeatures(ctx context.Context, userID string, features []string) (bool, error) {
	manager, ok := s.users.(UserFeatureManager)
	if !ok {
		return false, ErrUserManagementUnavailable
	}
	return manager.UpdateFeatures(ctx, userID, features)
}

func (s *Service) EnableUser(ctx context.Context, userID string) (*User, error) {
	manager, ok := s.users.(UserStatusManager)
	if !ok {
		return nil, ErrUserStatusUnavailable
	}
	return manager.Enable(ctx, userID)
}

func (s *Service) DeleteAdminUser(ctx context.Context, userID string) error {
	return s.deleteAccount(ctx, userID)
}

func (s *Service) DeleteOwnAccount(ctx context.Context, userID string) error {
	return s.deleteAccount(ctx, userID)
}

func (s *Service) deleteAccount(ctx context.Context, userID string) error {
	if s.lifecycle == nil {
		return ErrAccountLifecycleUnavailable
	}
	return s.lifecycle.DeleteUser(ctx, userID)
}

func (s *Service) BanAdminUser(ctx context.Context, userID string) (*User, error) {
	if s.lifecycle == nil {
		return nil, ErrAccountLifecycleUnavailable
	}
	return s.lifecycle.BanUser(ctx, userID)
}

func (s *Service) UpdateAdminUser(ctx context.Context, userID string, input AdminUpdateInput) (*User, bool, error) {
	user, err := s.users.Get(ctx, userID)
	if err != nil {
		return nil, false, err
	}
	publishUpdate := false
	if input.Email != nil && *input.Email != "" && *input.Email != user.Email {
		if err := validateAdminEmail(*input.Email); err != nil {
			return nil, false, err
		}
		updater, ok := s.users.(AdminEmailUpdater)
		if !ok {
			return nil, false, ErrUserManagementUnavailable
		}
		user, err = updater.UpdateEmail(ctx, userID, *input.Email)
		if err != nil {
			return nil, false, err
		}
		publishUpdate = true
	}
	if input.Email != nil && *input.Email != "" && input.Name == nil {
		publishUpdate = true
	}
	if input.Name != nil {
		updater, ok := s.users.(ProfileUpdater)
		if !ok {
			return nil, false, ErrProfileUpdateUnavailable
		}
		user, err = updater.UpdateProfile(ctx, userID, *input.Name)
		if err != nil {
			return nil, false, err
		}
		publishUpdate = true
	}
	return user, publishUpdate, nil
}

func validateAdminEmail(value string) error {
	local, _, hasDomain := strings.Cut(value, "@")
	if len(value) > 320 || !hasDomain || strings.HasPrefix(local, ".") || strings.Contains(local, "..") || !adminEmailPattern.MatchString(value) {
		return &InvalidEmailError{Email: value}
	}
	return nil
}

type InvalidEmailError struct {
	Email string
}

func (e *InvalidEmailError) Error() string { return "invalid email" }

func (e *InvalidEmailError) Is(target error) bool { return target == ErrInvalidEmail }

func (s *Service) CreateAdminUser(ctx context.Context, input CreateInput) (*User, error) {
	lookup, ok := s.users.(UserManagementReader)
	if !ok {
		return nil, ErrUserManagementUnavailable
	}
	existing, err := lookup.GetByEmailIncludingDisabled(ctx, input.Email)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, ErrEmailAlreadyUsed
	}
	creator, ok := s.users.(UserCreator)
	if !ok {
		return nil, ErrUserManagementUnavailable
	}
	name := strings.Split(input.Email, "@")[0]
	if input.Name != nil {
		name = *input.Name
	}
	var passwordHash *string
	if input.Password != nil {
		hashed, err := password.Hash(*input.Password)
		if err != nil {
			return nil, err
		}
		passwordHash = &hashed
	}
	return creator.Create(ctx, input.Email, name, passwordHash)
}

func (s *Service) GetPublic(ctx context.Context, id string) (*User, error) {
	return s.users.GetPublic(ctx, id)
}

func (s *Service) UpdateProfile(ctx context.Context, userID string, name *string) (*User, error) {
	if name == nil {
		return s.users.Get(ctx, userID)
	}
	updater, ok := s.users.(ProfileUpdater)
	if !ok {
		return nil, ErrProfileUpdateUnavailable
	}
	return updater.UpdateProfile(ctx, userID, *name)
}

func (s *Service) UpdateAvatar(ctx context.Context, userID string, avatarURL *string) (*User, error) {
	updater, ok := s.users.(AvatarUpdater)
	if !ok {
		return nil, ErrProfileUpdateUnavailable
	}
	return updater.UpdateAvatar(ctx, userID, avatarURL)
}

func (s *Service) GetSettings(ctx context.Context, userID string) (UserSettings, error) {
	store, ok := s.users.(SettingsStore)
	if !ok {
		return UserSettings{}, ErrSettingsUnavailable
	}
	return store.GetSettings(ctx, userID)
}

func (s *Service) UpdateSettings(ctx context.Context, userID string, update UserSettingsUpdate) (UserSettings, error) {
	store, ok := s.users.(SettingsStore)
	if !ok {
		return UserSettings{}, ErrSettingsUnavailable
	}
	return store.UpdateSettings(ctx, userID, update)
}

func (s *Service) GetVisibleByEmail(ctx context.Context, email, viewerID string) (*User, error) {
	target, err := s.users.GetByEmail(ctx, email)
	if errors.Is(err, ErrNotFound) || (err == nil && target == nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	allowed, err := s.visibility.CanViewUser(ctx, viewerID, target.ID)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, nil
	}
	return target, nil
}
