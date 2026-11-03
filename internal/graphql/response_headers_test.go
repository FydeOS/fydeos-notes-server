// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fydeos/fydeos-notes-server/internal/config"
)

func TestGraphQLResponseHeadersMatchLegacy(t *testing.T) {
	h := NewHandler(config.Config{}, noSessionAuthenticator{}, nil, nil)
	for _, mediaType := range []string{"application/json", "application/graphql-response+json"} {
		r := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"query { currentUser { id } }"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", mediaType)
		w := newDeadlineTestResponseWriter()
		h.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("Content-Type") != mediaType+"; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: status=%d headers=%v body=%s", mediaType, w.Code, w.Header(), w.Body.String())
		}
	}
}

func TestGraphQLUnsupportedMethodMatchesLegacy(t *testing.T) {
	h := NewHandler(config.Config{}, noSessionAuthenticator{}, nil, nil)
	for _, method := range []string{http.MethodOptions, http.MethodPut, http.MethodDelete} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/graphql", nil))
		if w.Code != 405 || w.Header().Get("Allow") != "GET, POST" || w.Header().Get("Cache-Control") != "" || !strings.Contains(w.Body.String(), `"name":"GRAPHQL_BAD_REQUEST"`) || !strings.Contains(w.Body.String(), `"message":"Apollo Server supports only GET/POST requests."`) {
			t.Errorf("%s: status=%d headers=%v body=%s", method, w.Code, w.Header(), w.Body.String())
		}
	}
}
