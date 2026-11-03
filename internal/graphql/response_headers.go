// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"encoding/json"
	"mime"
	"net/http"
)

// jsonResponseWriter preserves the transport's negotiated media type while adding
// the legacy charset. It is used only for ordinary HTTP responses, never upgrades.
type jsonResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *jsonResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *jsonResponseWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		} else if contentType, params, err := mime.ParseMediaType(w.Header().Get("Content-Type")); err == nil && (contentType == "application/json" || contentType == "application/graphql-response+json") {
			params["charset"] = "utf-8"
			w.Header().Set("Content-Type", mime.FormatMediaType(contentType, params))
		}
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *jsonResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func writeUnsupportedGraphQLMethod(w http.ResponseWriter) {
	const message = "Apollo Server supports only GET/POST requests."
	w.Header().Set("Allow", "GET, POST")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusMethodNotAllowed)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]any{{
			"message": message,
			"extensions": map[string]any{
				"status": 400, "code": "Bad Request", "type": "BAD_REQUEST", "name": "GRAPHQL_BAD_REQUEST",
				"message": "GraphQL bad request, code: BAD_REQUEST, " + message,
				"data":    map[string]string{"code": "BAD_REQUEST", "message": message},
			},
		}},
	})
}
