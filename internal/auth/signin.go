// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"github.com/fydeos/fydeos-notes-server/internal/identity"
	"github.com/fydeos/fydeos-notes-server/internal/password"
	"github.com/jackc/pgx/v5"
)

type PasswordLoginDatabase interface {
	Begin(context.Context) (pgx.Tx, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type PasswordLoginHTTPHandler struct {
	db         PasswordLoginDatabase
	cfg        config.Config
	limiter    PasswordLoginLimiter
	captcha    *CaptchaService
	privateKey string
}

func NewPasswordLoginHTTPHandlerWithDependencies(db PasswordLoginDatabase, cfg config.Config, limiter PasswordLoginLimiter, captcha *CaptchaService) *PasswordLoginHTTPHandler {
	if limiter == nil {
		limiter = newMemoryLimiter()
	}
	return &PasswordLoginHTTPHandler{db: db, cfg: cfg, limiter: limiter, captcha: captcha, privateKey: os.Getenv("AFFINE_PRIVATE_KEY")}
}

var signInFields = map[string]bool{"email": true, "password": true, "callbackUrl": true, "client_nonce": true, "verifyToken": true, "challenge": true}

// signInPrelude runs the shared method, body, version and strict IP checks.
func signInPrelude(w http.ResponseWriter, r *http.Request, cfg config.Config, limiter PasswordLoginLimiter) ([]byte, bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, apiMethodNotAllowed)
		return nil, false
	}
	body, err := readBody(w, r, signInBodyLimit)
	if errors.Is(err, errBodyTooLarge) {
		writeAPIError(w, apiBodyTooLarge)
		return nil, false
	}
	if err != nil {
		writeAPIError(w, apiBadSignInBody)
		return nil, false
	}
	if utf16Len(r.Header.Get("x-affine-version")) > 128 {
		writeAPIError(w, apiVersionTooLong)
		return nil, false
	}
	if !limiter.AllowStrict(r.Context(), requestClientIP(r, cfg), time.Now()) {
		writeAPIError(w, apiTooManyRequests)
		return nil, false
	}
	return body, true
}

func (h *PasswordLoginHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setNoStore(w, false)
	body, ok := signInPrelude(w, r, h.cfg, h.limiter)
	if !ok {
		return
	}
	ctx := r.Context()
	if h.captcha != nil {
		if err := h.captcha.Verify(ctx, r); err != nil {
			writeAPIError(w, captchaAPIError(err))
			return
		}
	}
	if !isJSONContentType(r) {
		writeAPIError(w, apiBadSignInBody)
		return
	}
	fields, err := parseStringObject(body, signInFields, nil)
	if err != nil {
		writeAPIError(w, apiBadSignInBody)
		return
	}
	rawEmail, hasEmail := fields["email"]
	email := strings.TrimSpace(rawEmail)
	if !hasEmail || !validSignInEmail(email) {
		writeAPIError(w, apiInvalidEmail(email))
		return
	}
	if !optionalLength(fields, "password", 1, 1024) || !optionalLength(fields, "callbackUrl", 1, 2048) ||
		!optionalLength(fields, "client_nonce", 1, 512) || !optionalLength(fields, "verifyToken", 0, 4096) || !optionalLength(fields, "challenge", 0, 4096) {
		writeAPIError(w, apiBadSignInBody)
		return
	}
	policy, err := loadSignInPolicy(ctx, h.db, h.cfg)
	if err != nil {
		writeAPIError(w, apiSignInUnavail)
		return
	}
	if !h.limiter.Allow(ctx, strings.ToLower(email), requestClientIP(r, h.cfg), policy, time.Now()) {
		writeAPIError(w, apiTooManyRequests)
		return
	}
	plain, hasPassword := fields["password"]
	if !hasPassword {
		var nonce *string
		if v, ok := fields["client_nonce"]; ok {
			nonce = &v
		}
		if apiErr := prepareMagicLink(ctx, h.db, h.cfg, r, email, fields["callbackUrl"], nonce, h.privateKey); apiErr != nil {
			writeAPIError(w, *apiErr)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"email": email})
		return
	}
	h.passwordLogin(w, r, email, plain)
}

type lockedUser struct {
	ID            string
	Name          string
	Email         string
	AvatarURL     *string
	Password      *string
	EmailVerified bool
	Disabled      bool
	AuthEpoch     int
}

func lockUsersByEmail(ctx context.Context, tx pgx.Tx, email string) ([]lockedUser, error) {
	rows, err := tx.Query(ctx, `SELECT id, name, email, avatar_url, password, email_verified IS NOT NULL, disabled, auth_epoch
		FROM users WHERE lower(email)=lower($1) ORDER BY id FOR UPDATE`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lockedUser
	for rows.Next() {
		var u lockedUser
		if err := rows.Scan(&u.ID, &u.Name, &u.Email, &u.AvatarURL, &u.Password, &u.EmailVerified, &u.Disabled, &u.AuthEpoch); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (h *PasswordLoginHTTPHandler) passwordLogin(w http.ResponseWriter, r *http.Request, email, plain string) {
	ctx := r.Context()
	tx, err := h.db.Begin(ctx)
	if err != nil {
		writeAPIError(w, apiInternal)
		return
	}
	defer rollback(tx)
	users, err := lockUsersByEmail(ctx, tx, email)
	if err != nil {
		writeAPIError(w, apiInternal)
		return
	}
	if len(users) != 1 || users[0].Disabled {
		writeAPIError(w, apiWrongCredentials(email))
		return
	}
	u := users[0]
	if u.Password == nil {
		writeAPIError(w, apiWrongMethod)
		return
	}
	if !password.Verify(*u.Password, plain) {
		writeAPIError(w, apiWrongCredentials(email))
		return
	}
	issued, err := issueSession(ctx, tx, r, h.cfg, u.ID, u.AuthEpoch)
	if err != nil || tx.Commit(ctx) != nil {
		writeAPIError(w, apiInternal)
		return
	}
	body := map[string]any{"id": u.ID, "email": u.Email, "avatarUrl": u.AvatarURL, "name": u.Name, "disabled": u.Disabled, "hasPassword": true, "emailVerified": u.EmailVerified}
	issued.apply(w, h.cfg, u.ID, body)
	writeJSON(w, http.StatusOK, body)
}

type magicLinkPayload struct {
	OTPHash     *string `json:"otpHash"`
	ClientNonce *string `json:"clientNonce"`
	UserID      *string `json:"userId"`
	AuthEpoch   *int    `json:"authEpoch"`
}

func randomOTP() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	return fmt.Sprintf("%06d", n.Int64())
}

func prepareMagicLink(ctx context.Context, db PasswordLoginDatabase, cfg config.Config, r *http.Request, email, callback string, nonce *string, bootstrapKey string) *apiError {
	fail := func(e apiError) *apiError { return &e }
	configured, err := mailConfigured(ctx, db, cfg)
	if err != nil {
		return fail(apiInternal)
	}
	if !configured {
		return fail(apiMailNotConfig)
	}
	if !strictEmail(email) {
		return fail(apiInvalidEmailShort(email))
	}
	callbackURL, err := resolveCallbackURL(r, cfg, callback, "/magic-link")
	if err != nil {
		return fail(apiUnsafeCallback)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fail(apiInternal)
	}
	defer rollback(tx)
	if err := identity.LockEmail(ctx, tx, email); err != nil {
		return fail(apiInternal)
	}
	users, err := lockUsersByEmail(ctx, tx, email)
	if err != nil {
		return fail(apiInternal)
	}
	payload := magicLinkPayload{ClientNonce: nonce}
	mailName := "SignUp"
	var userID *string
	switch {
	case len(users) > 1:
		return fail(apiWrongCredentials(email))
	case len(users) == 1:
		if users[0].Disabled {
			return fail(apiWrongCredentials(email))
		}
		payload.UserID, payload.AuthEpoch = &users[0].ID, &users[0].AuthEpoch
		userID = &users[0].ID
		mailName = "SignIn"
	default:
		allowed, err := signUpAllowed(ctx, tx, cfg, email, "auth.allowSignup")
		if errors.Is(err, errDomainCheckNetwork) {
			return fail(apiNetworkError)
		}
		if err != nil {
			return fail(apiInternal)
		}
		if !allowed {
			return fail(apiSignUpForbidden)
		}
	}
	privateKey, err := LoadAppPrivateKey(ctx, tx, cfg, bootstrapKey)
	if err != nil || strings.TrimSpace(privateKey) == "" {
		return fail(apiInternal)
	}
	otp := randomOTP()
	otpHash := sha256hex(otp)
	payload.OTPHash = &otpHash
	now, err := dbNow(ctx, tx)
	if err != nil {
		return fail(apiInternal)
	}
	expires := now.Add(30 * time.Minute)
	lower := strings.ToLower(email)
	if err := upsertMagicState(ctx, tx, lower, payload, expires); err != nil {
		return fail(apiInternal)
	}
	trusted, err := readConfigBool(ctx, tx, cfg, "auth.trustedCloudflareHeaders", false)
	if err != nil {
		return fail(apiInternal)
	}
	prefix := ""
	if trusted {
		prefix = sourcePrefix(requestClientIP(r, cfg))
	}
	err = queueMail(ctx, tx, mailRequest{
		name: mailName, to: email, userID: userID,
		props:     map[string]any{"url": appendQuery(callbackURL, [2]string{"token", otp}, [2]string{"email", email}), "otp": otp, "serverName": serverName(ctx, tx, cfg)},
		dedupeKey: "auth:magic-link:" + sha256hex(lower) + ":" + otpHash,
		expires:   expires, sourcePrefix: prefix, privateKey: privateKey,
	})
	if errors.Is(err, ErrMagicLinkQuotaDenied) {
		return fail(apiTooManyRequests)
	}
	if err != nil || tx.Commit(ctx) != nil {
		return fail(apiInternal)
	}
	return nil
}

func upsertMagicState(ctx context.Context, tx pgx.Tx, lowerEmail string, payload magicLinkPayload, expires time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO runtime_states(purpose,token_hash,lookup_key,payload,attempts,consumed_at,expires_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,0,NULL,$5,clock_timestamp(),clock_timestamp())
		ON CONFLICT (purpose,token_hash) DO UPDATE SET lookup_key=EXCLUDED.lookup_key, payload=EXCLUDED.payload, attempts=0,
		  consumed_at=NULL, expires_at=EXCLUDED.expires_at, updated_at=EXCLUDED.updated_at`,
		purposeMagicLink, sha256hex(lowerEmail), lowerEmail, payload, expires)
	return err
}
