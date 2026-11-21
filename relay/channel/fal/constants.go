package fal

import (
	"github.com/dengyie/apihub/constant"
)

const (
	ChannelName    = "fal"
	DefaultBaseURL = constant.FalDefaultBaseURL
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
	return constant.NormalizeFalBaseURL(raw)
}

func IsOpenRouterEndpoint(raw string) bool {
	return constant.IsFalOpenRouterEndpoint(raw)
}
