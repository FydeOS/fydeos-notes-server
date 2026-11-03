// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	SessionCookieName      = "affine_session"
	UserCookieName         = "affine_user_id"
	CSRFCookieName         = "affine_csrf_token"
	DefaultReadConcurrency = 16
	NativeExchangeCodeTTL  = time.Minute
)

type User struct {
	ID            string
	Name          string
	Email         string
	AvatarURL     *string
	EmailVerified bool
	HasPassword   bool
	Disabled      bool
	Features      []string
}

type Principal struct {
	SessionID        string
	UserSessionID    string
	AuthSessionID    string
	AuthenticatedAt  *time.Time
	SessionExpiresAt *time.Time
	User             User
}

type RowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type RequestAuthenticator interface {
	FromRequest(context.Context, *http.Request) (*Principal, error)
}

type responseAuthenticator interface {
	FromRequestWithResponse(context.Context, http.ResponseWriter, *http.Request) (*Principal, error)
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, principal *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

func PrincipalFromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}

// AuthenticateRequest prefers the response-aware resolver so that cookie
// sessions can be refreshed.
func AuthenticateRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, authenticator RequestAuthenticator) (*Principal, error) {
	if ra, ok := authenticator.(responseAuthenticator); ok {
		return ra.FromRequestWithResponse(ctx, w, r)
	}
	return authenticator.FromRequest(ctx, r)
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func clearedCookie(name string, httpOnly bool) *http.Cookie {
	return &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0), HttpOnly: httpOnly, SameSite: http.SameSiteLaxMode}
}

func ClearSessionCookies(w http.ResponseWriter) {
	http.SetCookie(w, clearedCookie(SessionCookieName, true))
	http.SetCookie(w, clearedCookie(UserCookieName, false))
	http.SetCookie(w, clearedCookie(CSRFCookieName, false))
}

func setSessionCookie(w http.ResponseWriter, sessionID string, expires time.Time, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookieName, Value: sessionID, Path: "/", Expires: expires, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
}

func setCSRFCookie(w http.ResponseWriter, expires time.Time, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: CSRFCookieName, Value: uuid.NewString(), Path: "/", Expires: expires, Secure: secure, SameSite: http.SameSiteLaxMode})
}

func setUserCookie(w http.ResponseWriter, userID string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: UserCookieName, Value: userID, Path: "/", Expires: expires, SameSite: http.SameSiteLaxMode})
}

func setSignInCookies(w http.ResponseWriter, sessionID, userID string, expires time.Time, secure bool) {
	setSessionCookie(w, sessionID, expires, secure)
	setUserCookie(w, userID, expires)
	setCSRFCookie(w, expires, secure)
}

func hasSessionCookie(r *http.Request) bool {
	_, err := r.Cookie(SessionCookieName)
	return err == nil
}

// checkCSRF requires the CSRF header to match the CSRF cookie.
func checkCSRF(r *http.Request) bool {
	header := r.Header.Get("x-affine-csrf-token")
	cookie, err := r.Cookie(CSRFCookieName)
	if header == "" || err != nil || cookie.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header), []byte(cookie.Value)) == 1
}

// IsNativeClientRequest reports whether the request comes from a native app.
func IsNativeClientRequest(r *http.Request) bool {
	if r.Header.Get("x-affine-client-kind") != "native" {
		return false
	}
	switch r.Header.Get("Origin") {
	case "", "capacitor://localhost", "ionic://localhost", "https://localhost":
		return true
	}
	return false
}

// ReadLimiter is a non-blocking concurrency cap.
type ReadLimiter struct {
	slots chan struct{}
}

func NewReadLimiter(limit int) *ReadLimiter {
	if limit < 1 {
		limit = DefaultReadConcurrency
	}
	return &ReadLimiter{slots: make(chan struct{}, limit)}
}

func (l *ReadLimiter) TryAcquire() (func(), bool) {
	if l == nil {
		return func() {}, true
	}
	select {
	case l.slots <- struct{}{}:
		return func() { <-l.slots }, true
	default:
		return nil, false
	}
}

func pickReadLimiter(limiters []*ReadLimiter) *ReadLimiter {
	for _, l := range limiters {
		if l != nil {
			return l
		}
	}
	return NewReadLimiter(DefaultReadConcurrency)
}
