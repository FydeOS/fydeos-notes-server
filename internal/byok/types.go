// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

// Package byok manages workspace "bring your own key" AI provider profiles:
// definition validation, runtime policy, encrypted credential storage, local
// leases, connectivity probes and call-target resolution.
package byok

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// EmbeddingDimensions is the fixed vector length produced by EmbedText.
const EmbeddingDimensions = 1024

var (
	ErrInvalidInput          = errors.New("invalid BYOK input")
	ErrTargetUnavailable     = errors.New("BYOK target is unavailable")
	ErrProfileNotFound       = errors.New("BYOK profile not found")
	ErrRevisionConflict      = errors.New("byok_revision_conflict")
	ErrProfileNameConflict   = errors.New("BYOK profile name already exists")
	ErrEntitlementRequired   = errors.New("BYOK requires a Pro, Team, Believer, or AI entitlement.")
	ErrCredentialUnavailable = errors.New("credential_unavailable")
	// ErrNoCompatibleServerTarget is the only resolution error after which
	// callers may fall back to a managed provider.
	ErrNoCompatibleServerTarget = fmt.Errorf("%w: no compatible server target", ErrTargetUnavailable)
	// ErrWorkspaceOwnerNotFound reports a workspace without an active owner.
	ErrWorkspaceOwnerNotFound = errors.New("workspace_owner_not_found")
	// ErrEmbeddingRequest reports any EmbedText failure.
	ErrEmbeddingRequest = errors.New("BYOK embedding request failed")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

type Capability struct {
	Input            []string `json:"input"`
	Output           []string `json:"output"`
	Features         []string `json:"features"`
	AttachmentKinds  []string `json:"attachmentKinds"`
	AttachmentSource []string `json:"attachmentSources"`
}

type ModelDeclaration struct {
	ModelID      string       `json:"modelId"`
	Enabled      bool         `json:"enabled"`
	Capabilities []Capability `json:"capabilities"`
}

type EndpointInput struct {
	Kind    string  `json:"kind"`
	URL     *string `json:"url"`
	Dialect *string `json:"dialect"`
}

type ProfileDefinitionInput struct {
	Endpoint EndpointInput      `json:"endpoint"`
	Models   []ModelDeclaration `json:"models"`
}

type Endpoint struct {
	Kind    string `json:"kind"`
	URL     string `json:"url,omitempty"`
	Dialect string `json:"dialect,omitempty"`
}

type ProfileDefinition struct {
	Endpoint Endpoint           `json:"endpoint"`
	Models   []ModelDeclaration `json:"models"`
}

type ProbeStatus struct {
	Kind       string  `json:"kind"`
	TestedAtMS *int64  `json:"testedAtMs,omitempty"`
	ErrorKind  *string `json:"errorKind,omitempty"`
}

type ModelProbeCheck struct {
	Operation string      `json:"operation"`
	Status    ProbeStatus `json:"status"`
}

type ModelProbe struct {
	ModelID string            `json:"modelId"`
	Checks  []ModelProbeCheck `json:"checks"`
}

type ProbeCheckInput struct {
	ModelID   string
	Operation string
}

type ProbeProfileInput struct {
	WorkspaceID string
	ProfileID   string
	Checks      []ProbeCheckInput
}

type ProbeDraftInput struct {
	WorkspaceID      string
	Provider         string
	Credential       *string
	ProfileID        *string
	ExpectedRevision *int64
	Definition       ProfileDefinitionInput
	Checks           []ProbeCheckInput
}

type ProbeResult struct {
	DefinitionFingerprint string       `json:"definitionFingerprint"`
	Stale                 bool         `json:"stale"`
	Connection            ProbeStatus  `json:"connection"`
	Models                []ModelProbe `json:"models"`
}

type Validation struct {
	DefinitionFingerprint string       `json:"definitionFingerprint"`
	CredentialGeneration  int64        `json:"credentialGeneration"`
	Connection            ProbeStatus  `json:"connection"`
	Models                []ModelProbe `json:"models"`
}

type Profile struct {
	ProfileID            string            `json:"profileId"`
	WorkspaceID          string            `json:"workspaceId"`
	Provider             string            `json:"provider"`
	Name                 string            `json:"name"`
	Description          *string           `json:"description"`
	Definition           ProfileDefinition `json:"definition"`
	Enabled              bool              `json:"enabled"`
	SortOrder            int64             `json:"sortOrder"`
	Revision             int64             `json:"revision"`
	CredentialGeneration int64             `json:"credentialGeneration"`
	Validation           *Validation       `json:"validation"`
	EncryptedCredential  string            `json:"-"`
	CreatedAt            time.Time         `json:"-"`
}

type Catalog struct {
	Version   string            `json:"version"`
	Providers []CatalogProvider `json:"providers"`
}

type CatalogProvider struct {
	Provider string         `json:"provider"`
	Models   []CatalogModel `json:"models"`
}

type CatalogModel struct {
	ModelID      string       `json:"modelId"`
	DisplayName  string       `json:"displayName"`
	Recommended  bool         `json:"recommended"`
	Capabilities []Capability `json:"capabilities"`
}

type Entitlement struct {
	Server bool
	Local  bool
}

type UsagePoint struct {
	Date        time.Time
	FeatureKind string
	TotalTokens int64
}

// ChatTarget is a resolved provider call target. The caller owns Credential
// and must Destroy it.
type ChatTarget struct {
	ProfileID  string
	Source     string
	Provider   string
	ModelID    string
	Endpoint   Endpoint
	BaseURL    string
	Credential *SensitiveCredential
	Transport  http.RoundTripper
}

type CreateProfileInput struct {
	WorkspaceID string
	Provider    string
	Name        string
	Description *string
	Credential  string
	Definition  ProfileDefinitionInput
	Enabled     bool
	ActorUserID string
}

type ReplaceProfileInput struct {
	WorkspaceID      string
	ProfileID        string
	ExpectedRevision int64
	Name             string
	Description      *string
	Definition       ProfileDefinitionInput
	Credential       *string
	Enabled          bool
	ActorUserID      string
}

type RotateCredentialInput struct {
	WorkspaceID      string
	ProfileID        string
	ExpectedRevision int64
	Credential       string
	ActorUserID      string
}

type ProfileOrderInput struct {
	ProfileID        string
	ExpectedRevision int64
}

type LocalLeaseProviderInput struct {
	Provider    string
	Name        string
	Description *string
	Credential  string
	Definition  ProfileDefinitionInput
	Enabled     bool
}

type CreateLocalLeaseInput struct {
	WorkspaceID string
	UserID      string
	Providers   []LocalLeaseProviderInput
}

type LocalLeaseResult struct {
	LeaseID     string
	ExpiresAtMS int64
}

// EndpointResolver resolves a host name to IP addresses.
type EndpointResolver func(context.Context, string) ([]netip.Addr, error)

// KeyLoader returns the application root secret.
type KeyLoader func(context.Context) (string, error)

type Database interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Begin(context.Context) (pgx.Tx, error)
}

type PolicyConfigReader interface {
	config.RuntimeConfigReader
}
