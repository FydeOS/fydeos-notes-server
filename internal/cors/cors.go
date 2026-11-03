// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

// Package cors implements the browser origin contract shared by HTTP and Engine.IO.
// CORS controls browser access to responses; it does not replace authentication or CSRF checks.
package cors

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/fydeos/fydeos-notes-server/internal/config"
	"golang.org/x/net/idna"
)

const allowedMethods = "GET,HEAD,PUT,PATCH,POST,DELETE,OPTIONS"
const allowedHeaders = "accept,authorization,content-type,x-affine-version,x-affine-client-kind,x-operation-name,x-request-id,x-captcha-token,x-captcha-challenge,x-captcha-provider,x-affine-csrf-token,x-requested-with,range"

// HTTP includes the exposed headers and preflight lifetime used by the REST/GraphQL server.
func HTTP(cfg config.Config, next http.Handler) http.Handler { return wrap(cfg, next, false) }

// Realtime deliberately omits HTTP-only exposed headers and the preflight lifetime.
func Realtime(cfg config.Config, next http.Handler) http.Handler { return wrap(cfg, next, true) }

func wrap(cfg config.Config, next http.Handler, realtime bool) http.Handler {
	allowed := map[string]bool{
		"https://localhost": true, "capacitor://localhost": true, "ionic://localhost": true,
		"assets://.": true, "assets://another-host": true, "file://": true,
	}
	if origin := canonicalOrigin(config.PublicOrigin(cfg)); origin != "" {
		allowed[origin] = true
	}
	scheme := "http://"
	if cfg.HTTPS {
		scheme = "https://"
	}
	for _, host := range cfg.Hosts {
		if origin := canonicalOrigin(scheme + host); origin != "" {
			allowed[origin] = true
		}
	}
	development := cfg.NodeEnv == "development" || cfg.NodeEnv == "test"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Node joins repeated Origin fields before evaluating the legacy policy.
		origin := strings.Join(r.Header.Values("Origin"), ", ")
		permit := origin == "" || allowed[origin]
		if !permit {
			normalized := canonicalOrigin(origin)
			permit = normalized != "" && allowed[normalized]
		}
		if !permit && development {
			parsed, err := url.Parse(origin)
			if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
				host := strings.ToLower(parsed.Hostname())
				permit = host == "localhost" || host == "127.0.0.1" || host == "::1"
			}
		}
		if !permit {
			next.ServeHTTP(w, r)
			return
		}
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		varyOrigin(w.Header())
		if !realtime {
			w.Header().Set("Access-Control-Expose-Headers", "content-length,content-range,x-request-id")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", allowedMethods)
			w.Header().Set("Access-Control-Allow-Headers", allowedHeaders)
			if !realtime {
				w.Header().Set("Access-Control-Max-Age", "86400")
			}
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func canonicalOrigin(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	case "http", "https":
	default:
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else {
		host, err = idna.Lookup.ToASCII(host)
		if err != nil {
			return ""
		}
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return ""
		}
		port = strconv.FormatUint(n, 10)
		if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
			port = ""
		}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host
}

func varyOrigin(headers http.Header) {
	values := headers.Values("Vary")
	for _, value := range values {
		for token := range strings.SplitSeq(value, ",") {
			if token = strings.TrimSpace(token); token == "*" || strings.EqualFold(token, "Origin") {
				return
			}
		}
	}
	values = append(values, "Origin")
	headers.Set("Vary", strings.Join(values, ", "))
}
