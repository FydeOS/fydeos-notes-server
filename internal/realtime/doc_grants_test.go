// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package realtime

import "testing"

func TestParseDocGrantsInputMatchesLegacyPaginationBounds(t *testing.T) {
	workspaceID, docID, first, offset, cursor, err := parseDocGrantsInput(map[string]any{
		"workspaceId": "workspace-1", "docId": "doc-1", "pagination": map[string]any{"first": 500, "offset": -4},
	})
	if err != nil || workspaceID != "workspace-1" || docID != "doc-1" || first != 100 || offset != 0 || cursor != "" {
		t.Fatalf("parsed grants request = %q %q %d %d %q, %v", workspaceID, docID, first, offset, cursor, err)
	}

	for _, input := range []map[string]any{
		{"workspaceId": "workspace-1", "docId": "doc-1", "pagination": map[string]any{"first": 8, "offset": 1, "after": "Y3Vyc29y"}},
		{"workspaceId": "workspace-1", "docId": "doc-1", "extra": true},
		{"workspaceId": "workspace-1", "docId": "doc-1", "pagination": map[string]any{"first": 8, "before": "Y3Vyc29y"}},
	} {
		if _, _, _, _, _, err := parseDocGrantsInput(input); err == nil {
			t.Fatalf("invalid grants input was accepted: %#v", input)
		}
	}
}
