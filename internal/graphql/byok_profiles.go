// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/fydeos/fydeos-notes-server/internal/workspaces"
)

var (
	ErrBYOKProfilesUnavailable = errors.New("BYOK profile service is unavailable")
	ErrBYOKDescriptionRequired = errors.New("description must be provided explicitly.")
)

func (r *Resolver) byokSettings(ctx context.Context, workspaceID string) (*model.WorkspaceByokSettingsType, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return nil, err
	}
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if err := r.authorizeBYOKSettings(ctx, principal.User.ID, workspaceID); err != nil {
		return nil, err
	}
	if r.BYOKProfiles == nil {
		return nil, ErrBYOKProfilesUnavailable
	}
	entitlement, err := r.BYOKProfiles.Entitlement(ctx, workspaceID, principal.User.ID)
	if err != nil {
		return nil, err
	}
	policy, err := r.BYOKProfiles.Policy(ctx)
	if err != nil {
		return nil, err
	}
	catalog, err := byok.ByokCatalog()
	if err != nil {
		return nil, err
	}
	settings := &model.WorkspaceByokSettingsType{
		WorkspaceID:    &workspaceID,
		Entitled:       entitlement.Server || entitlement.Local,
		ServerEntitled: entitlement.Server,
		LocalEntitled:  entitlement.Local,
		Policy:         byokPolicyModel(policy),
		Catalog:        byokCatalogModel(catalog, policy.AllowedProviders),
		Profiles:       []*model.WorkspaceByokProfileType{},
	}
	if entitlement.Server {
		profiles, err := r.BYOKProfiles.List(ctx, workspaceID)
		if err != nil {
			return nil, err
		}
		for _, profile := range profiles {
			settings.Profiles = append(settings.Profiles, byokProfileModel(profile))
		}
	}
	return settings, nil
}

func (r *Resolver) workspaceByokUsage(ctx context.Context, workspaceID string, from, to time.Time) ([]*model.WorkspaceByokUsagePointType, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return nil, err
	}
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if err := r.authorizeBYOKSettings(ctx, principal.User.ID, workspaceID); err != nil {
		return nil, err
	}
	if r.BYOKProfiles == nil {
		return nil, ErrBYOKProfilesUnavailable
	}
	points, err := r.BYOKProfiles.AggregateUsageByDay(ctx, workspaceID, from, to)
	if err != nil {
		return nil, err
	}
	result := make([]*model.WorkspaceByokUsagePointType, 0, len(points))
	for _, point := range points {
		result = append(result, &model.WorkspaceByokUsagePointType{
			Date: point.Date.UTC().Format(time.RFC3339Nano), FeatureKind: point.FeatureKind, TotalTokens: point.TotalTokens,
		})
	}
	return result, nil
}

func (r *Resolver) createWorkspaceByokProfile(ctx context.Context, input model.CreateWorkspaceByokProfileInput) (*model.WorkspaceByokProfileType, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return nil, err
	}
	principal, err := r.authorizeBYOKMutation(ctx, input.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if !input.Description.IsSet() {
		return nil, ErrBYOKDescriptionRequired
	}
	definition, err := byokDefinitionInput(input.Definition)
	if err != nil {
		return nil, err
	}
	description := input.Description.Value()
	profile, err := r.BYOKProfiles.Create(ctx, byok.CreateProfileInput{
		WorkspaceID: input.WorkspaceID, Provider: string(input.Provider), Name: input.Name,
		Description: description, Credential: input.Credential, Definition: definition,
		Enabled: input.Enabled, ActorUserID: principal.User.ID,
	})
	if err != nil {
		return nil, err
	}
	return byokProfileModel(profile), nil
}

func (r *Resolver) replaceWorkspaceByokProfile(ctx context.Context, input model.ReplaceWorkspaceByokProfileInput) (*model.WorkspaceByokProfileType, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return nil, err
	}
	principal, err := r.authorizeBYOKMutation(ctx, input.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if !input.Description.IsSet() {
		return nil, ErrBYOKDescriptionRequired
	}
	definition, err := byokDefinitionInput(input.Definition)
	if err != nil {
		return nil, err
	}
	profile, err := r.BYOKProfiles.Replace(ctx, byok.ReplaceProfileInput{
		WorkspaceID: input.WorkspaceID, ProfileID: input.ProfileID,
		ExpectedRevision: input.ExpectedRevision, Name: input.Name,
		Description: input.Description.Value(), Definition: definition,
		Credential: input.Credential, Enabled: input.Enabled, ActorUserID: principal.User.ID,
	})
	if err != nil {
		return nil, err
	}
	return byokProfileModel(profile), nil
}

func (r *Resolver) rotateWorkspaceByokCredential(ctx context.Context, input model.RotateWorkspaceByokCredentialInput) (*model.WorkspaceByokProfileType, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return nil, err
	}
	principal, err := r.authorizeBYOKMutation(ctx, input.WorkspaceID)
	if err != nil {
		return nil, err
	}
	profile, err := r.BYOKProfiles.Rotate(ctx, byok.RotateCredentialInput{
		WorkspaceID: input.WorkspaceID, ProfileID: input.ProfileID,
		ExpectedRevision: input.ExpectedRevision, Credential: input.Credential,
		ActorUserID: principal.User.ID,
	})
	if err != nil {
		return nil, err
	}
	return byokProfileModel(profile), nil
}

func (r *Resolver) probeWorkspaceByokProfile(ctx context.Context, input model.ProbeWorkspaceByokProfileInput) (*model.WorkspaceByokProbeResultType, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return nil, err
	}
	if _, err := r.authorizeBYOKMutation(ctx, input.WorkspaceID); err != nil {
		return nil, err
	}
	checks, err := byokProbeChecksInput(input.Checks)
	if err != nil {
		return nil, err
	}
	result, err := r.BYOKProfiles.ProbeProfile(ctx, byok.ProbeProfileInput{
		WorkspaceID: input.WorkspaceID, ProfileID: input.ProfileID, Checks: checks,
	})
	if err != nil {
		return nil, err
	}
	return byokProbeResultModel(result), nil
}

func (r *Resolver) probeWorkspaceByokDraft(ctx context.Context, input model.ProbeWorkspaceByokDraftInput) (*model.WorkspaceByokProbeResultType, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return nil, err
	}
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if err := r.authorizeBYOKSettings(ctx, principal.User.ID, input.WorkspaceID); err != nil {
		return nil, err
	}
	if r.BYOKProfiles == nil {
		return nil, ErrBYOKProfilesUnavailable
	}
	if input.ProfileID != nil && *input.ProfileID != "" {
		if _, err := r.authorizeBYOKMutation(ctx, input.WorkspaceID); err != nil {
			return nil, err
		}
	} else {
		entitlement, err := r.BYOKProfiles.Entitlement(ctx, input.WorkspaceID, principal.User.ID)
		if err != nil {
			return nil, err
		}
		if !entitlement.Server && !entitlement.Local {
			return nil, byok.ErrEntitlementRequired
		}
	}
	checks, err := byokProbeChecksInput(input.Checks)
	if err != nil {
		return nil, err
	}
	definition, err := byokDefinitionInput(input.Definition)
	if err != nil {
		return nil, err
	}
	result, err := r.BYOKProfiles.ProbeDraft(ctx, byok.ProbeDraftInput{
		WorkspaceID: input.WorkspaceID, Provider: string(input.Provider), Credential: input.Credential,
		ProfileID: input.ProfileID, ExpectedRevision: input.ExpectedRevision,
		Definition: definition, Checks: checks,
	})
	if err != nil {
		return nil, err
	}
	return byokProbeResultModel(result), nil
}

func (r *Resolver) deleteWorkspaceByokProfile(ctx context.Context, workspaceID, profileID string) (bool, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return false, err
	}
	if _, err := r.authorizeBYOKMutation(ctx, workspaceID); err != nil {
		return false, err
	}
	return r.BYOKProfiles.Delete(ctx, workspaceID, profileID)
}

func (r *Resolver) reorderWorkspaceByokProfiles(ctx context.Context, input model.ReorderWorkspaceByokProfilesInput) ([]*model.WorkspaceByokProfileType, error) {
	if err := r.requireCopilotEnabled(ctx); err != nil {
		return nil, err
	}
	principal, err := r.authorizeBYOKMutation(ctx, input.WorkspaceID)
	if err != nil {
		return nil, err
	}
	profiles := make([]byok.ProfileOrderInput, 0, len(input.Profiles))
	for _, profile := range input.Profiles {
		if profile == nil {
			return nil, byok.ErrInvalidInput
		}
		profiles = append(profiles, byok.ProfileOrderInput{ProfileID: profile.ProfileID, ExpectedRevision: profile.ExpectedRevision})
	}
	ordered, err := r.BYOKProfiles.Reorder(ctx, input.WorkspaceID, principal.User.ID, profiles)
	if err != nil {
		return nil, err
	}
	result := make([]*model.WorkspaceByokProfileType, 0, len(ordered))
	for _, profile := range ordered {
		result = append(result, byokProfileModel(profile))
	}
	return result, nil
}

func (r *Resolver) authorizeBYOKMutation(ctx context.Context, workspaceID string) (*auth.Principal, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return nil, ErrAuthenticationRequired
	}
	if err := r.authorizeBYOKSettings(ctx, principal.User.ID, workspaceID); err != nil {
		return nil, err
	}
	if r.BYOKProfiles == nil {
		return nil, ErrBYOKProfilesUnavailable
	}
	entitlement, err := r.BYOKProfiles.Entitlement(ctx, workspaceID, principal.User.ID)
	if err != nil {
		return nil, err
	}
	if !entitlement.Server {
		return nil, byok.ErrEntitlementRequired
	}
	return principal, nil
}

func (r *Resolver) authorizeBYOKSettings(ctx context.Context, userID, workspaceID string) error {
	if r.WorkspacePermissionReader == nil {
		return ErrWorkspaceRolePermissionsUnavailable
	}
	access, err := r.WorkspacePermissionReader.GetWorkspaceRolePermissions(ctx, userID, workspaceID)
	if errors.Is(err, workspaces.ErrAccessDenied) || errors.Is(err, workspaces.ErrNotFound) {
		return &WorkspacePermissionsDeniedError{WorkspaceID: workspaceID}
	}
	if err != nil {
		return err
	}
	if !access.Permissions["Workspace_Settings_Update"] {
		return &WorkspacePermissionsDeniedError{WorkspaceID: workspaceID}
	}
	return nil
}

func byokDefinitionInput(input *model.WorkspaceByokProfileDefinitionInput) (byok.ProfileDefinitionInput, error) {
	if input == nil || input.Endpoint == nil {
		return byok.ProfileDefinitionInput{}, byok.ErrInvalidInput
	}
	endpoint := byok.EndpointInput{Kind: string(input.Endpoint.Kind), URL: input.Endpoint.URL}
	if input.Endpoint.Dialect != nil {
		value := string(*input.Endpoint.Dialect)
		endpoint.Dialect = &value
	}
	definition := byok.ProfileDefinitionInput{Endpoint: endpoint, Models: make([]byok.ModelDeclaration, 0, len(input.Models))}
	for _, modelInput := range input.Models {
		if modelInput == nil {
			return byok.ProfileDefinitionInput{}, byok.ErrInvalidInput
		}
		declaration := byok.ModelDeclaration{ModelID: modelInput.ModelID, Enabled: modelInput.Enabled, Capabilities: make([]byok.Capability, 0, len(modelInput.Capabilities))}
		for _, capability := range modelInput.Capabilities {
			if capability == nil {
				return byok.ProfileDefinitionInput{}, byok.ErrInvalidInput
			}
			declaration.Capabilities = append(declaration.Capabilities, byok.Capability{
				Input: enumStrings(capability.Input), Output: enumStrings(capability.Output), Features: enumStrings(capability.Features),
				AttachmentKinds: enumStrings(capability.AttachmentKinds), AttachmentSource: enumStrings(capability.AttachmentSources),
			})
		}
		definition.Models = append(definition.Models, declaration)
	}
	return definition, nil
}

func enumStrings[T ~string](values []T) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = string(value)
	}
	return result
}

func byokProfileModel(profile byok.Profile) *model.WorkspaceByokProfileType {
	return &model.WorkspaceByokProfileType{
		ProfileID: profile.ProfileID, WorkspaceID: profile.WorkspaceID, Provider: model.ByokProvider(profile.Provider),
		Name: profile.Name, Description: profile.Description, Definition: byokDefinitionModel(profile.Definition),
		Enabled: profile.Enabled, SortOrder: profile.SortOrder, Revision: profile.Revision,
		Validation: byokValidationModel(profile.Validation),
	}
}

func byokDefinitionModel(definition byok.ProfileDefinition) *model.WorkspaceByokProfileDefinitionType {
	endpoint := &model.WorkspaceByokEndpointType{Kind: model.ByokEndpointKind(definition.Endpoint.Kind)}
	if definition.Endpoint.URL != "" {
		value := definition.Endpoint.URL
		endpoint.URL = &value
	}
	if definition.Endpoint.Dialect != "" {
		value := model.ByokOpenAiDialect(definition.Endpoint.Dialect)
		endpoint.Dialect = &value
	}
	result := &model.WorkspaceByokProfileDefinitionType{Endpoint: endpoint, Models: make([]*model.WorkspaceByokModelDeclarationType, 0, len(definition.Models))}
	for _, declaration := range definition.Models {
		item := &model.WorkspaceByokModelDeclarationType{
			ModelID: declaration.ModelID, Enabled: declaration.Enabled,
			Capabilities: make([]*model.WorkspaceByokCapabilityType, 0, len(declaration.Capabilities)),
		}
		for _, capability := range declaration.Capabilities {
			item.Capabilities = append(item.Capabilities, byokCapabilityModel(capability))
		}
		result.Models = append(result.Models, item)
	}
	return result
}

func byokCapabilityModel(capability byok.Capability) *model.WorkspaceByokCapabilityType {
	return &model.WorkspaceByokCapabilityType{
		Input: castEnum[model.ByokModelInput](capability.Input), Output: castEnum[model.ByokModelOutput](capability.Output),
		Features: castEnum[model.ByokModelFeature](capability.Features), AttachmentKinds: castEnum[model.ByokAttachmentKind](capability.AttachmentKinds),
		AttachmentSources: castEnum[model.ByokAttachmentSource](capability.AttachmentSource),
	}
}

func castEnum[T ~string](values []string) []T {
	result := make([]T, len(values))
	for index, value := range values {
		result[index] = T(value)
	}
	return result
}

func byokPolicyModel(policy byok.Policy) *model.WorkspaceByokPolicyType {
	return &model.WorkspaceByokPolicyType{
		Enabled: policy.Enabled, AllowedProviders: castEnum[model.ByokProvider](policy.AllowedProviders),
		CustomEndpointMode:       model.ByokCustomEndpointMode(policy.CustomEndpointMode),
		PrivateEndpointSupported: policy.PrivateEndpointSupported,
	}
}

func byokCatalogModel(catalog byok.Catalog, allowed []string) *model.WorkspaceByokCatalogType {
	result := &model.WorkspaceByokCatalogType{Version: catalog.Version, Providers: []*model.WorkspaceByokCatalogProviderType{}}
	for _, provider := range catalog.Providers {
		if !containsString(allowed, provider.Provider) {
			continue
		}
		providerModel := &model.WorkspaceByokCatalogProviderType{Provider: model.ByokProvider(provider.Provider), Models: []*model.WorkspaceByokCatalogModelType{}}
		for _, catalogModel := range provider.Models {
			modelItem := &model.WorkspaceByokCatalogModelType{
				ModelID: catalogModel.ModelID, DisplayName: catalogModel.DisplayName, Recommended: catalogModel.Recommended,
				Capabilities: []*model.WorkspaceByokCapabilityType{},
			}
			for _, capability := range catalogModel.Capabilities {
				modelItem.Capabilities = append(modelItem.Capabilities, byokCapabilityModel(capability))
			}
			providerModel.Models = append(providerModel.Models, modelItem)
		}
		result.Providers = append(result.Providers, providerModel)
	}
	return result
}

func byokValidationModel(validation *byok.Validation) *model.WorkspaceByokValidationType {
	if validation == nil {
		return nil
	}
	result := &model.WorkspaceByokValidationType{
		DefinitionFingerprint: validation.DefinitionFingerprint,
		CredentialGeneration:  validation.CredentialGeneration,
		Connection:            byokProbeStatusModel(validation.Connection),
		Models:                make([]*model.WorkspaceByokModelProbeType, 0, len(validation.Models)),
	}
	for _, probe := range validation.Models {
		probeModel := &model.WorkspaceByokModelProbeType{ModelID: probe.ModelID, Checks: make([]*model.WorkspaceByokModelProbeCheckType, 0, len(probe.Checks))}
		for _, check := range probe.Checks {
			probeModel.Checks = append(probeModel.Checks, &model.WorkspaceByokModelProbeCheckType{
				Operation: model.ByokProbeOperation(check.Operation), Status: byokProbeStatusModel(check.Status),
			})
		}
		result.Models = append(result.Models, probeModel)
	}
	return result
}

func byokProbeChecksInput(checks []*model.WorkspaceByokProbeCheckInput) ([]byok.ProbeCheckInput, error) {
	result := make([]byok.ProbeCheckInput, 0, len(checks))
	for _, check := range checks {
		if check == nil {
			return nil, byok.ErrInvalidInput
		}
		result = append(result, byok.ProbeCheckInput{ModelID: check.ModelID, Operation: string(check.Operation)})
	}
	return result, nil
}

func byokProbeResultModel(probe byok.ProbeResult) *model.WorkspaceByokProbeResultType {
	result := &model.WorkspaceByokProbeResultType{
		DefinitionFingerprint: probe.DefinitionFingerprint,
		Stale:                 probe.Stale,
		Connection:            byokProbeStatusModel(probe.Connection),
		Models:                make([]*model.WorkspaceByokModelProbeType, 0, len(probe.Models)),
	}
	for _, item := range probe.Models {
		modelProbe := &model.WorkspaceByokModelProbeType{ModelID: item.ModelID, Checks: make([]*model.WorkspaceByokModelProbeCheckType, 0, len(item.Checks))}
		for _, check := range item.Checks {
			modelProbe.Checks = append(modelProbe.Checks, &model.WorkspaceByokModelProbeCheckType{
				Operation: model.ByokProbeOperation(check.Operation), Status: byokProbeStatusModel(check.Status),
			})
		}
		result.Models = append(result.Models, modelProbe)
	}
	return result
}

func byokProbeStatusModel(status byok.ProbeStatus) *model.WorkspaceByokProbeStatusType {
	result := &model.WorkspaceByokProbeStatusType{Kind: model.ByokProbeStatusKind(status.Kind), ErrorKind: status.ErrorKind}
	if status.TestedAtMS != nil {
		formatted := time.UnixMilli(*status.TestedAtMS).UTC().Format(time.RFC3339Nano)
		result.TestedAt = &formatted
	}
	return result
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func byokErrorHTTPStatus(err error) int {
	if errors.Is(err, byok.ErrEntitlementRequired) {
		return http.StatusForbidden
	}
	return http.StatusBadRequest
}
