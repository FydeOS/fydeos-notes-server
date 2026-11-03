// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package oauth

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/net/idna"

	"github.com/fydeos/fydeos-notes-server/internal/clientip"
	"github.com/fydeos/fydeos-notes-server/internal/config"
)

const (
	versionHeader       = "x-affine-version"
	maxVersionHeaderLen = 128
	bodyReadTimeout     = 10 * time.Second
)

var errBodyTooLarge = errors.New("request body too large")

// applyRateLimit sets rate-limit headers and reports whether to continue.
func applyRateLimit(w http.ResponseWriter, r *http.Request, cfg config.Config, limiter RateLimiter) bool {
	now := time.Now()
	key := clientip.RequestClientIP(r, cfg.TrustedProxyCIDRs, cfg.TrustedCloudflareCIDRs, cfg.TrustedXRealIPCIDRs)
	allowed, remaining, reset := limiter.Allow(r.Context(), key, now)
	seconds := int(math.Ceil(reset.Sub(now).Seconds()))
	if seconds < 0 {
		seconds = 0
	}
	h := w.Header()
	h.Set("X-RateLimit-Limit", strconv.Itoa(rateLimit))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	h.Set("X-RateLimit-Reset", strconv.Itoa(seconds))
	if !allowed {
		h.Set("Retry-After", strconv.Itoa(max(1, seconds)))
	}
	return allowed
}

// readBody reads the request body with a deadline and a hard cap.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	w.Header().Set("Connection", "close")
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyReadTimeout))
	if r.ContentLength > limit {
		return nil, errBodyTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errBodyTooLarge
		}
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errBodyTooLarge
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	w.Header().Del("Connection")
	return body, nil
}

// decodeStrictJSON decodes exactly one object, rejecting unknown fields.
func decodeStrictJSON(body []byte, dst any) error {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

func isObjectJSON(body []byte) bool {
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	return strings.HasPrefix(trimmed, "{")
}

func mediaTypeIs(r *http.Request, want string) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), want)
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// canonicalOrigin returns scheme://ascii-lowercase-host[:non-default-port].
func canonicalOrigin(scheme, hostport string) (string, string, bool) {
	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		return "", "", false
	}
	host, port := hostport, ""
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		host, port = h, p
	} else {
		host = strings.Trim(host, "[]")
	}
	if host == "" {
		return "", "", false
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		ascii = strings.ToLower(host)
	}
	ascii = strings.ToLower(strings.TrimSuffix(ascii, "."))
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	h := ascii
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	if port != "" {
		h += ":" + port
	}
	return scheme + "://" + h, ascii, true
}

// requestOrigin picks the configured host matching the request Host, falling
// back to the public origin.
func requestOrigin(r *http.Request, cfg config.Config) string {
	scheme := "http"
	if cfg.HTTPS {
		scheme = "https"
	}
	reqHost, reqPort := r.Host, ""
	if h, p, err := net.SplitHostPort(r.Host); err == nil {
		reqHost, reqPort = h, p
	}
	for _, configured := range cfg.Hosts {
		cHost, cPort := configured, ""
		if h, p, err := net.SplitHostPort(configured); err == nil {
			cHost, cPort = h, p
		}
		if !strings.EqualFold(strings.Trim(cHost, "[]"), strings.Trim(reqHost, "[]")) {
			continue
		}
		if cPort != "" && cPort != reqPort {
			continue
		}
		if origin, _, ok := canonicalOrigin(scheme, configured); ok {
			return origin
		}
	}
	return config.PublicOrigin(cfg)
}

// allowedRedirectOrigins lists the public origin plus each configured host.
func allowedRedirectOrigins(cfg config.Config) []string {
	scheme := "http"
	if cfg.HTTPS {
		scheme = "https"
	}
	origins := []string{config.PublicOrigin(cfg)}
	for _, host := range cfg.Hosts {
		if origin, _, ok := canonicalOrigin(scheme, host); ok {
			origins = append(origins, origin)
		}
	}
	return origins
}

var trustedRedirectDomains = []string{"google.com", "stripe.com", "github.com", "twitter.com", "discord.gg", "youtube.com", "t.me", "reddit.com"}

// resolveRedirect validates a redirect URI and returns its absolute form.
func resolveRedirect(raw, base string, allowedOrigins []string) (string, error) {
	if n := utf16Len(raw); n < 1 || n > 2048 {
		return "", ErrInvalidInput
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", ErrInvalidInput
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return "", ErrInvalidInput
	}
	abs := baseURL.ResolveReference(ref)
	if (abs.Scheme != "http" && abs.Scheme != "https") || abs.Host == "" || abs.User != nil {
		return "", ErrRedirectNotAllowed
	}
	origin, host, ok := canonicalOrigin(abs.Scheme, abs.Host)
	if !ok {
		return "", ErrRedirectNotAllowed
	}
	for _, allowed := range allowedOrigins {
		if u, err := url.Parse(allowed); err == nil {
			if canon, _, ok := canonicalOrigin(u.Scheme, u.Host); ok && canon == origin {
				return abs.String(), nil
			}
		}
	}
	for _, domain := range trustedRedirectDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return abs.String(), nil
		}
	}
	return "", ErrRedirectNotAllowed
}
