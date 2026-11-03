// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

var (
	ErrNoSession          = errors.New("auth: no session")
	ErrAccessTokenExpired = errors.New("auth: access token expired")
	ErrAccessTokenInvalid = errors.New("auth: access token invalid")
	ErrAuthSessionExpired = errors.New("auth: auth session expired")
	ErrAuthSessionRevoked = errors.New("auth: auth session revoked")

	ErrActionForbidden  = errors.New("auth: action forbidden")
	ErrInvalidAuthState = errors.New("auth: invalid auth state")

	ErrInvalidEmail           = errors.New("auth: invalid email")
	ErrInvalidEmailToken      = errors.New("auth: invalid email token")
	ErrWrongSignInCredentials = errors.New("auth: wrong sign-in credentials")

	ErrEmailServiceNotConfigured = errors.New("auth: email service not configured")
	ErrMagicLinkQuotaDenied      = errors.New("auth: mail quota denied")
	ErrUnsafeMagicCallback       = errors.New("auth: unsafe callback url")

	ErrSecurityLinkExpired             = errors.New("auth: security link expired")
	ErrSecurityEmailVerificationNeeded = errors.New("auth: email verification needed")
	ErrSecurityEmailAlreadyUsed        = errors.New("auth: email already used")
	ErrSecuritySameEmail               = errors.New("auth: same email")
	ErrSecurityPasswordRequired        = errors.New("auth: password required")

	ErrSigningKeyChanged      = errors.New("auth: active signing key changed")
	ErrSigningKeyNotFound     = errors.New("auth: signing key not found")
	ErrSigningKeyCannotDelete = errors.New("auth: signing key cannot be deleted")
)

// Internal sentinels for flows that map to specific HTTP responses.
var (
	errWrongSignInMethod    = errors.New("auth: wrong sign-in method")
	errSignUpForbidden      = errors.New("auth: sign up forbidden")
	errDomainCheckNetwork   = errors.New("auth: domain verification network error")
	errRefreshTokenInvalid  = errors.New("auth: refresh token invalid")
	errRefreshTokenReused   = errors.New("auth: refresh token reused")
	errRefreshRateLimited   = errors.New("auth: refresh rate limited")
	errNativeValidation     = errors.New("auth: invalid request body")
	errBodyTooLarge         = errors.New("auth: body too large")
	errMalformedBody        = errors.New("auth: malformed body")
	errCaptchaCredential    = errors.New("auth: invalid captcha credential")
	errCaptchaRejected      = errors.New("auth: captcha rejected")
	errCaptchaUnavailable   = errors.New("auth: captcha provider unavailable")
	errCaptchaConfigInvalid = errors.New("auth: invalid captcha configuration")
)

func setNoStore(w http.ResponseWriter, pragma bool) {
	w.Header().Set("Cache-Control", "no-store")
	if pragma {
		w.Header().Set("Pragma", "no-cache")
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// apiError is a sign-in style error body.
type apiError struct {
	status     int
	category   string
	name       string
	message    string
	data       any
	retryAfter string
}

func writeAPIError(w http.ResponseWriter, e apiError) {
	if e.retryAfter != "" {
		w.Header().Set("Retry-After", e.retryAfter)
	}
	writeJSON(w, e.status, map[string]any{
		"status":  e.status,
		"code":    http.StatusText(e.status),
		"type":    strings.ToUpper(e.category),
		"name":    strings.ToUpper(e.name),
		"message": e.message,
		"data":    e.data,
	})
}

var (
	apiMethodNotAllowed = apiError{status: 405, category: "bad_request", name: "method_not_allowed", message: "Method not allowed."}
	apiVersionTooLong   = apiError{status: 400, category: "action_forbidden", name: "unsupported_client_version", message: "Unsupported client version."}
	apiTooManyRequests  = apiError{status: 429, category: "too_many_requests", name: "too_many_request", message: "Too many requests.", retryAfter: "60"}
	apiBodyTooLarge     = apiError{status: 413, category: "bad_request", name: "bad_request", message: "Bad request."}
	apiBadSignInBody    = apiError{status: 400, category: "invalid_input", name: "validation_error", message: "Invalid sign-in request."}
	apiWrongMethod      = apiError{status: 400, category: "invalid_input", name: "wrong_sign_in_method", message: "You are trying to sign in by a different method than you signed up with."}
	apiCaptchaHeaders   = apiError{status: 400, category: "bad_request", name: "captcha_verification_failed", message: "Invalid Credential"}
	apiCaptchaRejected  = apiError{status: 400, category: "bad_request", name: "captcha_verification_failed", message: "Invalid Captcha Response"}
	apiCaptchaDown      = apiError{status: 504, category: "network_error", name: "network_error", message: "Captcha verification temporarily unavailable"}
	apiSignUpForbidden  = apiError{status: 403, category: "action_forbidden", name: "sign_up_forbidden", message: "You are not allowed to sign up."}
	apiMailNotConfig    = apiError{status: 500, category: "internal_server_error", name: "email_service_not_configured", message: "Email service is not configured."}
	apiNetworkError     = apiError{status: 504, category: "network_error", name: "network_error", message: "Network error."}
	apiUnsafeCallback   = apiError{status: 403, category: "action_forbidden", name: "action_forbidden", message: "You are not allowed to perform this action."}
	apiInvalidAuthState = apiError{status: 400, category: "bad_request", name: "invalid_auth_state", message: "Invalid auth state. You might start the auth progress from another device."}
	apiInvalidToken     = apiError{status: 400, category: "invalid_input", name: "invalid_email_token", message: "An invalid email token provided."}
	apiTokenNotFound    = apiError{status: 400, category: "invalid_input", name: "email_token_not_found", message: "The email token provided is not found."}
	apiSignInUnavail    = apiError{status: 503, category: "service_unavailable", name: "sign_in_temporarily_unavailable", message: "An internal error occurred."}
	apiInternal         = apiError{status: 500, category: "internal_server_error", name: "internal_server_error", message: "An internal error occurred."}
)

func apiInvalidEmail(email string) apiError {
	return apiError{status: 400, category: "invalid_input", name: "invalid_email", message: "An invalid email provided: " + email, data: map[string]any{"email": email}}
}

func apiInvalidEmailShort(email string) apiError {
	return apiError{status: 400, category: "invalid_input", name: "invalid_email", message: "Invalid email.", data: map[string]any{"email": email}}
}

func apiWrongCredentials(email string) apiError {
	return apiError{status: 400, category: "invalid_input", name: "wrong_sign_in_credentials", message: "Wrong user email or password: " + email, data: map[string]any{"email": email}}
}

func writeTokenError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"status":  status,
		"code":    code,
		"type":    "AUTHENTICATION_REQUIRED",
		"name":    code,
		"message": message,
	})
}

// WriteAccessTokenError writes a 401 for the access-token sentinels and
// reports whether it did.
func WriteAccessTokenError(w http.ResponseWriter, err error) bool {
	var code, message string
	switch {
	case errors.Is(err, ErrAccessTokenExpired):
		code, message = "ACCESS_TOKEN_EXPIRED", "The access token has expired."
	case errors.Is(err, ErrAccessTokenInvalid):
		code, message = "ACCESS_TOKEN_INVALID", "The access token is invalid."
	case errors.Is(err, ErrAuthSessionExpired):
		code, message = "AUTH_SESSION_EXPIRED", "The auth session has expired."
	case errors.Is(err, ErrAuthSessionRevoked):
		code, message = "AUTH_SESSION_REVOKED", "The auth session has been revoked."
	default:
		return false
	}
	setNoStore(w, false)
	writeTokenError(w, http.StatusUnauthorized, code, message)
	return true
}

func writeForbidden(w http.ResponseWriter) {
	writeJSON(w, 403, map[string]any{"status": 403, "code": "FORBIDDEN", "type": "ACTION_FORBIDDEN", "name": "ACTION_FORBIDDEN", "message": "You are not allowed to perform this action."})
}

func writeInvalidAuthState(w http.ResponseWriter) {
	writeJSON(w, 400, map[string]any{"status": 400, "code": "BAD REQUEST", "type": "BAD_REQUEST", "name": "INVALID_AUTH_STATE", "message": "Invalid auth state. You might start the auth progress from another device."})
}

func writeAuthRequired(w http.ResponseWriter) {
	writeJSON(w, 401, map[string]any{"status": 401, "code": "Unauthorized", "type": "AUTHENTICATION_REQUIRED", "name": "AUTHENTICATION_REQUIRED", "message": "You must sign in first to access this resource."})
}

func writeOverloaded(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeJSON(w, 429, map[string]any{"error": "too many requests"})
}

func writeUnavailable(w http.ResponseWriter) {
	writeJSON(w, 503, map[string]any{"error": "authentication service unavailable"})
}

func writeSimpleMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeJSON(w, 405, map[string]any{"error": "method not allowed"})
}
