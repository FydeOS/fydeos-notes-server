// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	_ "embed"
	"encoding/json"
)

//go:embed prompt-actions.json
var promptActionsJSON []byte

var builtinPromptActions = func() map[string]string {
	var actions map[string]string
	if err := json.Unmarshal(promptActionsJSON, &actions); err != nil {
		panic("decode embedded Copilot prompt actions: " + err.Error())
	}
	return actions
}()

func BuiltinPromptAction(name string) (string, bool) {
	action, ok := builtinPromptActions[name]
	return action, ok
}
