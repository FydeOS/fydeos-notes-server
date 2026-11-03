// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
)

var ErrAuthSigningKeysUnavailable = errors.New("auth signing key management is unavailable")

func (r *Resolver) authSigningKeys(ctx context.Context) ([]*model.AuthSigningKeyType, error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if r.AuthSigningKeys == nil {
		return nil, ErrAuthSigningKeysUnavailable
	}
	keys, err := r.AuthSigningKeys.Metadata(ctx)
	if err != nil {
		return nil, err
	}
	return authSigningKeyModels(keys), nil
}

func (r *Resolver) rotateAuthSigningKey(ctx context.Context, expectedActiveKeyID string) ([]*model.AuthSigningKeyType, error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if r.AuthSigningKeys == nil {
		return nil, ErrAuthSigningKeysUnavailable
	}
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAdminAccessDenied
	}
	keys, err := r.AuthSigningKeys.Rotate(ctx, principal.User.ID, expectedActiveKeyID)
	if err != nil {
		return nil, err
	}
	return authSigningKeyModels(keys), nil
}

func (r *Resolver) deleteAuthSigningKey(ctx context.Context, keyID string) ([]*model.AuthSigningKeyType, error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if r.AuthSigningKeys == nil {
		return nil, ErrAuthSigningKeysUnavailable
	}
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAdminAccessDenied
	}
	keys, err := r.AuthSigningKeys.Delete(ctx, principal.User.ID, keyID)
	if err != nil {
		return nil, err
	}
	return authSigningKeyModels(keys), nil
}

func authSigningKeyModels(keys []auth.AuthSigningKeyMetadata) []*model.AuthSigningKeyType {
	result := make([]*model.AuthSigningKeyType, 0, len(keys))
	for _, key := range keys {
		result = append(result, &model.AuthSigningKeyType{
			ID: key.ID, Status: key.Status, Source: key.Source,
			CreatedAt: signingKeyTime(key.CreatedAt), RetiredAt: signingKeyTime(key.RetiredAt),
			VerifyUntil: signingKeyTime(key.VerifyUntil), CanDelete: key.CanDelete,
		})
	}
	return result
}

func signingKeyTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}
