// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package invitelinks

import "context"

type requestFactsContextKey struct{}

type RequestFacts struct {
	RequestID string
	ClientIP  string
	ASN       string
}

func WithRequestFacts(ctx context.Context, facts RequestFacts) context.Context {
	return context.WithValue(ctx, requestFactsContextKey{}, facts)
}

func requestFactsFromContext(ctx context.Context) RequestFacts {
	facts, _ := ctx.Value(requestFactsContextKey{}).(RequestFacts)
	return facts
}
