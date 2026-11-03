// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package byok

import "slices"

var (
	supportedProviders = []string{"anthropic", "fal", "gemini", "openai"}
	allowedInputs      = []string{"text", "image", "audio", "file"}
	allowedOutputs     = []string{"text", "object", "structured", "embedding", "rerank", "image"}
	allowedFeatures    = []string{"tool_calling", "reasoning", "web_search"}
	allowedKinds       = []string{"image", "audio", "file"}
	allowedSources     = []string{"url", "data", "bytes", "file_handle"}
)

func isSupportedProvider(provider string) bool { return slices.Contains(supportedProviders, provider) }

func capOf(in, out []string, features []string, kinds, sources []string) Capability {
	return Capability{Input: in, Output: out, Features: features, AttachmentKinds: kinds, AttachmentSource: sources}
}

// Probe operation requirements.
var probeRequirements = map[string]Capability{
	"chat":         capOf([]string{"text"}, []string{"text"}, nil, nil, nil),
	"structured":   capOf([]string{"text"}, []string{"structured"}, nil, nil, nil),
	"tool_calling": capOf([]string{"text"}, []string{"text"}, []string{"tool_calling"}, nil, nil),
	"vision":       capOf([]string{"text", "image"}, []string{"text"}, nil, []string{"image"}, []string{"data"}),
	"embedding":    capOf([]string{"text"}, []string{"embedding"}, nil, nil, nil),
	"rerank":       capOf([]string{"text"}, []string{"rerank"}, nil, nil, nil),
	"image":        capOf([]string{"text"}, []string{"image"}, nil, nil, nil),
	"transcript":   capOf([]string{"audio"}, []string{"structured"}, nil, []string{"audio"}, []string{"data"}),
}

var imageRequirements = map[string]Capability{
	"image_generate": capOf([]string{"text"}, []string{"image"}, nil, nil, nil),
	"image_edit":     capOf([]string{"text", "image"}, []string{"image"}, nil, []string{"image"}, []string{"data"}),
	"image_process":  capOf([]string{"text", "image"}, []string{"image"}, nil, []string{"image"}, []string{"data"}),
}

// Server-side (non-chat) action requirements.
var serverRequirements = map[string]Capability{
	"embedding":      probeRequirements["embedding"],
	"structured":     probeRequirements["structured"],
	"transcript":     capOf([]string{"audio"}, []string{"structured"}, nil, []string{"audio"}, []string{"url", "data", "bytes", "file_handle"}),
	"image_generate": imageRequirements["image_generate"],
	"image_edit":     imageRequirements["image_edit"],
	"image_process":  imageRequirements["image_process"],
}

func subset(small, large []string) bool {
	for _, v := range small {
		if !slices.Contains(large, v) {
			return false
		}
	}
	return true
}

// covers reports whether outer is a field-wise superset of inner.
func covers(outer, inner Capability) bool {
	return subset(inner.Input, outer.Input) && subset(inner.Output, outer.Output) &&
		subset(inner.Features, outer.Features) && subset(inner.AttachmentKinds, outer.AttachmentKinds) &&
		subset(inner.AttachmentSource, outer.AttachmentSource)
}

// satisfies reports whether any declared capability covers the requirement.
func satisfies(declared []Capability, requirement Capability) bool {
	for _, c := range declared {
		if covers(c, requirement) {
			return true
		}
	}
	return false
}
