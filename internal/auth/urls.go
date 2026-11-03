// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"golang.org/x/net/idna"
)

var trustedRedirectHosts = []string{"google.com", "stripe.com", "github.com", "twitter.com", "discord.gg", "youtube.com", "t.me", "reddit.com"}

func schemeOf(cfg config.Config) string {
	if cfg.HTTPS {
		return "https"
	}
	return "http"
}

func configuredHosts(cfg config.Config) []string {
	hosts := []string{}
	if cfg.Host != "" {
		hosts = append(hosts, cfg.Host)
	}
	return append(hosts, cfg.Hosts...)
}

func requestOrigin(r *http.Request, cfg config.Config) string {
	if r != nil {
		for _, h := range configuredHosts(cfg) {
			if strings.EqualFold(h, r.Host) {
				return schemeOf(cfg) + "://" + h
			}
		}
	}
	return config.PublicOrigin(cfg)
}

// canonicalOrigin returns scheme://host[:port] normalised, or "".
func canonicalOrigin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host, err := idna.Lookup.ToASCII(strings.ToLower(u.Hostname()))
	if err != nil || host == "" {
		return ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}

func allowedOrigins(r *http.Request, cfg config.Config) map[string]bool {
	allowed := map[string]bool{}
	add := func(raw string) {
		if u, err := url.Parse(raw); err == nil {
			if o := canonicalOrigin(u); o != "" {
				allowed[o] = true
			}
		}
	}
	add(config.PublicOrigin(cfg))
	add(requestOrigin(r, cfg))
	for _, h := range configuredHosts(cfg) {
		add(schemeOf(cfg) + "://" + h)
	}
	return allowed
}

func safeAbsoluteURL(base *url.URL, raw string) (*url.URL, bool) {
	if strings.Contains(raw, "\\") {
		return nil, false
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return nil, false
	}
	u := base.ResolveReference(ref)
	if u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, false
	}
	return u, true
}

// resolveCallbackURL validates a callback against the allowed origins and
// returns its canonical absolute form.
func resolveCallbackURL(r *http.Request, cfg config.Config, raw, fallback string) (string, error) {
	if raw == "" {
		raw = fallback
	}
	base, err := url.Parse(requestOrigin(r, cfg) + "/")
	if err != nil {
		return "", ErrUnsafeMagicCallback
	}
	u, ok := safeAbsoluteURL(base, raw)
	if !ok {
		return "", ErrUnsafeMagicCallback
	}
	allowed := allowedOrigins(r, cfg)
	if !allowed[canonicalOrigin(u)] {
		return "", ErrUnsafeMagicCallback
	}
	if redirect := u.Query().Get("redirect_uri"); u.Query().Has("redirect_uri") {
		target, ok := safeAbsoluteURL(u, redirect)
		if !ok {
			return "", ErrUnsafeMagicCallback
		}
		if !allowed[canonicalOrigin(target)] && !trustedRedirectHost(target.Hostname()) {
			return "", ErrUnsafeMagicCallback
		}
	}
	return u.String(), nil
}

func trustedRedirectHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, t := range trustedRedirectHosts {
		if host == t || strings.HasSuffix(host, "."+t) {
			return true
		}
	}
	return false
}

// formEncode encodes like application/x-www-form-urlencoded.
func formEncode(s string) string {
	var b strings.Builder
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '*', c == '-', c == '.', c == '_':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

// appendQuery keeps existing parameters in order and appends pairs.
func appendQuery(raw string, pairs ...[2]string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	var parts []string
	if u.RawQuery != "" {
		for _, part := range strings.Split(u.RawQuery, "&") {
			if part == "" {
				continue
			}
			k, v, _ := strings.Cut(part, "=")
			dk, err1 := url.QueryUnescape(k)
			dv, err2 := url.QueryUnescape(v)
			if err1 != nil || err2 != nil {
				parts = append(parts, part)
				continue
			}
			parts = append(parts, formEncode(dk)+"="+formEncode(dv))
		}
	}
	for _, p := range pairs {
		parts = append(parts, formEncode(p[0])+"="+formEncode(p[1]))
	}
	u.RawQuery = strings.Join(parts, "&")
	u.ForceQuery = false
	return u.String()
}

// validSignInEmail applies the RFC 5322 rule used by sign-in.
func validSignInEmail(email string) bool {
	n := utf16Len(email)
	if n < 1 || n > 320 || !strings.Contains(email, "@") {
		return false
	}
	addr, err := mail.ParseAddress(email)
	return err == nil && addr.Address == email
}

var (
	localPartPattern = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)
	domainLabel      = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

// strictEmail applies the magic-link syntax rule.
func strictEmail(email string) bool {
	at := strings.LastIndex(email, "@")
	if at <= 0 || strings.Count(email, "@") != 1 {
		return false
	}
	local, domain := email[:at], strings.ToLower(email[at+1:])
	if !localPartPattern.MatchString(local) || strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, l := range labels {
		if !domainLabel.MatchString(l) {
			return false
		}
	}
	return true
}

type dnsLookup interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

var domainResolver dnsLookup = net.DefaultResolver

func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// emailDomainVerified checks MX, SPF and DMARC records for the address.
func emailDomainVerified(ctx context.Context, email string) (bool, error) {
	at := strings.LastIndex(email, "@")
	if at < 0 || strings.Contains(email[:at], "+") {
		return false, nil
	}
	domain := strings.ToLower(email[at+1:])
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	mx, err := domainResolver.LookupMX(ctx, domain)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, errDomainCheckNetwork
	}
	if len(mx) == 0 {
		return false, nil
	}
	hasPrefix := func(name, prefix string) (bool, error) {
		records, err := domainResolver.LookupTXT(ctx, name)
		if err != nil {
			if isNotFound(err) {
				return false, nil
			}
			return false, errDomainCheckNetwork
		}
		for _, r := range records {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(r)), strings.ToLower(prefix)) {
				return true, nil
			}
		}
		return false, nil
	}
	if ok, err := hasPrefix(domain, "v=spf1"); !ok || err != nil {
		return false, err
	}
	return hasPrefix("_dmarc."+domain, "v=DMARC1")
}

// signUpAllowed applies the sign-up policy for a new address.
func signUpAllowed(ctx context.Context, db RowQuerier, cfg config.Config, email, key string) (bool, error) {
	allow, err := readConfigBool(ctx, db, cfg, key, true)
	if err != nil || !allow {
		return false, err
	}
	return domainAllowed(ctx, db, cfg, email)
}

func domainAllowed(ctx context.Context, db RowQuerier, cfg config.Config, email string) (bool, error) {
	required, err := readConfigBool(ctx, db, cfg, "auth.requireEmailDomainVerification", false)
	if err != nil || !required {
		return err == nil, err
	}
	return emailDomainVerified(ctx, email)
}
