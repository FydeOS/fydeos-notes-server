// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/fydeos/fydeos-notes-server/internal/auth"
)

var ErrSecurityChallengeUnavailable = errors.New("security challenge service is unavailable")
var ErrSecurityChallengeInternal = errors.New("security challenge operation failed")

func (r *Resolver) createChangePasswordURL(ctx context.Context, userID, callbackURL string) (string, error) {
	if err := requireAdministrator(ctx); err != nil {
		return "", err
	}
	if r.SecurityChallenges == nil {
		return "", ErrSecurityChallengeUnavailable
	}
	result, err := r.SecurityChallenges.CreateSecurityURL(ctx, RequestFromContext(ctx), userID, callbackURL)
	return result, presentSecurityChallengeError(ctx, err)
}

func (r *Resolver) prepareSecurityChallenge(ctx context.Context, kind, callbackURL string) (bool, error) {
	principal := auth.PrincipalFromContext(ctx)
	if principal == nil {
		return false, ErrAuthenticationRequired
	}
	if r.SecurityChallenges == nil {
		return false, ErrSecurityChallengeUnavailable
	}
	result, err := r.SecurityChallenges.PrepareSecurityChallenge(ctx, RequestFromContext(ctx), kind, principal.User.ID, callbackURL)
	return result, presentSecurityChallengeError(ctx, err)
}

func securityChallengeRequest(ctx context.Context) *http.Request {
	return RequestFromContext(ctx)
}

func presentSecurityChallengeError(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, auth.ErrInvalidEmail) || errors.Is(err, auth.ErrInvalidEmailToken) ||
		errors.Is(err, auth.ErrMagicLinkQuotaDenied) || errors.Is(err, auth.ErrEmailServiceNotConfigured) ||
		errors.Is(err, auth.ErrUnsafeMagicCallback) || errors.Is(err, auth.ErrWrongSignInCredentials) ||
		errors.Is(err, auth.ErrSecurityLinkExpired) || errors.Is(err, auth.ErrSecurityEmailVerificationNeeded) ||
		errors.Is(err, auth.ErrSecurityEmailAlreadyUsed) || errors.Is(err, auth.ErrSecuritySameEmail) ||
		errors.Is(err, auth.ErrSecurityPasswordRequired) {
		return err
	}
	if err != nil {
		slog.ErrorContext(ctx, "security challenge operation failed", "error", err)
		return ErrSecurityChallengeInternal
	}
	return nil
}
