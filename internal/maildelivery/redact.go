// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package maildelivery

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxRedactedLength is the maximum number of runes kept by redact, including
// the trailing ellipsis added on truncation.
const maxRedactedLength = 1000

var (
	redactURLPattern    = regexp.MustCompile(`(?i)https?://[^\s<>"'()\[\]{}]+`)
	redactEmailPattern  = regexp.MustCompile(`[\p{L}\p{N}._%+\-]+@[\p{L}\p{N}\-]+(?:\.[\p{L}\p{N}\-]+)+`)
	redactNumberPattern = regexp.MustCompile(`\p{Nd}{6,}`)
)

// redact removes personal data and secrets from SMTP error messages and
// provider responses before they are stored: URLs become "[url]", email
// addresses "[email]" and runs of six or more digits (one-time codes, phone
// numbers) "[number]". Whitespace runs collapse to one space, the result is
// trimmed, and it is capped at maxRedactedLength runes with a trailing "…"
// when truncated.
func redact(value string) string {
	value = redactURLPattern.ReplaceAllLiteralString(value, "[url]")
	value = redactEmailPattern.ReplaceAllLiteralString(value, "[email]")
	value = redactNumberPattern.ReplaceAllLiteralString(value, "[number]")
	value = strings.Join(strings.Fields(value), " ")
	if utf8.RuneCountInString(value) <= maxRedactedLength {
		return value
	}
	runes := []rune(value)[:maxRedactedLength-1]
	return strings.TrimRight(string(runes), " ") + "…"
}
