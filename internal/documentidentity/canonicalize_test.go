// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package documentidentity

import (
	"errors"
	"testing"
)

func TestCanonicalize(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		workspace string
		want      Identity
		wantErr   error
	}{
		{name: "workspace root", raw: "workspace-1", workspace: "workspace-1", want: Identity{WorkspaceID: "workspace-1", DocumentID: "workspace-1", Variant: Workspace}},
		{name: "space alias", raw: "space:doc-1", workspace: "workspace-1", want: Identity{WorkspaceID: "workspace-1", DocumentID: "doc-1", Variant: Space}},
		{name: "page alias", raw: "page:doc-1", workspace: "workspace-1", want: Identity{WorkspaceID: "workspace-1", DocumentID: "doc-1", Variant: Page}},
		{name: "settings alias", raw: "settings:doc-1", workspace: "workspace-1", want: Identity{WorkspaceID: "workspace-1", DocumentID: "doc-1", Variant: Settings}},
		{name: "workspace prefix is overridden by hint", raw: "other:space:doc-1", workspace: "workspace-1", want: Identity{WorkspaceID: "workspace-1", DocumentID: "doc-1", Variant: Space}},
		{name: "legacy four part alias", raw: "other:space:page:doc-1", workspace: "workspace-1", want: Identity{WorkspaceID: "workspace-1", DocumentID: "doc-1", Variant: Space}},
		{name: "unprefixed non-root document", raw: "doc-1", workspace: "workspace-1", want: Identity{WorkspaceID: "workspace-1", DocumentID: "doc-1", Variant: Unknown}},
		{name: "empty identity", raw: "", workspace: "workspace-1", wantErr: ErrEmpty},
		{name: "empty workspace", raw: "doc-1", workspace: "", wantErr: ErrWorkspaceRequired},
		{name: "unknown variant", raw: "other:doc-1", workspace: "workspace-1", wantErr: ErrInvalidVariant},
		{name: "empty document", raw: "space:", workspace: "workspace-1", wantErr: ErrDocumentRequired},
		{name: "invalid four part alias", raw: "workspace:page:space:doc-1", workspace: "workspace-1", wantErr: ErrInvalidFormat},
		{name: "too many components", raw: "a:b:c:d:e", workspace: "workspace-1", wantErr: ErrInvalidFormat},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Canonicalize(test.raw, test.workspace)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("Canonicalize() error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Canonicalize() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("Canonicalize() = %+v, want %+v", got, test.want)
			}
		})
	}
}
