// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package model

import (
	"io"

	"github.com/99designs/gqlgen/graphql"
)

type JSON struct {
	Value any
}

func (v JSON) MarshalGQL(w io.Writer) {
	graphql.MarshalAny(v.Value).MarshalGQL(w)
}

func (v *JSON) UnmarshalGQL(value any) error {
	decoded, err := graphql.UnmarshalAny(value)
	if err != nil {
		return err
	}
	v.Value = decoded
	return nil
}
