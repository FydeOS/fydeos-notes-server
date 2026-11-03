// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSplitTranscriptAudioIntoBoundedOpusParts(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is unavailable")
	}
	if err := exec.Command(ffmpeg, "-version").Run(); err != nil {
		t.Skip("ffmpeg is not runnable")
	}
	if err := exec.Command(ffprobe, "-version").Run(); err != nil {
		t.Skip("ffprobe is not runnable")
	}
	command := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=31", "-c:a", "pcm_s16le", "-f", "wav", "pipe:1")
	body, err := command.Output()
	if err != nil {
		t.Fatal("create recovery fixture", err)
	}
	parts, err := splitTranscriptAudio(context.Background(), body, "audio/wav")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].StartSec != 0 || parts[0].DurationSec < 29.9 || parts[0].DurationSec > 30.1 || parts[1].StartSec != 30 || parts[1].DurationSec < .9 || parts[1].DurationSec > 1.1 {
		t.Fatalf("unexpected recovery parts: %#v", parts)
	}
	if got := transcriptAudioExtension("application/octet-stream"); got != "" {
		t.Fatalf("accepted unknown audio format: %q", got)
	}
}

func TestSplitTranscriptAudioRunsBoundedTranscoderAndProbe(t *testing.T) {
	directory := t.TempDir()
	ffmpegPath := filepath.Join(directory, "ffmpeg-fixture")
	ffprobePath := filepath.Join(directory, "ffprobe-fixture")
	ffmpeg := "#!/bin/sh\nfor output do :; done\nprefix=${output%%%03d.ogg}\nprintf part > \"${prefix}000.ogg\"\nprintf part > \"${prefix}001.ogg\"\n"
	ffprobe := "#!/bin/sh\nfor input do :; done\ncase \"$input\" in *000.ogg) echo 30 ;; *001.ogg) echo 1 ;; *) exit 1 ;; esac\n"
	if err := os.WriteFile(ffmpegPath, []byte(ffmpeg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ffprobePath, []byte(ffprobe), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FFMPEG_BINARY", ffmpegPath)
	t.Setenv("FFPROBE_BINARY", ffprobePath)
	parts, err := splitTranscriptAudio(context.Background(), []byte("source"), "audio/mp4")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || string(parts[0].Data) != "part" || parts[0].StartSec != 0 || parts[0].DurationSec != 30 || parts[1].StartSec != 30 || parts[1].DurationSec != 1 {
		t.Fatalf("parts = %#v", parts)
	}
	if strings.TrimSpace(string(parts[0].Data)) == "" {
		t.Fatal("empty transcoded part")
	}
}
