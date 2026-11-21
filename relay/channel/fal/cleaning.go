package fal

import (
	"errors"
	"strconv"
	"strings"

	"github.com/dengyie/apihub/common"
	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relaykit/dto"
	"github.com/gin-gonic/gin"
)

var openAIFieldsToOmit = map[string]struct{}{
	"messages":              {},
	"model":                 {},
	"stream":                {},
	"stream_options":        {},
	"temperature":           {},
	"top_p":                 {},
	"top_k":                 {},
	"max_tokens":            {},
	"max_completion_tokens": {},
	"functions":             {},
	"tools":                 {},
	"tool_choice":           {},
	"frequency_penalty":     {},
	"presence_penalty":      {},
	"response_format":       {},
	"user":                  {},
	"n":                     {},
	"stop":                  {},
	"logit_bias":            {},
	"metadata":              {},
	"store":                 {},
	"service_tier":          {},
	"safety_identifier":     {},
	"reasoning_effort":      {},
	"verbosity":             {},
	"modalities":            {},
	"audio":                 {},
	"prompt_cache_key":      {},
	"prompt_cache_retention": {},
	"prediction":            {},
	"web_search_options":    {},
	"search_parameters":     {},
	"parallel_tool_calls":   {},
	"function_call":         {},
	"usage":                 {},
	"dimensions":            {},
	"encoding_format":       {},
}

func CleanOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (map[string]any, error) {
	if request == nil {
		return nil, errors.New("request is nil")
	}

	payload := make(map[string]any)

	// If raw body exists in context storage, load extra non-OpenAI fields
	if c != nil {
		if storage, err := common.GetBodyStorage(c); err == nil && storage != nil {
			if rawBytes, bErr := storage.Bytes(); bErr == nil && len(rawBytes) > 0 {
				var rawMap map[string]any
				if err := common.Unmarshal(rawBytes, &rawMap); err == nil {
					for k, v := range rawMap {
						if _, omit := openAIFieldsToOmit[k]; !omit {
							payload[k] = v
						}
					}
				}
			}
		}
	}

	// Also check request.ExtraBody
	if len(request.ExtraBody) > 0 {
		var extraMap map[string]any
		if err := common.Unmarshal(request.ExtraBody, &extraMap); err == nil {
			for k, v := range extraMap {
				if _, omit := openAIFieldsToOmit[k]; !omit {
					payload[k] = v
				}
			}
		}
	}

	// Extract prompt and image_url from messages
	var extractedPrompt string
	var extractedImageURL string

	if len(request.Messages) > 0 {
		// Look for the last user message
		var targetMsg *dto.Message
		for i := len(request.Messages) - 1; i >= 0; i-- {
			msg := &request.Messages[i]
			if strings.EqualFold(msg.Role, "user") {
				targetMsg = msg
				break
			}
		}
		if targetMsg == nil {
			targetMsg = &request.Messages[len(request.Messages)-1]
		}

		if targetMsg != nil {
			// Extract text and image_url
			mediaContents := targetMsg.ParseContent()
			if len(mediaContents) > 0 {
				var textParts []string
				for _, part := range mediaContents {
					switch part.Type {
					case dto.ContentTypeText:
						if trimmed := strings.TrimSpace(part.Text); trimmed != "" {
							textParts = append(textParts, trimmed)
						}
					case dto.ContentTypeImageURL:
						imgMedia := part.GetImageMedia()
						if imgMedia != nil && imgMedia.Url != "" {
							if extractedImageURL == "" {
								extractedImageURL = imgMedia.Url
							}
						}
					}
				}
				if len(textParts) > 0 {
					extractedPrompt = strings.Join(textParts, "\n")
				}
			} else if targetMsg.IsStringContent() {
				extractedPrompt = strings.TrimSpace(targetMsg.StringContent())
			}
		}
	}

	// Fallback prompt from request.Prompt if messages didn't have it
	if extractedPrompt == "" && request.Prompt != nil {
		if s, ok := request.Prompt.(string); ok {
			extractedPrompt = strings.TrimSpace(s)
		}
	}

	// Apply extracted prompt if not already explicitly provided in payload
	if _, ok := payload["prompt"]; !ok || payload["prompt"] == "" {
		if extractedPrompt != "" {
			payload["prompt"] = extractedPrompt
		}
	}

	// Apply extracted image_url if not already explicitly provided in payload
	if _, ok := payload["image_url"]; !ok || payload["image_url"] == "" {
		if extractedImageURL != "" {
			payload["image_url"] = extractedImageURL
		}
	}

	// Pass seed if provided in request
	if request.Seed != nil {
		if _, ok := payload["seed"]; !ok {
			payload["seed"] = int64(*request.Seed)
		}
	}

	// Pass size as aspect_ratio if not set
	if request.Size != "" {
		if _, ok := payload["aspect_ratio"]; !ok {
			payload["aspect_ratio"] = request.Size
		}
	}

	// Validation
	if payload["prompt"] == nil && payload["image_url"] == nil {
		return nil, errors.New("fal video generation requires prompt or image_url")
	}

	return payload, nil
}

func ExtractDuration(payload map[string]any) float64 {
	if payload == nil {
		return 0
	}
	if v, ok := payload["duration"]; ok {
		return parseDurationValue(v)
	}
	if v, ok := payload["seconds"]; ok {
		return parseDurationValue(v)
	}
	return 0
}

func parseDurationValue(v any) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case float32:
		return float64(val)
	case int:
		return float64(val)
	case int64:
		return float64(val)
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
			return f
		}
	}
	return 0
}
