// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
)

func TestCopilotBYOKRouteChoicesFilterUnavailableModels(t *testing.T) {
	textChat := []byok.Capability{{Input: []string{"text"}, Output: []string{"text"}}}
	imageOnly := []byok.Capability{{Input: []string{"image"}, Output: []string{"image"}}}
	profiles := []byok.Profile{
		{
			ProfileID: "profile-enabled", Name: "Studio",
			Enabled: true,
			Definition: byok.ProfileDefinition{Models: []byok.ModelDeclaration{
				{ModelID: "chat-model", Enabled: true, Capabilities: textChat},
				{ModelID: "disabled-model", Enabled: false, Capabilities: textChat},
				{ModelID: "image-model", Enabled: true, Capabilities: imageOnly},
			}},
		},
		{
			ProfileID: "profile-disabled", Name: "Archived", Enabled: false,
			Definition: byok.ProfileDefinition{Models: []byok.ModelDeclaration{
				{ModelID: "hidden-model", Enabled: true, Capabilities: textChat},
			}},
		},
	}

	choices := copilotBYOKRouteChoices(profiles)
	if len(choices) != 1 {
		t.Fatalf("route choices = %#v; want only the enabled text-chat model", choices)
	}
	if choices[0].ID != "fydeos-byok:profile-enabled:chat-model" || !choices[0].Available {
		t.Fatalf("route choice = %#v", choices[0])
	}
}

func TestCopilotRouteOptionsIncludesConfiguredFydeOSManagedTarget(t *testing.T) {
	resolver := &copilotResolver{Resolver: &Resolver{
		Config:         config.Config{FydeOSManagedCopilot: true},
		MCPCredentials: enabledCopilotCredentials{},
	}}
	options, err := resolver.routeOptions(context.Background(), &model.Copilot{}, fydeosBYOKPromptName)
	if err != nil {
		t.Fatalf("resolve managed route options: %v", err)
	}
	if options == nil || options.DefaultTargetID == nil || *options.DefaultTargetID != fydeosManagedCopilotTargetID || len(options.Choices) != 1 {
		t.Fatalf("managed route options = %#v", options)
	}
	if options.Choices[0].ID != fydeosManagedCopilotTargetID || options.Choices[0].DisplayName != "FydeOS AI" || !options.Choices[0].Available {
		t.Fatalf("managed route choice = %#v", options.Choices[0])
	}
}
