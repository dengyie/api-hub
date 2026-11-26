package halo

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// BuildGenerationPayload converts an OpenAI-style task request (chat messages
// or prompt field) into halo's generation payload: top-level content plus
// parameters block. Supported client hints (all optional, sensible upstream
// defaults otherwise): duration (seconds), resolution (e.g. "0.5mp",
// "720p"), aspect_ratio (e.g. "16:9"), text_strategy ("direct" |
// "first_frame"), fps, seed, audio_mode, image_url / image / input_reference
// for image-conditioned modes, asset_ids for uploaded assets.
func BuildGenerationPayload(rawBody []byte) (map[string]any, error) {
	res := gjson.ParseBytes(rawBody)

	prompt := res.Get("prompt").String()
	if prompt == "" {
		prompt = lastUserContent(res)
	}
	if prompt == "" {
		return nil, fmt.Errorf("halo adaptor: prompt is required")
	}

	params := map[string]any{}

	// input_mode: model name wins, then explicit client field, then t2va.
	// The caller (task adaptor) overrides with ModeForModel before submit.
	if m := res.Get("input_mode"); m.Exists() && m.String() != "" {
		params["input_mode"] = m.String()
	}
	if v := res.Get("duration"); v.Exists() {
		params["duration"] = v.Float()
	} else if v := res.Get("seconds"); v.Exists() {
		params["duration"] = v.Float()
	}
	if v := res.Get("resolution"); v.Exists() && v.String() != "" {
		params["resolution"] = v.String()
	}
	if v := res.Get("aspect_ratio"); v.Exists() && v.String() != "" {
		params["aspect_ratio"] = v.String()
	}
	if v := res.Get("text_strategy"); v.Exists() && v.String() != "" {
		params["text_strategy"] = v.String()
	} else if v := res.Get("text_strategy"); !v.Exists() {
		// keep default, upstream chooses
	}
	if v := res.Get("fps"); v.Exists() && v.Int() > 0 {
		params["fps"] = v.Int()
	}
	if v := res.Get("seed"); v.Exists() {
		params["seed"] = v.Int()
	}
	if v := res.Get("audio_mode"); v.Exists() && v.String() != "" {
		params["audio_mode"] = v.String()
	}
	if v := res.Get("polish"); v.Exists() {
		params["polish"] = v.Bool()
	}

	// Image reference: top-level slots, then any image_url part inside the
	// last user message's multimodal content array.
	imageURL := firstString(res, "image_url", "input_reference", "image")
	if imageURL == "" {
		imageURL = imageFromMessages(res)
	}

	payload := map[string]any{
		"content":    prompt,
		"parameters": params,
	}
	if imageURL != "" {
		payload["image_url"] = imageURL
	}
	if a := res.Get("asset_ids"); a.Exists() && a.IsArray() {
		var assets []string
		for _, item := range a.Array() {
			if s := item.String(); s != "" {
				assets = append(assets, s)
			}
		}
		if len(assets) > 0 {
			payload["asset_ids"] = assets
		}
	}

	return payload, nil
}

// SetInputMode forces the mode derived from the model name, overriding any
// client-supplied input_mode (model selection is the authoritative routing).
func SetInputMode(payload map[string]any, mode string) {
	if params, ok := payload["parameters"].(map[string]any); ok {
		params["input_mode"] = mode
	}
}

// QuoteParameters extracts the exact parameters blob for the preview call.
// Quote tokens bind to parameters, so submit must send back the identical map.
func QuoteParameters(payload map[string]any) map[string]any {
	params, _ := payload["parameters"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		out[k] = v
	}
	return out
}

// lastUserContent extracts the last user message text from an OpenAI-style
// messages array (string or multimodal content parts).
func lastUserContent(res gjson.Result) string {
	messages := res.Get("messages")
	if !messages.Exists() || !messages.IsArray() {
		return ""
	}
	var last string
	for _, msg := range messages.Array() {
		if msg.Get("role").String() != "user" {
			continue
		}
		content := msg.Get("content")
		if content.Type == gjson.String {
			if s := content.String(); s != "" {
				last = s
			}
			continue
		}
		// multimodal: concatenate text parts
		var texts []string
		if content.IsArray() {
			for _, part := range content.Array() {
				if part.Get("type").String() == "text" {
					if t := part.Get("text").String(); t != "" {
						texts = append(texts, t)
					}
				}
			}
		}
		if len(texts) > 0 {
			last = strings.Join(texts, "\n")
		}
	}
	return last
}

// imageFromMessages finds the first image URL in an OpenAI multimodal
// content array ({"type":"image_url","image_url":{"url":...}}).
func imageFromMessages(res gjson.Result) string {
	for _, msg := range res.Get("messages").Array() {
		for _, part := range msg.Get("content").Array() {
			if part.Get("type").String() != "image_url" {
				continue
			}
			if u := part.Get("image_url.url"); u.Exists() && u.String() != "" {
				return u.String()
			}
			if u := part.Get("image_url"); u.Type == gjson.String && u.String() != "" {
				return u.String()
			}
		}
	}
	return ""
}

// firstString returns the first non-empty string among the given gjson paths.
func firstString(res gjson.Result, paths ...string) string {
	for _, p := range paths {
		if v := res.Get(p); v.Exists() {
			if v.Type == gjson.String && v.String() != "" {
				return v.String()
			}
			// {"image_url": {"url": ...}} nesting
			if u := v.Get("url"); u.Exists() && u.String() != "" {
				return u.String()
			}
		}
	}
	return ""
}
