// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"

	"github.com/fydeos/fydeos-notes-server/internal/appconfig"
	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
)

var ErrAppConfigUnavailable = errors.New("app config administration is unavailable")

func (r *Resolver) appConfig(ctx context.Context) (model.JSONObject, error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if r.AppConfig == nil {
		return nil, ErrAppConfigUnavailable
	}
	snapshot, err := r.AppConfig.Read(ctx)
	if err != nil {
		return nil, ErrAppConfigUnavailable
	}
	return model.JSONObject(snapshot.Values), nil
}

func (r *Resolver) appConfigMetadata(ctx context.Context) (model.JSONObject, error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if r.AppConfig == nil {
		return nil, ErrAppConfigUnavailable
	}
	snapshot, err := r.AppConfig.Read(ctx)
	if err != nil {
		return nil, ErrAppConfigUnavailable
	}
	return model.JSONObject(snapshot.Metadata), nil
}

func (r *Resolver) validateAppConfig(ctx context.Context, inputs []*model.UpdateAppConfigInput) ([]*model.AppConfigValidateResult, error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if r.AppConfig == nil {
		return nil, ErrAppConfigUnavailable
	}
	updates := appConfigUpdates(inputs)
	validated := r.AppConfig.Validate(updates)
	results := make([]*model.AppConfigValidateResult, 0, len(validated))
	for _, item := range validated {
		result := &model.AppConfigValidateResult{Module: item.Module, Key: item.Key, Valid: item.Valid, Error: item.Error}
		if item.Value != nil {
			result.Value = &model.JSON{Value: item.Value}
		}
		results = append(results, result)
	}
	return results, nil
}

func (r *Resolver) updateAppConfig(ctx context.Context, inputs []*model.UpdateAppConfigInput) (model.JSONObject, error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAdminAccessDenied
	}
	if r.AppConfig == nil {
		return nil, ErrAppConfigUnavailable
	}
	updated, err := r.AppConfig.Update(ctx, principal.User.ID, appConfigUpdates(inputs))
	if err != nil {
		return nil, err
	}
	return model.JSONObject(updated), nil
}

func appConfigUpdates(inputs []*model.UpdateAppConfigInput) []appconfig.Update {
	updates := make([]appconfig.Update, 0, len(inputs))
	for _, input := range inputs {
		if input == nil {
			continue
		}
		update := appconfig.Update{Module: input.Module, Key: input.Key}
		if value, hasValue := input.Value.ValueOK(); hasValue {
			update.HasValue = true
			if value != nil {
				update.Value = value.Value
			}
		}
		if input.Clear != nil {
			update.Clear = *input.Clear
		}
		updates = append(updates, update)
	}
	return updates
}
