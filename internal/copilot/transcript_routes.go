// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/byok"
	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/entitlements"
	entitlementspg "github.com/fydeos/fydeos-notes-server/internal/entitlements/postgres"
	"github.com/google/uuid"
)

type TranscriptRoutes struct {
	db       DB
	config   config.Config
	profiles TranscriptTargetResolver
}

func NewTranscriptRoutes(db DB, cfg config.Config, profiles TranscriptTargetResolver) *TranscriptRoutes {
	return &TranscriptRoutes{db: db, config: cfg, profiles: profiles}
}

func (r *TranscriptRoutes) ReserveCloudImageQuota(ctx context.Context, userID, workspaceID, sessionID, billingUnitID, provider, model string) (string, error) {
	if r == nil || r.db == nil || userID == "" || billingUnitID == "" {
		return "", fmt.Errorf("managed image quota is unavailable")
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "copilot-image-quota:"+userID); err != nil {
		return "", err
	}
	facts, err := entitlementspg.LoadFacts(ctx, tx, "user", []string{userID})
	if err != nil {
		return "", err
	}
	grant := entitlements.Resolve(entitlements.Context{Deployment: entitlements.Cloud, TargetType: "user", Now: time.Now()}, facts[userID])
	if grant.Limits.CopilotActionLimit == nil {
		return "", tx.Commit(ctx)
	}
	used, err := countUserQuotaCost(ctx, tx, userID)
	if err != nil {
		return "", err
	}
	if used >= int64(*grant.Limits.CopilotActionLimit) {
		return "", ErrManagedQuotaExceeded
	}
	reservationID := uuid.NewString()
	_, err = tx.Exec(ctx, `INSERT INTO ai_usage_events(id,workspace_id,user_id,provider,provider_source,billing_unit_id,feature_kind,model,session_id,created_at)
		VALUES($1,$2,$3,$4,'affine_cloud_image_reserved',$5,'image',$6,$7,now())`, reservationID, workspaceID, userID, provider, billingUnitID, model, sessionID)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return reservationID, nil
}

func (r *TranscriptRoutes) ReleaseCloudImageQuota(ctx context.Context, reservationID string) error {
	if r == nil || r.db == nil || reservationID == "" {
		return nil
	}
	_, err := r.db.Exec(ctx, `DELETE FROM ai_usage_events WHERE id=$1 AND provider_source='affine_cloud_image_reserved'`, reservationID)
	return err
}

func (r *TranscriptRoutes) ResolveServerTarget(ctx context.Context, workspaceID, userID, operation string) (byok.ChatTarget, error) {
	if operation != "transcript" && operation != "structured" && operation != "embedding" && operation != "image_edit" && operation != "image_generate" && operation != "image_process" {
		return byok.ChatTarget{}, byok.ErrTargetUnavailable
	}
	selfhost := entitlements.ParseDeployment(r.config.Deployment) == entitlements.SelfHosted
	// Metered actions in FydeOS managed selfhost must use the managed route. Its
	// text-only preset cannot start an audio task, even if a BYOK profile exists.
	if selfhost && r.config.FydeOSManagedCopilot {
		return byok.ChatTarget{}, byok.ErrTargetUnavailable
	}
	if r.profiles != nil {
		target, err := r.profiles.ResolveServerTarget(ctx, workspaceID, userID, operation)
		if err == nil || !errors.Is(err, byok.ErrNoCompatibleServerTarget) {
			return target, err
		}
	}
	if selfhost || r.db == nil {
		return byok.ChatTarget{}, byok.ErrTargetUnavailable
	}
	if r.config.FydeOSManagedCopilot && (operation == "transcript" || strings.HasPrefix(operation, "image_")) {
		return byok.ChatTarget{}, byok.ErrTargetUnavailable
	}
	allowed, err := r.managedAccess(ctx, userID)
	if err != nil {
		return byok.ChatTarget{}, err
	}
	if !allowed {
		return byok.ChatTarget{}, byok.ErrTargetUnavailable
	}
	if r.config.FydeOSManagedCopilot {
		if operation == "embedding" {
			return byok.ChatTarget{}, byok.ErrTargetUnavailable
		}
		return managedTranscriptTarget(managedTranscriptProfile{ID: "fydeos-managed-openai", Provider: "openai", Config: managedTranscriptSettings{APIKey: r.config.FydeOSManagedCopilotAPIKey, BaseURL: r.config.FydeOSManagedCopilotBaseURL, Dialect: "chat_completions"}}, r.config.FydeOSManagedCopilotModel, "fydeos_managed")
	}
	raw, found, err := config.ReadRuntimeValue(ctx, r.db, r.config.RuntimeConfigPath, "copilot.providers.profiles")
	if err != nil {
		return byok.ChatTarget{}, err
	}
	if !found {
		return byok.ChatTarget{}, byok.ErrTargetUnavailable
	}
	var profiles []managedTranscriptProfile
	if json.Unmarshal(raw, &profiles) != nil {
		return byok.ChatTarget{}, errTranscriptRoute
	}
	// Managed routes use fixed models from the BYOK catalog (model_catalog.json).
	model := "gemini-3.8-flash"
	if operation == "embedding" {
		return r.managedEmbeddingTarget(profiles)
	}
	if operation == "structured" {
		model = "gpt-6-luna"
	}
	if operation == "image_edit" {
		model = "flux-1/dev/image-to-image"
	}
	if operation == "image_generate" || operation == "image_process" {
		model = "gpt-image-2.5-sunburst"
	}
	var match *managedTranscriptProfile
	for i := range profiles {
		p := &profiles[i]
		if p.Enabled != nil && !*p.Enabled {
			continue
		}
		for _, candidate := range p.Models {
			if candidate == model {
				if match != nil {
					return byok.ChatTarget{}, errTranscriptRoute
				}
				match = p
				break
			}
		}
	}
	if match == nil || !byok.ModelSupportsServerAction(match.Provider, model, operation) {
		return byok.ChatTarget{}, byok.ErrTargetUnavailable
	}
	return managedTranscriptTarget(*match, model, "affine_cloud")
}

func (r *TranscriptRoutes) managedEmbeddingTarget(profiles []managedTranscriptProfile) (byok.ChatTarget, error) {
	var match *managedTranscriptProfile
	model := ""
	for index := range profiles {
		profile := &profiles[index]
		if profile.Enabled != nil && !*profile.Enabled {
			continue
		}
		candidate := managedEmbeddingModel(profile.Provider)
		if candidate == "" || !containsManagedModel(profile.Models, candidate) || !byok.ModelSupportsServerAction(profile.Provider, candidate, "embedding") {
			continue
		}
		if match != nil {
			return byok.ChatTarget{}, errTranscriptRoute
		}
		match, model = profile, candidate
	}
	if match == nil {
		return byok.ChatTarget{}, byok.ErrTargetUnavailable
	}
	return managedTranscriptTarget(*match, model, "affine_cloud")
}

func managedEmbeddingModel(provider string) string {
	switch provider {
	case "openai":
		return "text-embedding-3-small"
	case "gemini":
		return "gemini-embedding-001"
	default:
		return ""
	}
}

func containsManagedModel(models []string, target string) bool {
	for _, model := range models {
		if model == target {
			return true
		}
	}
	return false
}

type managedTranscriptProfile struct {
	ID       string                    `json:"id"`
	Provider string                    `json:"type"`
	Enabled  *bool                     `json:"enabled"`
	Models   []string                  `json:"models"`
	Config   managedTranscriptSettings `json:"config"`
}
type managedTranscriptSettings struct {
	APIKey  string `json:"apiKey"`
	BaseURL string `json:"baseURL"`
	Dialect string `json:"openaiDialect"`
}

func managedTranscriptTarget(profile managedTranscriptProfile, model, source string) (byok.ChatTarget, error) {
	if profile.ID == "" || model == "" || strings.TrimSpace(profile.Config.APIKey) == "" {
		return byok.ChatTarget{}, errTranscriptRoute
	}
	defaults := map[string]string{"gemini": "https://generativelanguage.googleapis.com/v1beta", "openai": "https://api.openai.com/v1", "anthropic": "https://api.anthropic.com", "fal": "https://fal.run"}
	base, ok := defaults[profile.Provider]
	if !ok {
		return byok.ChatTarget{}, byok.ErrTargetUnavailable
	}
	endpoint := byok.Endpoint{Kind: "provider_default"}
	if profile.Config.BaseURL != "" {
		var err error
		base, err = byok.CanonicalizeEndpoint(profile.Config.BaseURL)
		if err != nil {
			return byok.ChatTarget{}, errTranscriptRoute
		}
		parsed, err := url.Parse(base)
		if err != nil || parsed.Scheme != "https" {
			return byok.ChatTarget{}, errTranscriptRoute
		}
	}
	if profile.Provider == "openai" {
		endpoint.Dialect = profile.Config.Dialect
		if endpoint.Dialect == "" {
			endpoint.Dialect = "responses"
		}
		if endpoint.Dialect != "responses" && endpoint.Dialect != "chat_completions" {
			return byok.ChatTarget{}, errTranscriptRoute
		}
	}
	return byok.ChatTarget{ProfileID: profile.ID, Source: source, Provider: profile.Provider, ModelID: model, Endpoint: endpoint, BaseURL: base, Credential: byok.NewSensitiveCredential([]byte(profile.Config.APIKey))}, nil
}

func (r *TranscriptRoutes) managedAccess(ctx context.Context, userID string) (bool, error) {
	if userID == "" {
		return false, nil
	}
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return false, err
	}
	if r.config.FydeOSManagedCopilot {
		used, err := countFydeosManagedRequests(ctx, tx, userID, time.Now())
		return used < FydeosManagedQuotaLimit, err
	}
	facts, err := entitlementspg.LoadFacts(ctx, tx, "user", []string{userID})
	if err != nil {
		return false, err
	}
	grant := entitlements.Resolve(entitlements.Context{Deployment: entitlements.Cloud, TargetType: "user", Now: time.Now()}, facts[userID])
	if grant.Limits.CopilotActionLimit == nil {
		return true, nil
	}
	used, err := countUserQuotaCost(ctx, tx, userID)
	return used < int64(*grant.Limits.CopilotActionLimit), err
}
