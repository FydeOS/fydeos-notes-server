// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"errors"
	"strings"
	"testing"
)

func TestTranscriptErrorCodeIsSafeForStorageAndRealtime(t *testing.T) {
	for _, code := range []string{"provider secret: sk-test", strings.Repeat("x", 129), "", "bad\ncode"} {
		if got := transcriptErrorCode(transcriptExecutionError(code)); got != TranscriptProviderErrorCode {
			t.Errorf("unsafe code became %q", got)
		}
	}
	if got := transcriptErrorCode(errors.New("private provider body")); got != TranscriptProviderErrorCode {
		t.Fatal(got)
	}
	if got := transcriptErrorCode(errTranscriptRoute); got != string(errTranscriptRoute) {
		t.Fatal(got)
	}
}
