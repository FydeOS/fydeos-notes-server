// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documentidentity

import (
	"errors"
	"strings"
)

type Variant string

const (
	Workspace Variant = "workspace"
	Page      Variant = "page"
	Space     Variant = "space"
	Settings  Variant = "settings"
	Unknown   Variant = "unknown"
)

type Identity struct {
	WorkspaceID string
	DocumentID  string
	Variant     Variant
}

func (identity Identity) IsWorkspace() bool {
	return identity.Variant == Workspace
}

var (
	ErrEmpty             = errors.New("empty document identity")
	ErrWorkspaceRequired = errors.New("workspace required")
	ErrInvalidFormat     = errors.New("invalid document identity format")
	ErrInvalidVariant    = errors.New("invalid document variant")
	ErrDocumentRequired  = errors.New("document required")
)

// Canonicalize mirrors affine_core's canonicalize_document_identity rules.
func Canonicalize(rawID, workspaceHint string) (Identity, error) {
	if rawID == "" {
		return Identity{}, ErrEmpty
	}
	if workspaceHint == "" {
		return Identity{}, ErrWorkspaceRequired
	}

	parts := strings.Split(rawID, ":")
	var workspace string
	var variant Variant
	var document string

	switch len(parts) {
	case 1:
		if parts[0] != workspaceHint {
			workspace, variant, document = workspaceHint, Unknown, parts[0]
		} else {
			workspace, variant, document = parts[0], Workspace, parts[0]
		}
	case 2:
		parsed, ok := parseVariant(parts[0])
		if !ok {
			return Identity{}, ErrInvalidVariant
		}
		workspace, variant, document = workspaceHint, parsed, parts[1]
	case 3:
		parsed, ok := parseVariant(parts[1])
		if !ok {
			return Identity{}, ErrInvalidVariant
		}
		workspace, variant, document = workspaceHint, parsed, parts[2]
	case 4:
		if parts[1] != "space" || parts[2] != "page" {
			return Identity{}, ErrInvalidFormat
		}
		workspace, variant, document = workspaceHint, Space, parts[3]
	default:
		return Identity{}, ErrInvalidFormat
	}

	if workspace == "" {
		return Identity{}, ErrWorkspaceRequired
	}
	if variant != Workspace && document == "" {
		return Identity{}, ErrDocumentRequired
	}
	return Identity{WorkspaceID: workspace, DocumentID: document, Variant: variant}, nil
}

func parseVariant(value string) (Variant, bool) {
	switch Variant(value) {
	case Page, Space, Settings:
		return Variant(value), true
	default:
		return "", false
	}
}
