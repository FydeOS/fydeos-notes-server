// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	leasePurpose       = "copilot_byok_local_lease"
	activeLeasePurpose = "copilot_byok_local_lease:active"
	leaseTTL           = 10 * time.Minute
)

type leaseProvider struct {
	Provider            string            `json:"provider"`
	Name                string            `json:"name"`
	Description         *string           `json:"description"`
	EncryptedCredential string            `json:"encryptedCredential"`
	Definition          ProfileDefinition `json:"definition"`
	Enabled             bool              `json:"enabled"`
}

type leasePayload struct {
	WorkspaceID string          `json:"workspaceId"`
	UserID      string          `json:"userId"`
	Providers   []leaseProvider `json:"providers"`
}

type activeLeasePayload struct {
	LeaseID string `json:"leaseId"`
}

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

// loadLease reads a live lease by id.
func (s *ProfileStore) loadLease(ctx context.Context, leaseID string) (leasePayload, bool, error) {
	var raw []byte
	err := s.db.QueryRow(ctx, `
		SELECT payload FROM runtime_states
		WHERE purpose = $1 AND token_hash = $2 AND consumed_at IS NULL AND expires_at > now()`,
		leasePurpose, sha256Hex([]byte(leaseID))).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return leasePayload{}, false, nil
	}
	if err != nil {
		return leasePayload{}, false, err
	}
	var payload leasePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return leasePayload{}, false, nil
	}
	return payload, true, nil
}

// CreateLocalLease uploads device-held credentials encrypted for a short TTL.
// Identical uploads while a lease is live return that lease.
func (s *ProfileStore) CreateLocalLease(ctx context.Context, input CreateLocalLeaseInput) (LocalLeaseResult, error) {
	if err := requireField("workspaceId", input.WorkspaceID); err != nil {
		return LocalLeaseResult{}, err
	}
	if err := requireField("userId", input.UserID); err != nil {
		return LocalLeaseResult{}, err
	}
	if len(input.Providers) == 0 {
		return LocalLeaseResult{}, invalidf("providers is required")
	}
	definitions := make([]ProfileDefinition, len(input.Providers))
	for i, provider := range input.Providers {
		if err := requireField("name", provider.Name); err != nil {
			return LocalLeaseResult{}, err
		}
		if err := requireField("credential", provider.Credential); err != nil {
			return LocalLeaseResult{}, err
		}
		definition, err := s.admitDefinition(ctx, provider.Provider, provider.Definition)
		if err != nil {
			return LocalLeaseResult{}, err
		}
		definitions[i] = definition
	}
	secret, err := s.rootSecret(ctx)
	if err != nil {
		return LocalLeaseResult{}, err
	}
	defer clear(secret)
	key, err := NewCredentialEnvelopeKey(secret)
	if err != nil {
		return LocalLeaseResult{}, err
	}
	defer key.Destroy()

	mac := hmac.New(sha256.New, secret)
	parts := []string{input.WorkspaceID, input.UserID}
	for i, provider := range input.Providers {
		parts = append(parts, provider.Provider, provider.Credential, string(compactDefinitionJSON(definitions[i])))
	}
	mac.Write([]byte(strings.Join(parts, "\x00")))
	activeHash := sha256Hex(mac.Sum(nil))

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return LocalLeaseResult{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('byok-local-lease:' || $1, 0))`, activeHash); err != nil {
		return LocalLeaseResult{}, err
	}
	var activeRaw []byte
	err = tx.QueryRow(ctx, `
		SELECT payload FROM runtime_states
		WHERE purpose = $1 AND token_hash = $2 AND consumed_at IS NULL AND expires_at > now()`,
		activeLeasePurpose, activeHash).Scan(&activeRaw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return LocalLeaseResult{}, err
	}
	if err == nil {
		var active activeLeasePayload
		if json.Unmarshal(activeRaw, &active) == nil && active.LeaseID != "" {
			var expiresAt time.Time
			err := tx.QueryRow(ctx, `
				SELECT expires_at FROM runtime_states
				WHERE purpose = $1 AND token_hash = $2 AND consumed_at IS NULL AND expires_at > now()`,
				leasePurpose, sha256Hex([]byte(active.LeaseID))).Scan(&expiresAt)
			if err == nil {
				return LocalLeaseResult{LeaseID: active.LeaseID, ExpiresAtMS: expiresAt.UnixMilli()}, tx.Commit(ctx)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return LocalLeaseResult{}, err
			}
		}
	}

	leaseID := uuid.NewString()
	payload := leasePayload{WorkspaceID: input.WorkspaceID, UserID: input.UserID, Providers: make([]leaseProvider, len(input.Providers))}
	for i, provider := range input.Providers {
		encrypted, err := key.Encrypt([]byte(provider.Credential),
			LocalAAD(input.WorkspaceID, input.UserID, leaseID, uint64(i), provider.Provider, endpointIdentity(definitions[i].Endpoint)))
		if err != nil {
			return LocalLeaseResult{}, err
		}
		payload.Providers[i] = leaseProvider{
			Provider: provider.Provider, Name: provider.Name, Description: provider.Description,
			EncryptedCredential: encrypted, Definition: definitions[i], Enabled: provider.Enabled,
		}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return LocalLeaseResult{}, err
	}
	activeJSON, _ := json.Marshal(activeLeasePayload{LeaseID: leaseID})
	// Truncate to milliseconds so the reported expiry round-trips exactly.
	var expiresAt time.Time
	if err := tx.QueryRow(ctx, `SELECT date_trunc('milliseconds', now() + $1::interval)`, leaseTTL.String()).Scan(&expiresAt); err != nil {
		return LocalLeaseResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO runtime_states (purpose, token_hash, payload, expires_at)
		VALUES ($1, $2, $3, $4)`, leasePurpose, sha256Hex([]byte(leaseID)), payloadJSON, expiresAt); err != nil {
		return LocalLeaseResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO runtime_states (purpose, token_hash, payload, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (purpose, token_hash) DO UPDATE
		SET payload = EXCLUDED.payload, expires_at = EXCLUDED.expires_at, consumed_at = NULL, updated_at = now()`,
		activeLeasePurpose, activeHash, activeJSON, expiresAt); err != nil {
		return LocalLeaseResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LocalLeaseResult{}, err
	}
	return LocalLeaseResult{LeaseID: leaseID, ExpiresAtMS: expiresAt.UnixMilli()}, nil
}
