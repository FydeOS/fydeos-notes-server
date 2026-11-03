// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"sync"

	gqlgraphql "github.com/99designs/gqlgen/graphql"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
)

type oauthSnapshotContextKey struct{}

type oauthProviderSnapshot struct {
	once      sync.Once
	providers []model.OAuthProviderType
	err       error
}

func serverConfigOAuthSnapshotMiddleware() gqlgraphql.OperationMiddleware {
	return func(ctx context.Context, next gqlgraphql.OperationHandler) gqlgraphql.ResponseHandler {
		return next(context.WithValue(ctx, oauthSnapshotContextKey{}, &oauthProviderSnapshot{}))
	}
}

func (r *Resolver) serverFeatures(ctx context.Context) ([]model.ServerFeature, error) {
	features := []model.ServerFeature{}
	commentEnabled := r.CommentReader != nil && r.CommentWriter != nil && r.CommentAuth != nil
	if commentEnabled {
		features = append(features, model.ServerFeatureComment)
	}
	local, err := r.ServerConfig.LocalWorkspaceEnabled(ctx)
	if err != nil {
		return nil, err
	}
	if local {
		features = append(features, model.ServerFeatureLocalWorkspace)
	}
	providers, err := r.serverOAuthProviders(ctx)
	if err != nil {
		return nil, err
	}
	if !local && len(providers) > 0 {
		features = append(features, model.ServerFeatureOAuth)
	}
	if r.MCPCredentials != nil {
		copilotEnabled, err := r.MCPCredentials.CopilotEnabled(ctx)
		if err != nil {
			return nil, err
		}
		if copilotEnabled {
			features = append(features, model.ServerFeatureCopilot)
		}
	}
	if r.CopilotWorkspaceArtifacts != nil && r.CopilotWorkspaceArtifacts.EmbeddingAvailable(ctx) {
		features = append(features, model.ServerFeatureCopilotEmbedding)
	}
	if r.SearchProjection != nil {
		indexerEnabled, err := r.ServerConfig.IndexerEnabled(ctx)
		if err != nil {
			return nil, err
		}
		if indexerEnabled {
			features = append(features, model.ServerFeatureIndexer)
		}
	}
	if local && len(providers) > 0 {
		features = append(features, model.ServerFeatureOAuth)
	}
	return features, nil
}

func (r *Resolver) serverOAuthProviders(ctx context.Context) ([]model.OAuthProviderType, error) {
	snapshot, ok := ctx.Value(oauthSnapshotContextKey{}).(*oauthProviderSnapshot)
	if !ok {
		return r.loadOAuthProviders(ctx)
	}
	snapshot.once.Do(func() { snapshot.providers, snapshot.err = r.loadOAuthProviders(ctx) })
	return snapshot.providers, snapshot.err
}

func (r *Resolver) loadOAuthProviders(ctx context.Context) ([]model.OAuthProviderType, error) {
	providers := []model.OAuthProviderType{}
	if r.OAuthProviders == nil {
		return providers, nil
	}
	available, err := r.OAuthProviders.AvailableProviders(ctx)
	if err != nil {
		return nil, errors.New("OAuth provider catalog is unavailable")
	}
	for _, name := range available {
		var provider model.OAuthProviderType
		switch name {
		case "google":
			provider = model.OAuthProviderTypeGoogle
		case "github":
			provider = model.OAuthProviderTypeGitHub
		case "apple":
			provider = model.OAuthProviderTypeApple
		case "fydeos":
			provider = model.OAuthProviderTypeFydeOs
		case "oidc":
			provider = model.OAuthProviderTypeOidc
		default:
			return nil, errors.New("unknown OAuth provider in catalog")
		}
		providers = append(providers, provider)
	}
	return providers, nil
}
