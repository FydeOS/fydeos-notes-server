// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

// Package oauth implements third-party sign-in (Google, GitHub, Apple,
// generic OIDC and the FydeOS account server).
package oauth

import "errors"

var (
	ErrInvalidInput              = errors.New("invalid oauth input")
	ErrUnknownProvider           = errors.New("unknown oauth provider")
	ErrProviderDisabled          = errors.New("oauth provider disabled")
	ErrRedirectNotAllowed        = errors.New("oauth redirect uri not allowed")
	ErrInvalidCallback           = errors.New("invalid oauth callback")
	ErrInvalidCallbackState      = errors.New("invalid oauth callback state")
	ErrOAuthStateExpired         = errors.New("oauth state expired")
	ErrInvalidAuthState          = errors.New("invalid auth state")
	ErrInvalidOAuthCallbackCode  = errors.New("invalid oauth callback code")
	ErrInvalidOAuthResponse      = errors.New("invalid oauth response")
	ErrOAuthSignupForbidden      = errors.New("oauth sign up forbidden")
	ErrOAuthAccountAlreadyLinked = errors.New("oauth account already linked")
	ErrWrongSignInCredentials    = errors.New("account cannot sign in")
	ErrProviderUnavailable       = errors.New("oauth provider unavailable")
)

// providerStatusError carries the HTTP status of a failed provider call.
type providerStatusError struct {
	status int
}

func (e *providerStatusError) Error() string { return "oauth provider returned non-2xx status" }
func (e *providerStatusError) Unwrap() error { return ErrInvalidOAuthCallbackCode }
