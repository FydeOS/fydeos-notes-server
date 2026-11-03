// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package model

import (
	"io"

	"github.com/99designs/gqlgen/graphql"
)

type JSONObject map[string]any

func (o JSONObject) MarshalGQL(w io.Writer) {
	graphql.MarshalMap(map[string]any(o)).MarshalGQL(w)
}

func (o *JSONObject) UnmarshalGQL(value any) error {
	decoded, err := graphql.UnmarshalMap(value)
	if err != nil {
		return err
	}
	*o = JSONObject(decoded)
	return nil
}
