package fal

import (
	"strings"
)

const (
	ChannelName    = "fal"
	DefaultBaseURL = "https://queue.fal.run"
)

var ModelList = []string{
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
	if raw == "https://fal.run" || raw == "http://fal.run" {
		return DefaultBaseURL
	}
	if strings.HasPrefix(raw, "https://fal.run/") {
		return DefaultBaseURL + strings.TrimPrefix(raw, "https://fal.run")
	}
	if strings.HasPrefix(raw, "http://fal.run/") {
		return "http://queue.fal.run" + strings.TrimPrefix(raw, "http://fal.run")
	}
	return raw
}
