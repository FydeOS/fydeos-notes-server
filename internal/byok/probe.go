// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
)

const (
	maxProbeChecks = 32
	probeDeadline  = 30 * time.Second

	probeVerified  = "verified"
	probeFailed    = "failed"
	probeNotTested = "not_tested"
)

// ProbeTarget is one connectivity check handed to a ProbeRunner.
type ProbeTarget struct {
	Provider             string
	Endpoint             Endpoint
	ModelID              string
	Credential           []byte
	Operation            string
	AllowPrivateEndpoint bool
	EndpointResolver     EndpointResolver
}

// ProbeRunner performs one provider check.
type ProbeRunner interface {
	Probe(context.Context, ProbeTarget) ProbeStatus
}

var connectionErrorKinds = []string{"authentication", "permission", "not_found", "rate_limited",
	"unavailable", "rejected", "transport", "timeout", "invalid_response"}

func failedStatus(kind string) ProbeStatus {
	return ProbeStatus{Kind: probeFailed, ErrorKind: &kind}
}

func nowMS() *int64 {
	ms := time.Now().UnixMilli()
	return &ms
}

// validateChecks normalizes probe checks against a definition.
func validateChecks(definition ProfileDefinition, checks []ProbeCheckInput) ([]ProbeCheckInput, error) {
	if len(checks) > maxProbeChecks {
		return nil, invalidf("too many BYOK probe checks")
	}
	out := make([]ProbeCheckInput, 0, len(checks))
	for _, check := range checks {
		check.ModelID = strings.TrimSpace(check.ModelID)
		if _, ok := probeRequirements[check.Operation]; !ok {
			return nil, invalidf("unsupported BYOK probe operation")
		}
		if slices.Contains(out, check) {
			return nil, invalidf("duplicate BYOK probe check")
		}
		if _, ok := findModel(definition, check.ModelID); !ok {
			return nil, invalidf("BYOK probe model not found")
		}
		out = append(out, check)
	}
	return out, nil
}

type probeRun struct {
	provider     string
	definition   ProfileDefinition
	credential   []byte
	allowPrivate bool
	resolver     EndpointResolver
	runner       ProbeRunner
}

// run executes the checks and groups results per model in definition order.
func (r probeRun) run(ctx context.Context, checks []ProbeCheckInput) (ProbeStatus, []ModelProbe) {
	ctx, cancel := context.WithTimeout(ctx, probeDeadline)
	defer cancel()
	results := make([]ProbeStatus, len(checks))
	for i, check := range checks {
		model, _ := findModel(r.definition, check.ModelID)
		switch {
		case !model.Enabled:
			results[i] = failedStatus("model_disabled")
		case !satisfies(model.Capabilities, probeRequirements[check.Operation]):
			results[i] = failedStatus("capability_not_declared")
		case check.Operation == "vision":
			results[i] = ProbeStatus{Kind: probeNotTested}
		default:
			results[i] = r.runner.Probe(ctx, ProbeTarget{
				Provider: r.provider, Endpoint: r.definition.Endpoint, ModelID: model.ModelID,
				Credential: r.credential, Operation: check.Operation,
				AllowPrivateEndpoint: r.allowPrivate, EndpointResolver: r.resolver,
			})
		}
	}
	models := []ModelProbe{}
	for _, model := range r.definition.Models {
		var probe *ModelProbe
		for i, check := range checks {
			if check.ModelID != model.ModelID {
				continue
			}
			if probe == nil {
				models = append(models, ModelProbe{ModelID: model.ModelID, Checks: []ModelProbeCheck{}})
				probe = &models[len(models)-1]
			}
			probe.Checks = append(probe.Checks, ModelProbeCheck{Operation: check.Operation, Status: results[i]})
		}
	}
	return connectionStatus(results), models
}

func connectionStatus(results []ProbeStatus) ProbeStatus {
	var latest *int64
	for _, status := range results {
		if status.Kind == probeVerified {
			if latest == nil || (status.TestedAtMS != nil && *status.TestedAtMS > *latest) {
				latest = status.TestedAtMS
			}
			if latest == nil {
				latest = nowMS()
			}
		}
	}
	if latest != nil {
		return ProbeStatus{Kind: probeVerified, TestedAtMS: latest}
	}
	for _, status := range results {
		if status.Kind == probeFailed && status.ErrorKind != nil && slices.Contains(connectionErrorKinds, *status.ErrorKind) {
			result := failedStatus(*status.ErrorKind)
			result.TestedAtMS = status.TestedAtMS
			if result.TestedAtMS == nil {
				result.TestedAtMS = nowMS()
			}
			return result
		}
	}
	return ProbeStatus{Kind: probeNotTested}
}

// ProbeProfile probes a stored profile and persists the evidence when the
// profile did not change meanwhile.
func (s *ProfileStore) ProbeProfile(ctx context.Context, input ProbeProfileInput) (ProbeResult, error) {
	if err := requireField("workspaceId", input.WorkspaceID); err != nil {
		return ProbeResult{}, err
	}
	if err := requireField("profileId", input.ProfileID); err != nil {
		return ProbeResult{}, err
	}
	profile, err := getProfile(ctx, s.db, input.WorkspaceID, input.ProfileID, false)
	if errors.Is(err, ErrProfileNotFound) {
		return ProbeResult{}, invalidf("BYOK profile not found")
	}
	if err != nil {
		return ProbeResult{}, err
	}
	policy, err := s.Policy(ctx)
	if err != nil {
		return ProbeResult{}, err
	}
	resolver := s.endpointResolver()
	if err := policy.Admit(ctx, profile.Provider, profile.Definition.Endpoint, resolver); err != nil {
		return ProbeResult{}, err
	}
	checks, err := validateChecks(profile.Definition, input.Checks)
	if err != nil {
		return ProbeResult{}, err
	}
	credential, err := s.decryptServer(ctx, profile)
	if err != nil {
		return ProbeResult{}, err
	}
	defer credential.Destroy()
	run := probeRun{provider: profile.Provider, definition: profile.Definition, credential: credential.Bytes(),
		allowPrivate: policy.PrivateEndpointSupported, resolver: resolver, runner: s.probeRunner()}
	connection, models := run.run(ctx, checks)
	result := ProbeResult{DefinitionFingerprint: definitionFingerprint(profile.Definition), Connection: connection, Models: models}
	validationJSON, err := marshalValidation(&Validation{
		DefinitionFingerprint: result.DefinitionFingerprint, CredentialGeneration: profile.CredentialGeneration,
		Connection: connection, Models: models,
	})
	if err != nil {
		return ProbeResult{}, err
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE ai_workspace_byok_configs SET validation = $5
		WHERE workspace_id = $1 AND id = $2 AND revision = $3 AND credential_generation = $4`,
		profile.WorkspaceID, profile.ProfileID, profile.Revision, profile.CredentialGeneration, validationJSON)
	if err != nil {
		return ProbeResult{}, err
	}
	result.Stale = tag.RowsAffected() == 0
	return result, nil
}

// ProbeDraft probes an unsaved definition without persisting anything.
func (s *ProfileStore) ProbeDraft(ctx context.Context, input ProbeDraftInput) (ProbeResult, error) {
	if err := requireField("workspaceId", input.WorkspaceID); err != nil {
		return ProbeResult{}, err
	}
	definition, err := s.admitDefinition(ctx, input.Provider, input.Definition)
	if err != nil {
		return ProbeResult{}, err
	}
	checks, err := validateChecks(definition, input.Checks)
	if err != nil {
		return ProbeResult{}, err
	}
	var credential *SensitiveCredential
	switch {
	case input.Credential != nil && input.ProfileID == nil && input.ExpectedRevision == nil:
		if strings.TrimSpace(*input.Credential) == "" {
			return ProbeResult{}, invalidf("credential is required")
		}
		credential = NewSensitiveCredential([]byte(*input.Credential))
	case input.Credential == nil && input.ProfileID != nil && input.ExpectedRevision != nil:
		profile, err := getProfile(ctx, s.db, input.WorkspaceID, *input.ProfileID, false)
		if errors.Is(err, ErrProfileNotFound) {
			return ProbeResult{}, invalidf("BYOK profile not found")
		}
		if err != nil {
			return ProbeResult{}, err
		}
		if profile.Provider != input.Provider || *input.ExpectedRevision < 1 || profile.Revision != *input.ExpectedRevision {
			return ProbeResult{}, ErrRevisionConflict
		}
		if credential, err = s.decryptServer(ctx, profile); err != nil {
			return ProbeResult{}, err
		}
	default:
		return ProbeResult{}, invalidf("exactly one BYOK credential source is required")
	}
	defer credential.Destroy()
	policy, err := s.Policy(ctx)
	if err != nil {
		return ProbeResult{}, err
	}
	run := probeRun{provider: input.Provider, definition: definition, credential: credential.Bytes(),
		allowPrivate: policy.PrivateEndpointSupported, resolver: s.endpointResolver(), runner: s.probeRunner()}
	connection, models := run.run(ctx, checks)
	return ProbeResult{DefinitionFingerprint: definitionFingerprint(definition), Connection: connection, Models: models}, nil
}
