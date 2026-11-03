// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	sourceServer = "byok_server"
	sourceLocal  = "byok_local"
)

// targetCandidate is a provider/model choice about to be turned into a target.
type targetCandidate struct {
	profileID  string
	source     string
	provider   string
	endpoint   Endpoint
	model      ModelDeclaration
	decrypt    func(*CredentialEnvelopeKey) (*SensitiveCredential, error)
	policy     Policy
	requires   Capability
	providerOn bool
}

func unavailable(reason string) error { return fmt.Errorf("%w: %s", ErrTargetUnavailable, reason) }

// finish admits a candidate and decrypts its credential.
func (s *ProfileStore) finish(ctx context.Context, c targetCandidate) (ChatTarget, error) {
	if !c.providerOn {
		return ChatTarget{}, unavailable("profile disabled")
	}
	if !c.model.Enabled || !satisfies(c.model.Capabilities, c.requires) {
		return ChatTarget{}, unavailable("model unavailable")
	}
	resolver := s.endpointResolver()
	addrs, err := c.policy.admit(ctx, c.provider, c.endpoint, resolver)
	if err != nil {
		return ChatTarget{}, unavailable("endpoint not admitted")
	}
	key, err := s.envelopeKey(ctx)
	if err != nil {
		return ChatTarget{}, err
	}
	defer key.Destroy()
	credential, err := c.decrypt(key)
	if err != nil {
		return ChatTarget{}, ErrCredentialUnavailable
	}
	target := ChatTarget{
		ProfileID: c.profileID, Source: c.source, Provider: c.provider, ModelID: c.model.ModelID,
		Endpoint: c.endpoint, BaseURL: defaultBaseURLs[c.provider], Credential: credential,
	}
	if c.endpoint.Kind == endpointOpenAICompatible {
		target.BaseURL = c.endpoint.URL
		target.Transport = c.policy.customTransport(c.endpoint, addrs, resolver)
	}
	return target, nil
}

func (s *ProfileStore) serverCandidate(policy Policy, profile Profile, model ModelDeclaration, requires Capability) targetCandidate {
	return targetCandidate{
		profileID: profile.ProfileID, source: sourceServer, provider: profile.Provider, endpoint: profile.Definition.Endpoint,
		model: model, policy: policy, requires: requires, providerOn: profile.Enabled,
		decrypt: func(key *CredentialEnvelopeKey) (*SensitiveCredential, error) {
			return key.Decrypt(profile.EncryptedCredential, ServerAAD(profile.WorkspaceID, profile.ProfileID, profile.Provider, endpointIdentity(profile.Definition.Endpoint)))
		},
	}
}

// serverEntitled reports whether the workspace may use server profiles.
func (s *ProfileStore) serverEntitled(ctx context.Context, workspaceID, actorID string) (bool, error) {
	entitlement, err := s.Entitlement(ctx, workspaceID, actorID)
	if errors.Is(err, ErrWorkspaceOwnerNotFound) {
		return false, nil
	}
	return entitlement.Server, err
}

func (s *ProfileStore) resolveServer(ctx context.Context, workspaceID, actorID, profileID, modelID string, requires Capability) (ChatTarget, error) {
	if workspaceID == "" || actorID == "" || profileID == "" || modelID == "" {
		return ChatTarget{}, unavailable("missing identifier")
	}
	entitled, err := s.serverEntitled(ctx, workspaceID, actorID)
	if err != nil {
		return ChatTarget{}, err
	}
	if !entitled {
		return ChatTarget{}, unavailable("not entitled")
	}
	profile, err := getProfile(ctx, s.db, workspaceID, profileID, false)
	if errors.Is(err, ErrProfileNotFound) {
		return ChatTarget{}, unavailable("profile not found")
	}
	if err != nil {
		return ChatTarget{}, err
	}
	model, ok := findModel(profile.Definition, modelID)
	if !ok {
		return ChatTarget{}, unavailable("model not declared")
	}
	policy, err := s.Policy(ctx)
	if err != nil {
		return ChatTarget{}, err
	}
	return s.finish(ctx, s.serverCandidate(policy, profile, model, requires))
}

// ResolveChatTarget resolves a server profile model for chat.
func (s *ProfileStore) ResolveChatTarget(ctx context.Context, workspaceID, actorID, profileID, modelID string) (ChatTarget, error) {
	return s.resolveServer(ctx, workspaceID, actorID, profileID, modelID, probeRequirements["chat"])
}

// ResolveImageTarget resolves a server profile model for an image operation.
func (s *ProfileStore) ResolveImageTarget(ctx context.Context, workspaceID, actorID, profileID, modelID, operation string) (ChatTarget, error) {
	requires, ok := imageRequirements[operation]
	if !ok {
		return ChatTarget{}, unavailable("unknown image operation")
	}
	return s.resolveServer(ctx, workspaceID, actorID, profileID, modelID, requires)
}

// ResolveServerTarget picks the first enabled capable server profile model
// for a server action. Only ErrNoCompatibleServerTarget permits fallback.
func (s *ProfileStore) ResolveServerTarget(ctx context.Context, workspaceID, actorID, operation string) (ChatTarget, error) {
	requires, ok := serverRequirements[operation]
	if !ok || workspaceID == "" || actorID == "" {
		return ChatTarget{}, unavailable("unknown server operation")
	}
	entitled, err := s.serverEntitled(ctx, workspaceID, actorID)
	if err != nil {
		return ChatTarget{}, err
	}
	if !entitled {
		return ChatTarget{}, ErrNoCompatibleServerTarget
	}
	policy, err := s.Policy(ctx)
	if err != nil {
		return ChatTarget{}, err
	}
	profiles, err := s.List(ctx, workspaceID)
	if err != nil {
		return ChatTarget{}, err
	}
	for _, profile := range profiles {
		if !profile.Enabled || !policy.Allows(profile.Provider, profile.Definition.Endpoint) {
			continue
		}
		for _, model := range profile.Definition.Models {
			if model.Enabled && satisfies(model.Capabilities, requires) {
				return s.finish(ctx, s.serverCandidate(policy, profile, model, requires))
			}
		}
	}
	return ChatTarget{}, ErrNoCompatibleServerTarget
}

// ResolveLocalChatTarget resolves a local-lease provider model for chat.
func (s *ProfileStore) ResolveLocalChatTarget(ctx context.Context, workspaceID, actorID, leaseID, profileID, modelID string) (ChatTarget, error) {
	return s.resolveLocal(ctx, workspaceID, actorID, leaseID, profileID, modelID, probeRequirements["chat"])
}

// ResolveLocalImageTarget resolves a local-lease provider model for an image operation.
func (s *ProfileStore) ResolveLocalImageTarget(ctx context.Context, workspaceID, actorID, leaseID, profileID, modelID, operation string) (ChatTarget, error) {
	requires, ok := imageRequirements[operation]
	if !ok {
		return ChatTarget{}, unavailable("unknown image operation")
	}
	return s.resolveLocal(ctx, workspaceID, actorID, leaseID, profileID, modelID, requires)
}

// parseLocalProfileID splits "<leaseID>:<n>".
func parseLocalProfileID(leaseID, profileID string) (uint64, bool) {
	rest, ok := strings.CutPrefix(profileID, leaseID+":")
	if !ok || leaseID == "" || rest == "" {
		return 0, false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	index, err := strconv.ParseUint(rest, 10, 64)
	return index, err == nil
}

func (s *ProfileStore) resolveLocal(ctx context.Context, workspaceID, actorID, leaseID, profileID, modelID string, requires Capability) (ChatTarget, error) {
	index, ok := parseLocalProfileID(leaseID, profileID)
	if !ok || workspaceID == "" || actorID == "" || modelID == "" {
		return ChatTarget{}, unavailable("invalid local profile")
	}
	entitlement, err := s.Entitlement(ctx, workspaceID, actorID)
	if err != nil {
		if errors.Is(err, ErrWorkspaceOwnerNotFound) {
			return ChatTarget{}, unavailable("not entitled")
		}
		return ChatTarget{}, err
	}
	if !entitlement.Local {
		return ChatTarget{}, unavailable("not entitled")
	}
	policy, err := s.Policy(ctx)
	if err != nil {
		return ChatTarget{}, err
	}
	if !policy.Enabled {
		return ChatTarget{}, unavailable("disabled by policy")
	}
	lease, ok, err := s.loadLease(ctx, leaseID)
	if err != nil {
		return ChatTarget{}, err
	}
	if !ok || lease.WorkspaceID != workspaceID || lease.UserID != actorID || index >= uint64(len(lease.Providers)) {
		return ChatTarget{}, unavailable("lease unavailable")
	}
	entry := lease.Providers[index]
	if !policy.Allows(entry.Provider, entry.Definition.Endpoint) {
		return ChatTarget{}, unavailable("provider not allowed")
	}
	model, ok := findModel(entry.Definition, modelID)
	if !ok {
		return ChatTarget{}, unavailable("model not declared")
	}
	return s.finish(ctx, targetCandidate{
		profileID: profileID, source: sourceLocal, provider: entry.Provider, endpoint: entry.Definition.Endpoint,
		model: model, policy: policy, requires: requires, providerOn: entry.Enabled,
		decrypt: func(key *CredentialEnvelopeKey) (*SensitiveCredential, error) {
			return key.Decrypt(entry.EncryptedCredential, LocalAAD(workspaceID, actorID, leaseID, index, entry.Provider, endpointIdentity(entry.Definition.Endpoint)))
		},
	})
}
