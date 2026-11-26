// Package halo implements a native task adaptor for halo.video, a
// conversation-based video generation platform with no OpenAI-compatible
// surface. The upstream workflow is:
//
//	POST /api/v1/generation/preview            (free quote, returns quote_token)
//	POST /api/v1/conversations                 (create conversation, idempotency key)
//	POST /api/v1/conversations/{id}/messages   (submit prompt+parameters+quote_token)
//	GET  /api/v1/jobs/{job_id}                 (poll; supports ?wait_seconds=N long-poll)
//	GET  /api/v1/jobs/{job_id}/media?download=1 (signed download, needs Bearer auth)
//
// The TaskAdaptor folds preview+conversation+submit into one upstream call
// chain and stores conversation/job ids plus the media path in PluginState so
// polling and the content proxy can resume without re-quoting.
package halo

import (
	"github.com/dengyie/apihub/constant"
)

const (
	ChannelName    = "halo"
	DefaultBaseURL = constant.HaloDefaultBaseURL

	// MaxDurationSeconds bounds the per-second billing multiplier so a single
	// request cannot pre-charge an unbounded quota amount before preview.
	MaxDurationSeconds = 60
)

// NormalizeBaseURL strips the site's /api suffix so callers always work from
// the host root and append "/api/v1/..." themselves. Accepts all of
// https://halo.video, https://halo.video/, https://halo.video/api and
// https://halo.video/api/v1 and returns https://halo.video.
func NormalizeBaseURL(raw string) string {
	return constant.NormalizeHaloBaseURL(raw)
}

// ModelList exposes the gateway-facing model names. halo.video has a single
// unified generation engine with no model catalog; each model name selects the
// generation mode (see ModeForModel).
var ModelList = []string{
	"halo-video",       // text-to-video (t2va)
	"halo-video-i2v",   // image-to-video (i2va)
	"halo-video-fl2v",  // first/last frame interpolation (fl2va)
	"halo-video-ref2v", // reference-image video (ref2va)
	"halo-video-edit",  // source video edit (source edit)
	"halo-image",       // image generation
}

// ModeForModel maps a gateway model name to the upstream input_mode. Unknown
// names fall back to t2va so bare "halo-*" aliases still work.
func ModeForModel(model string) string {
	switch model {
	case "halo-video-i2v":
		return "i2va"
	case "halo-video-fl2v":
		return "fl2va"
	case "halo-video-ref2v":
		return "ref2va"
	case "halo-video-edit":
		return "source_edit"
	case "halo-image":
		return "image"
	default:
		return "t2va"
	}
}

// IsHaloModel reports whether the model name belongs to this channel.
func IsHaloModel(model string) bool {
	switch model {
	case "halo-video", "halo-video-i2v", "halo-video-fl2v", "halo-video-ref2v", "halo-video-edit", "halo-image":
		return true
	}
	return false
}
