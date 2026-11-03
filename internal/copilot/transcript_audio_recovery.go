// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	transcriptRecoveryPartSeconds = 30
	maxTranscriptRecoveryParts    = 120
	transcriptRecoveryTimeout     = time.Minute
)

type transcriptAudioPart struct {
	Data        []byte
	StartSec    float64
	DurationSec float64
}

func splitTranscriptAudio(parent context.Context, body []byte, mime string) ([]transcriptAudioPart, error) {
	if len(body) == 0 || int64(len(body)) > maxTranscriptUploadBytes {
		return nil, errors.New("transcript recovery audio size is invalid")
	}
	extension := transcriptAudioExtension(mime)
	if extension == "" {
		return nil, errors.New("transcript recovery audio format is unsupported")
	}
	ffmpeg, err := executablePath("FFMPEG_BINARY", "ffmpeg")
	if err != nil {
		return nil, err
	}
	ffprobe, err := executablePath("FFPROBE_BINARY", "ffprobe")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, transcriptRecoveryTimeout)
	defer cancel()
	directory, err := os.MkdirTemp("", "notes-transcript-recovery-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	inputPath := filepath.Join(directory, "input"+extension)
	if err := os.WriteFile(inputPath, body, 0o600); err != nil {
		return nil, err
	}
	pattern := filepath.Join(directory, "part-%03d.ogg")
	command := exec.CommandContext(ctx, ffmpeg,
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", inputPath,
		"-vn", "-c:a", "libopus", "-b:a", "48k", "-f", "segment", "-segment_time", strconv.Itoa(transcriptRecoveryPartSeconds),
		"-reset_timestamps", "1", "-segment_format", "ogg", pattern,
	)
	if output, err := command.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		_ = output
		return nil, errors.New("audio transcoder failed")
	}
	matches, err := filepath.Glob(filepath.Join(directory, "part-*.ogg"))
	if err != nil || len(matches) == 0 || len(matches) > maxTranscriptRecoveryParts {
		return nil, errors.New("transcript recovery produced an invalid part count")
	}
	sort.Strings(matches)
	parts := make([]transcriptAudioPart, 0, len(matches))
	var total int64
	for index, fileName := range matches {
		data, err := os.ReadFile(fileName)
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		if len(data) == 0 || total > maxTranscriptUploadBytes {
			return nil, errors.New("transcript recovery output exceeds size limit")
		}
		duration, err := transcriptPartDuration(ctx, ffprobe, fileName)
		if err != nil || duration <= 0 || duration > transcriptRecoveryPartSeconds+1 {
			return nil, errors.New("transcript recovery duration is invalid")
		}
		parts = append(parts, transcriptAudioPart{Data: data, StartSec: float64(index * transcriptRecoveryPartSeconds), DurationSec: duration})
	}
	return parts, nil
}

func transcriptPartDuration(ctx context.Context, ffprobe, fileName string) (float64, error) {
	command := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", fileName)
	output, err := command.Output()
	if err != nil {
		return 0, errors.New("audio probe failed")
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, errors.New("audio probe returned invalid duration")
	}
	return value, nil
}

func executablePath(environment, fallback string) (string, error) {
	value := strings.TrimSpace(os.Getenv(environment))
	if value == "" {
		value = fallback
	}
	path, err := exec.LookPath(value)
	if err != nil {
		return "", fmt.Errorf("%s is unavailable", environment)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode()&fs.ModeType != 0 {
		return "", fmt.Errorf("%s is unavailable", environment)
	}
	return path, nil
}

func transcriptAudioExtension(mime string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0])) {
	case "audio/mp4", "audio/x-m4a", "video/mp4":
		return ".m4a"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/ogg", "application/ogg", "audio/opus":
		return ".ogg"
	case "audio/webm", "video/webm":
		return ".webm"
	case "audio/wav", "audio/x-wav", "audio/wave", "audio/vnd.wave":
		return ".wav"
	case "audio/flac", "audio/x-flac":
		return ".flac"
	case "audio/aac":
		return ".aac"
	default:
		return ""
	}
}
