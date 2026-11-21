package fal

import (
	"fmt"
	"strings"

	"github.com/dengyie/apihub/common"
	"github.com/tidwall/gjson"
)

var candidateVideoPaths = []string{
	"video.url",
	"videos.0.url",
	"output.video.url",
	"output.videos.0.url",
	"video_url",
	"images.0.url",
	"output.images.0.url",
	"image.url",
	"file.url",
	"video.file_url",
}

type FalQueueSubmitResponse struct {
	RequestID   string `json:"request_id"`
	StatusURL   string `json:"status_url"`
	ResponseURL string `json:"response_url"`
	Status      string `json:"status,omitempty"`
	CancelURL   string `json:"cancel_url,omitempty"`
	Detail      any    `json:"detail,omitempty"`
	Message     string `json:"message,omitempty"`
	Error       string `json:"error,omitempty"`
}

type FalQueueStatusResponse struct {
	Status        string `json:"status"` // IN_QUEUE, IN_PROGRESS, COMPLETED, FAILED
	ResponseURL   string `json:"response_url,omitempty"`
	Logs          any    `json:"logs,omitempty"`
	Metrics       any    `json:"metrics,omitempty"`
	QueuePosition *int   `json:"queue_position,omitempty"`
	Error         string `json:"error,omitempty"`
	Detail        any    `json:"detail,omitempty"`
}

type FalTaskData struct {
	RequestID   string `json:"request_id"`
	StatusURL   string `json:"status_url"`
	ResponseURL string `json:"response_url"`
	VideoURL    string `json:"video_url,omitempty"`
}

func ExtractErrorMsg(body []byte) string {
	if len(body) == 0 {
		return "empty response from upstream fal"
	}
	res := gjson.ParseBytes(body)
	if val := res.Get("detail"); val.Exists() {
		if val.IsArray() {
			var msgs []string
			for _, item := range val.Array() {
				if msg := item.Get("msg"); msg.Exists() {
					loc := item.Get("loc").String()
					msgs = append(msgs, fmt.Sprintf("%s: %s", loc, msg.String()))
				} else {
					msgs = append(msgs, item.String())
				}
			}
			if len(msgs) > 0 {
				return strings.Join(msgs, "; ")
			}
		}
		if s := val.String(); s != "" {
			return s
		}
	}
	if val := res.Get("message"); val.Exists() && val.String() != "" {
		return val.String()
	}
	if val := res.Get("error"); val.Exists() && val.String() != "" {
		return val.String()
	}
	msg := string(body)
	if len(msg) > 512 {
		return msg[:512] + "..."
	}
	return msg
}

func ExtractVideoURL(body []byte) (string, error) {
	if len(body) == 0 {
		return "", fmt.Errorf("empty response body from fal")
	}
	res := gjson.ParseBytes(body)
	for _, p := range candidateVideoPaths {
		if val := res.Get(p); val.Exists() && val.String() != "" {
			return val.String(), nil
		}
	}

	// In case output has raw url directly
	if urlVal := res.Get("url"); urlVal.Exists() && urlVal.String() != "" {
		return urlVal.String(), nil
	}

	preview := string(body)
	if len(preview) > 256 {
		preview = preview[:256] + "..."
	}
	return "", fmt.Errorf("no video url found in fal response: %s", preview)
}

func MarshalJSON(v any) ([]byte, error) {
	return common.Marshal(v)
}
