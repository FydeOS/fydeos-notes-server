// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
)

func TestServerActionRoutingUsesCapabilityOrderAndEntitlement(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	fixture := seedGraphQLFixture(t, ctx, pool)
	applyBYOKMigration(t, pool)
	store, _ := newBYOKGraphQLHandler(t, pool)
	catalog, err := byok.ByokCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var audioCapabilities []byok.Capability
	for _, provider := range catalog.Providers {
		if provider.Provider == "gemini" {
			for _, model := range provider.Models {
				if model.ModelID == "gemini-3.8-flash" {
					audioCapabilities = model.Capabilities
				}
			}
		}
	}
	if len(audioCapabilities) == 0 {
		t.Fatal("missing Gemini catalog capabilities")
	}
	create := func(name string, capabilities []byok.Capability) byok.Profile {
		p, err := store.Create(ctx, byok.CreateProfileInput{WorkspaceID: fixture.workspaceID, ActorUserID: fixture.userID, Provider: "gemini", Name: name, Credential: "credential-" + name, Enabled: true, Definition: byok.ProfileDefinitionInput{Endpoint: byok.EndpointInput{Kind: "provider_default"}, Models: []byok.ModelDeclaration{{ModelID: "gemini-3.8-flash", Enabled: true, Capabilities: capabilities}}}})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// A probe-compatible data-only model is not a valid runtime transcript target.
	narrow := create("data-only", []byok.Capability{{Input: []string{"audio"}, Output: []string{"structured"}, AttachmentKinds: []string{"audio"}, AttachmentSource: []string{"data"}}})
	first := create("first", audioCapabilities)
	second := create("second", audioCapabilities)
	if _, err := store.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "transcript"); !errors.Is(err, byok.ErrTargetUnavailable) {
		t.Fatalf("unentitled route error=%v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO entitlements(target_type,target_id,source,plan,status) VALUES('user',$1,'admin_grant','pro','active')`, fixture.userID); err != nil {
		t.Fatal(err)
	}
	assertRoute := func(operation, credential string) {
		target, err := store.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, operation)
		if err != nil {
			t.Fatal(err)
		}
		defer target.Credential.Destroy()
		if target.Provider != "gemini" || string(target.Credential.Bytes()) != credential {
			t.Fatal("wrong selected profile")
		}
	}
	assertRoute("transcript", "credential-first")
	assertRoute("structured", "credential-first")
	if _, err := pool.Exec(ctx, `UPDATE ai_workspace_byok_configs SET sort_order=-1 WHERE id=$1`, second.ProfileID); err != nil {
		t.Fatal(err)
	}
	assertRoute("transcript", "credential-second")
	if _, err := pool.Exec(ctx, `UPDATE ai_workspace_byok_configs SET enabled=false WHERE id=$1`, second.ProfileID); err != nil {
		t.Fatal(err)
	}
	assertRoute("transcript", "credential-first")
	if _, err := store.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "unknown"); !errors.Is(err, byok.ErrTargetUnavailable) {
		t.Fatal("unknown operation accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE ai_workspace_byok_configs SET enabled=false WHERE id=$1`, first.ProfileID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveServerTarget(ctx, fixture.workspaceID, fixture.userID, "transcript"); !errors.Is(err, byok.ErrTargetUnavailable) {
		t.Fatalf("narrow profile %s accepted, err=%v", narrow.ProfileID, err)
	}
	if _, err := store.ResolveServerTarget(ctx, "other-workspace", fixture.userID, "transcript"); !errors.Is(err, byok.ErrTargetUnavailable) {
		t.Fatalf("cross-workspace route=%v", err)
	}
}
