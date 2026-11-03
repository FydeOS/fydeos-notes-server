// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/users"
)

const administratorFeatureName = "administrator"

func storedUserFeatureName(feature model.FeatureType) string {
	switch feature {
	case model.FeatureTypeAdmin:
		return administratorFeatureName
	default:
		return string(feature)
	}
}

func graphQLUserFeature(name string) (model.FeatureType, bool) {
	switch name {
	case administratorFeatureName:
		return model.FeatureTypeAdmin, true
	default:
		return "", false
	}
}

func requireAdministrator(ctx context.Context) error {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return ErrAdminAccessDenied
	}
	for _, feature := range principal.User.Features {
		if feature == administratorFeatureName {
			return nil
		}
	}
	return ErrAdminAccessDenied
}

func userListFilter(input model.ListUserInput) users.UserListFilter {
	filter := users.UserListFilter{Skip: 0, First: 20}
	if input.Skip != nil {
		filter.Skip = *input.Skip
	}
	if input.First != nil {
		filter.First = *input.First
	}
	if input.Keyword != nil {
		filter.Keyword = *input.Keyword
	}
	filter.Features = make([]string, 0, len(input.Features))
	for _, feature := range input.Features {
		filter.Features = append(filter.Features, storedUserFeatureName(feature))
	}
	return filter
}
