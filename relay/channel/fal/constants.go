package fal

import (
	"strings"
)

const (
	ChannelName    = "fal"
	DefaultBaseURL = "https://queue.fal.run"
)

var ModelList = []string{
	"fal-ai/any-llm",
	"minimax/h3-max/text-to-video",
	"minimax/h3-max/image-to-video",
	"fal-ai/kling-video/v3/pro/text-to-video",
	"fal-ai/kling-video/v3/turbo/pro/text-to-video",
	"fal-ai/kling-video/v3/pro/image-to-video",
	"alibaba/wan-3.0/text-to-video",
	"alibaba/wan-3.0/image-to-video",
	"fal-ai/veo3.1",
	"fal-ai/veo3.1/image-to-video",
	"blackforestlabs/flux-3-max/text-to-video",
	"blackforestlabs/flux-3-max/image-to-video",
}

func NormalizeBaseURL(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return DefaultBaseURL
	}
	// fal-ai/any-llm/openai is deprecated/unpriced on fal.ai; openrouter router is the real endpoint
	if strings.Contains(raw, "any-llm/openai") {
		return "https://fal.run/openrouter/router/openai/v1"
	}
	if strings.HasSuffix(raw, "openrouter/router/openai") {
		return raw + "/v1"
	}
	if raw == "https://fal.run" || raw == "http://fal.run" {
		return DefaultBaseURL
	}
	if strings.HasPrefix(raw, "https://fal.run/") && !strings.Contains(raw, "openrouter") {
		return DefaultBaseURL + strings.TrimPrefix(raw, "https://fal.run")
	}
	if strings.HasPrefix(raw, "http://fal.run/") && !strings.Contains(raw, "openrouter") {
		return "http://queue.fal.run" + strings.TrimPrefix(raw, "http://fal.run")
	}
	return raw
}

func IsOpenRouterEndpoint(raw string) bool {
	return strings.Contains(raw, "openrouter") || strings.Contains(raw, "any-llm/openai")
}
