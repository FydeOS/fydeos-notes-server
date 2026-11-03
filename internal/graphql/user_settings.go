// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"fmt"

	gqlgraphql "github.com/99designs/gqlgen/graphql"
)

func settingsInputValue(field gqlgraphql.Omittable[*bool]) (*bool, error) {
	if !field.IsSet() {
		return nil, nil
	}
	value := field.Value()
	if value == nil {
		return nil, fmt.Errorf("Expected boolean, received null")
	}
	return value, nil
}
