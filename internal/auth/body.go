// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"
	"unicode/utf16"
)

const (
	signInBodyLimit = 96 << 10
	nativeBodyLimit = 16 << 10
	bodyReadTimeout = 30 * time.Second
)

// readBody reads at most limit bytes under a read deadline. It returns
// errBodyTooLarge when the body exceeds the limit.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	w.Header().Set("Connection", "close")
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, errMalformedBody
	}
	if int64(len(data)) > limit {
		return nil, errBodyTooLarge
	}
	_ = rc.SetReadDeadline(time.Time{})
	w.Header().Del("Connection")
	return data, nil
}

func isJSONContentType(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// parseStringObject parses a single JSON object whose values must all be
// strings and whose keys must be in allowed. nullable keys may also be null.
func parseStringObject(data []byte, allowed map[string]bool, nullable map[string]bool) (map[string]string, error) {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errMalformedBody
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, errMalformedBody
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errMalformedBody
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if !allowed[k] {
			return nil, errMalformedBody
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) && nullable[k] {
			continue
		}
		var s string
		if len(v) == 0 || v[0] != '"' || json.Unmarshal(v, &s) != nil {
			return nil, errMalformedBody
		}
		out[k] = s
	}
	return out, nil
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// optionalLength checks an optional field length in UTF-16 units.
func optionalLength(fields map[string]string, key string, min, max int) bool {
	v, ok := fields[key]
	if !ok {
		return true
	}
	n := utf16Len(v)
	return n >= min && n <= max
}

func setPragma(w http.ResponseWriter) { setNoStore(w, true) }
