// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import "testing"

func TestTranscriptUsageMatchesNativeProviderCounters(t *testing.T) {
	cases := []struct {
		format, body string
		want         TranscriptTokenUsage
	}{
		{"gemini", `{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":3,"totalTokenCount":18,"cachedContentTokenCount":4}}`, TranscriptTokenUsage{10, 5, 18, 4}},
		{"openai-chat", `{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}}`, TranscriptTokenUsage{10, 5, 15, 4}},
		{"openai-responses", `{"usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":4}}}`, TranscriptTokenUsage{10, 5, 15, 4}},
		{"anthropic", `{"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":4,"cache_creation_input_tokens":2}}`, TranscriptTokenUsage{16, 5, 21, 4}},
	}
	for _, tc := range cases {
		t.Run(tc.format, func(t *testing.T) {
			got, err := transcriptProviderUsage([]byte(tc.body), tc.format)
			if err != nil || got == nil || *got != tc.want {
				t.Fatalf("usage=%#v err=%v want=%#v", got, err, tc.want)
			}
		})
	}
	for _, body := range []string{`{"usage":{"input_tokens":-1}}`, `{"usage":{"input_tokens":2147483648}}`, `{"usage":{"input_tokens":1.5}}`, `{"usage":[]}`} {
		if got, err := transcriptProviderUsage([]byte(body), "openai-responses"); err == nil || got != nil {
			t.Fatalf("accepted invalid usage %s", body)
		}
	}
	if got, err := transcriptProviderUsage([]byte(`{}`), "gemini"); err != nil || got != nil {
		t.Fatalf("absent usage = %#v %v", got, err)
	}
}
