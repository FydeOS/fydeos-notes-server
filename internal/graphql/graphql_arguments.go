// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"

	gqlgraphql "github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/ast"
)

func graphQLArgumentIsExplicitNull(ctx context.Context, argumentName string) bool {
	field := gqlgraphql.GetFieldContext(ctx)
	if field == nil || field.Field.Field == nil {
		return false
	}
	for _, argument := range field.Field.Arguments {
		if argument.Name != argumentName || argument.Value == nil {
			continue
		}
		switch argument.Value.Kind {
		case ast.NullValue:
			return true
		case ast.Variable:
			if !gqlgraphql.HasOperationContext(ctx) {
				return false
			}
			value, exists := gqlgraphql.GetOperationContext(ctx).Variables[argument.Value.Raw]
			return exists && value == nil
		}
	}
	return false
}
