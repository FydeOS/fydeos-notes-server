// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

func copilotParamsRequireWorkspace(value any) bool {
	params, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{"scopeSelectors", "focusSelectors", "preferredSourceIds"} {
		if items, ok := params[key].([]any); ok && len(items) > 0 {
			return true
		}
	}
	return false
}
