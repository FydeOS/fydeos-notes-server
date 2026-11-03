// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package maildelivery

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"whitespace only", " \t\n ", ""},
		{"plain smtp error kept", "550 5.1.1 mailbox unavailable", "550 5.1.1 mailbox unavailable"},
		{"email", "550 5.1.1 <alice@example.com>: Recipient address rejected", "550 5.1.1 <[email]>: Recipient address rejected"},
		{"email with plus and subdomain", "rcpt bob.smith+notes@mail.example.co.uk failed", "rcpt [email] failed"},
		{"several emails", "from a@b.io to c@d.io", "from [email] to [email]"},
		{"https url", "see https://example.com/help?id=1&x=y for details", "see [url] for details"},
		{"http url uppercase", "HTTP://Example.com/path", "[url]"},
		{"url with credentials", "https://user:pw@example.com/x", "[url]"},
		{"url in angle brackets", "<https://example.com/a>", "<[url]>"},
		{"six digit code", "code 123456 expired", "code [number] expired"},
		{"long number", "call 8613800138000", "call [number]"},
		{"five digits kept", "queue 12345", "queue 12345"},
		{"digits glued to text", "id=abc1234567", "id=abc[number]"},
		{"collapse whitespace", "  421\t4.7.0\n\n try   again  later ", "421 4.7.0 try again later"},
		{"mixed", "450 user@example.com https://x.test/y 9876543", "450 [email] [url] [number]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redact(tt.input); got != tt.want {
				t.Fatalf("redact(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRedactTruncatesByRunes(t *testing.T) {
	exact := strings.Repeat("é", maxRedactedLength)
	if got := redact(exact); got != exact {
		t.Fatalf("redact kept %d runes, want input unchanged", utf8.RuneCountInString(got))
	}

	long := strings.Repeat("错", maxRedactedLength+500)
	got := redact(long)
	if n := utf8.RuneCountInString(got); n != maxRedactedLength {
		t.Fatalf("redacted length = %d runes, want %d", n, maxRedactedLength)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated value %q does not end with an ellipsis", got[len(got)-10:])
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncated value is not valid UTF-8")
	}
}
