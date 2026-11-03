// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/copilot"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
)

const fydeosBYOKPromptName = "Chat With AFFiNE AI"
const fydeosManagedCopilotTargetID = "fydeos-managed-openai"

func (r *copilotResolver) quota(ctx context.Context) (*model.CopilotQuota, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return nil, err
	}
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if r.CopilotSessions == nil {
		return nil, ErrCopilotSessionsUnavailable
	}
	if r.Config.FydeOSManagedCopilot {
		used, err := r.CopilotSessions.CountFydeosManagedRequests(ctx, principal.User.ID, time.Now())
		if err != nil {
			return nil, err
		}
		limit := copilot.FydeosManagedQuotaLimit
		return &model.CopilotQuota{Limit: &limit, Used: used}, nil
	}
	used, err := r.CopilotSessions.UserMessageCost(ctx, principal.User.ID)
	if err != nil {
		return nil, err
	}
	return &model.CopilotQuota{Used: used}, nil
}

func (r *copilotResolver) routeOptions(ctx context.Context, parent *model.Copilot, promptName string) (*model.CopilotRouteOptions, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return nil, err
	}
	if promptName != fydeosBYOKPromptName {
		return nil, nil
	}
	result := &model.CopilotRouteOptions{RouteID: promptName, Choices: []*model.CopilotRouteTarget{}}
	if r.Config.FydeOSManagedCopilot {
		defaultTargetID := fydeosManagedCopilotTargetID
		result.DefaultTargetID = &defaultTargetID
		result.Choices = append(result.Choices, &model.CopilotRouteTarget{
			ID: fydeosManagedCopilotTargetID, DisplayName: "FydeOS AI", MinimumTier: "Standard", Available: true,
		})
	}
	if parent.WorkspaceID == nil || *parent.WorkspaceID == "" {
		return result, nil
	}
	principal, _, err := r.authorizeCopilotScope(ctx, *parent.WorkspaceID, nil, "Workspace.Copilot")
	if err != nil {
		return nil, err
	}
	if r.BYOKProfiles == nil {
		return nil, ErrBYOKProfilesUnavailable
	}
	entitlement, err := r.BYOKProfiles.Entitlement(ctx, *parent.WorkspaceID, principal.User.ID)
	if err != nil {
		return nil, err
	}
	if !entitlement.Server {
		return result, nil
	}
	policy, err := r.BYOKProfiles.Policy(ctx)
	if err != nil {
		return nil, err
	}
	if !policy.Enabled {
		return result, nil
	}
	profiles, err := r.BYOKProfiles.List(ctx, *parent.WorkspaceID)
	if err != nil {
		return nil, err
	}
	result.Choices = copilotBYOKRouteChoices(profiles)
	return result, nil
}

func copilotBYOKRouteChoices(profiles []byok.Profile) []*model.CopilotRouteTarget {
	choices := make([]*model.CopilotRouteTarget, 0)
	for _, profile := range profiles {
		if !profile.Enabled {
			continue
		}
		for _, modelProfile := range profile.Definition.Models {
			if !modelProfile.Enabled || !supportsTextChat(modelProfile.Capabilities) {
				continue
			}
			choices = append(choices, &model.CopilotRouteTarget{
				ID:          "fydeos-byok:" + profile.ProfileID + ":" + modelProfile.ModelID,
				DisplayName: "Workspace BYOK " + profile.Name + " / " + modelProfile.ModelID,
				MinimumTier: "Standard",
				Available:   true,
			})
		}
	}
	return choices
}

func supportsTextChat(capabilities []byok.Capability) bool {
	for _, capability := range capabilities {
		inputText, outputText := false, false
		for _, input := range capability.Input {
			inputText = inputText || input == "text"
		}
		for _, output := range capability.Output {
			outputText = outputText || output == "text"
		}
		if inputText && outputText {
			return true
		}
	}
	return false
}
