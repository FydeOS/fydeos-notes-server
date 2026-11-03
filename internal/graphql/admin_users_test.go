// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
)

func TestUserFeatureNameMapsBetweenGraphQLAndStorage(t *testing.T) {
	if got := storedUserFeatureName(model.FeatureTypeAdmin); got != administratorFeatureName {
		t.Fatalf("stored feature name = %q, want %q", got, administratorFeatureName)
	}
	if got, ok := graphQLUserFeature(administratorFeatureName); !ok || got != model.FeatureTypeAdmin {
		t.Fatalf("GraphQL feature = %q, recognized=%t; want Admin", got, ok)
	}
	if got, ok := graphQLUserFeature("unknown"); ok || got != "" {
		t.Fatalf("unknown feature = %q, recognized=%t; want unrecognized", got, ok)
	}
}
