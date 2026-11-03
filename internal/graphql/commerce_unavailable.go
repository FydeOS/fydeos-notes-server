// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"

	"github.com/99designs/gqlgen/graphql"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
)

// ErrFeatureNotAvailable is returned by the subscription, checkout and
// license operations of the GraphQL protocol. This edition has no commerce
// backend; the fields remain in the schema for client compatibility.
var ErrFeatureNotAvailable = errors.New("feature not available")

func (r *Resolver) prices(context.Context) ([]*model.SubscriptionPrice, error) {
	return []*model.SubscriptionPrice{}, nil
}

func (r *mutationResolver) createCustomerPortal(context.Context) (string, error) {
	return "", ErrFeatureNotAvailable
}

func (r *mutationResolver) createCheckoutSession(context.Context, model.CreateCheckoutSessionInput) (string, error) {
	return "", ErrFeatureNotAvailable
}

func (r *mutationResolver) cancelSubscription(context.Context, *string, *model.SubscriptionPlan, *string) (*model.SubscriptionType, error) {
	return nil, ErrFeatureNotAvailable
}

func (r *mutationResolver) resumeSubscription(context.Context, *string, *model.SubscriptionPlan, *string) (*model.SubscriptionType, error) {
	return nil, ErrFeatureNotAvailable
}

func (r *mutationResolver) updateSubscriptionRecurring(context.Context, *string, *model.SubscriptionPlan, model.SubscriptionRecurring, *string) (*model.SubscriptionType, error) {
	return nil, ErrFeatureNotAvailable
}

func (r *Resolver) activateLicense(context.Context, string, string) (*model.License, error) {
	return nil, ErrFeatureNotAvailable
}

func (r *Resolver) deactivateLicense(context.Context, string) (bool, error) {
	return false, ErrFeatureNotAvailable
}

func (r *Resolver) createSelfhostWorkspaceCustomerPortal(context.Context, string) (string, error) {
	return "", ErrFeatureNotAvailable
}

func (r *Resolver) installLicenseFile(context.Context, graphql.Upload, string) (*model.License, error) {
	return nil, ErrFeatureNotAvailable
}

func (r *Resolver) previewLicenseFile(context.Context, graphql.Upload) (*model.AdminLicensePreview, error) {
	return nil, ErrFeatureNotAvailable
}

// workspaceLicense resolves the nullable WorkspaceType.license field. No
// workspace ever has an installed license in this edition.
func (r *Resolver) workspaceLicense(context.Context, string) (*model.License, error) {
	return nil, nil
}
