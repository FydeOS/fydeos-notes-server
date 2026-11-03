// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/fydeos/fydeos-notes-server/internal/config"
)

type LoginProviderCatalog interface {
	AvailableProviders(context.Context) ([]string, error)
}

type LoginPreflightHTTPHandler struct {
	db        config.RuntimeConfigReader
	cfg       config.Config
	providers LoginProviderCatalog
	limiter   PasswordLoginLimiter
	reads     *ReadLimiter
}

func NewLoginPreflightHTTPHandler(db config.RuntimeConfigReader, cfg config.Config, providers LoginProviderCatalog, limiter PasswordLoginLimiter, reads *ReadLimiter) *LoginPreflightHTTPHandler {
	if limiter == nil {
		limiter = newMemoryLimiter()
	}
	if reads == nil {
		reads = NewReadLimiter(DefaultReadConcurrency)
	}
	return &LoginPreflightHTTPHandler{db: db, cfg: cfg, providers: providers, limiter: limiter, reads: reads}
}

var preflightEmailRe = regexp.MustCompile(`^[^\s@.][^\s@]*@[^\s@]+\.[A-Za-z]{2,}$`)

func (h *LoginPreflightHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setNoStore(w, false)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, apiMethodNotAllowed)
		return
	}
	if !h.limiter.AllowStrict(r.Context(), requestClientIP(r, h.cfg), time.Now()) {
		writeAPIError(w, apiTooManyRequests)
		return
	}
	release, ok := h.reads.TryAcquire()
	if !ok {
		writeOverloaded(w)
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	version := r.Header.Get("x-affine-version")
	if utf16Len(version) > 128 {
		writeAPIError(w, apiVersionTooLong)
		return
	}
	if ok, required, err := checkClientVersion(ctx, h.db, h.cfg, version, time.Now()); err != nil {
		writeAPIError(w, apiInternal)
		return
	} else if !ok {
		reported := version
		if reported == "" {
			reported = "unset_or_invalid"
		}
		writeAPIError(w, apiError{status: 403, category: "action_forbidden", name: "unsupported_client_version",
			message: fmt.Sprintf("Unsupported client with version [%s], required version is [%s].", reported, required),
			data:    map[string]any{"clientVersion": reported, "requiredVersion": required}})
		return
	}
	body, err := readBody(w, r, signInBodyLimit)
	if errors.Is(err, errBodyTooLarge) {
		writeAPIError(w, apiBodyTooLarge)
		return
	}
	var fields map[string]string
	if err == nil {
		fields, err = parseStringObject(body, map[string]bool{"email": true}, nil)
	}
	email, has := fields["email"]
	if err != nil || !has || len(fields) != 1 || utf16Len(email) > 320 {
		writeAPIError(w, apiInvalidEmail("not provided"))
		return
	}
	if !preflightEmailRe.MatchString(email) || strings.Contains(email, "..") {
		writeAPIError(w, apiInvalidEmail(email))
		return
	}
	providers := []string{}
	if h.providers != nil {
		list, err := h.providers.AvailableProviders(ctx)
		if err != nil {
			writeAPIError(w, apiInternal)
			return
		}
		if list != nil {
			providers = list
		}
	}
	var count int
	var uRegistered, uPassword, uDisabled *bool
	if err := h.db.QueryRow(ctx, `SELECT count(*)::int, bool_or(registered), bool_or(has_password), bool_or(disabled)
		FROM (SELECT registered, password IS NOT NULL AS has_password, disabled FROM users WHERE lower(email)=lower($1) LIMIT 2) matches`, email).
		Scan(&count, &uRegistered, &uPassword, &uDisabled); err != nil {
		writeAPIError(w, apiInternal)
		return
	}
	registered, pw, magic, oauth := false, false, false, false
	switch {
	case count == 1 && !*uDisabled:
		registered, pw, magic, oauth = *uRegistered, *uPassword, true, len(providers) > 0
	case count == 0:
		signup, err1 := readConfigBool(ctx, h.db, h.cfg, "auth.allowSignup", true)
		signupOAuth, err2 := readConfigBool(ctx, h.db, h.cfg, "auth.allowSignupForOauth", true)
		if err := errors.Join(err1, err2); err != nil {
			writeAPIError(w, apiInternal)
			return
		}
		domainOK := false
		if signup || (signupOAuth && len(providers) > 0) {
			domainOK, err = domainAllowed(ctx, h.db, h.cfg, email)
			if errors.Is(err, errDomainCheckNetwork) {
				writeAPIError(w, apiNetworkError)
				return
			}
			if err != nil {
				writeAPIError(w, apiInternal)
				return
			}
		}
		magic = signup && domainOK
		oauth = signupOAuth && len(providers) > 0
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"registered": registered,
		"methods": map[string]any{
			"password":  map[string]any{"available": pw},
			"magicLink": map[string]any{"available": magic},
			"oauth":     map[string]any{"available": oauth, "providers": providers},
			"passkey":   map[string]any{"available": false, "discoverable": false},
		},
	})
}

var canaryVersionRe = regexp.MustCompile(`^(\d{4})\.(\d{1,2})\.(\d{1,2})-canary\.\d+$`)

func readConfigString(ctx context.Context, db RowQuerier, cfg config.Config, key, def string) (string, error) {
	raw, found, err := config.ReadRuntimeValue(ctx, db, cfg.RuntimeConfigPath, key)
	if err != nil || !found {
		return def, err
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("invalid %s", key)
	}
	return v, nil
}

// checkClientVersion applies the optional client version gate.
func checkClientVersion(ctx context.Context, db RowQuerier, cfg config.Config, version string, now time.Time) (bool, string, error) {
	enabled, err := readConfigBool(ctx, db, cfg, "client.versionControl.enabled", false)
	if err != nil || !enabled {
		return err == nil, "", err
	}
	required, err := readConfigString(ctx, db, cfg, "client.versionControl.requiredVersion", ">=0.25.0")
	if err != nil {
		return false, "", err
	}
	if cfg.Namespace == "canary" {
		if canaryDateOK(version, now) {
			return true, "", nil
		}
		return false, "canary (within 2 months)", nil
	}
	if version == "" {
		return false, required, nil
	}
	v, err := semver.StrictNewVersion(strings.TrimPrefix(version, "v"))
	if err != nil {
		return false, required, nil
	}
	floor, _ := semver.NewConstraint(">=0.25.0")
	floor.IncludePrerelease = true
	if !floor.Check(v) {
		return false, required, nil
	}
	if c, err := semver.NewConstraint(required); err == nil {
		c.IncludePrerelease = true
		if !c.Check(v) {
			return false, required, nil
		}
	}
	return true, "", nil
}

func canaryDateOK(version string, now time.Time) bool {
	m := canaryVersionRe.FindStringSubmatch(strings.TrimPrefix(version, "v"))
	if m == nil {
		return false
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	d, _ := strconv.Atoi(m[3])
	date := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if date.Year() != y || int(date.Month()) != mo || date.Day() != d {
		return false
	}
	return !date.After(now.Add(48*time.Hour)) && !date.Before(now.Add(-62*24*time.Hour))
}
