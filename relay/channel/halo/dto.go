package halo

import (
	"fmt"
	"strings"

	"github.com/dengyie/apihub/common"
	"github.com/tidwall/gjson"
)

// PreviewRequest quotes a generation. Quote tokens bind to the exact
// parameters quoted, so the submit body must reuse the same parameters
// verbatim (BindParameters enforces that).
type PreviewRequest struct {
	Parameters     map[string]any `json:"parameters"`
	AssetIDs       []string       `json:"asset_ids,omitempty"`
	CandidateCount int            `json:"candidate_count,omitempty"`
	Polish         bool           `json:"polish,omitempty"`
	TextStrategy   string         `json:"-"`
}

// SubmitResponse is the conversation messages response. The jobs array holds
// one entry per candidate; the first job id is the task id we track.
type SubmitResponse struct {
	Request SubmitRequest `json:"request"`
	Jobs    []SubmitJob   `json:"jobs"`
	Detail  any           `json:"detail,omitempty"`
	Message string        `json:"message,omitempty"`
	Error   any           `json:"error,omitempty"`
}

type SubmitRequest struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	OriginalPrompt string `json:"original_prompt"`
}

type SubmitJob struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// JobStatusResponse is the poll payload. Status values observed: queued,
// running, succeeded, failed. download_url/video_url are gateway-relative
// paths that require the Bearer key, so they are never handed to clients
// directly — the content proxy serves the bytes instead.
type JobStatusResponse struct {
	Status      string `json:"status"`
	Progress    any    `json:"progress,omitempty"`
	DownloadURL string `json:"download_url,omitempty"`
	VideoURL    string `json:"video_url,omitempty"`
	PosterURL   string `json:"poster_url,omitempty"`
	Error       any    `json:"error,omitempty"`
	Detail      any    `json:"detail,omitempty"`
	Message     string `json:"message,omitempty"`
}

// TaskData is the cross-round plugin state: conversation id for submit,
// job id for polling, media path for the final content request.
type TaskData struct {
	ConversationID string `json:"conversation_id,omitempty"`
	JobID          string `json:"job_id,omitempty"`
	MediaPath      string `json:"media_path,omitempty"`
}

func (t TaskData) Marshal() []byte {
	b, _ := common.Marshal(t)
	return b
}

// ExtractErrorMsg pulls a human-readable message out of any upstream error
// envelope. halo.video uses {"error":{"code","message"}} but some 4xx return
// plain text, so fall through to the raw body.
func ExtractErrorMsg(body []byte) string {
	if len(body) == 0 {
		return "empty response from upstream halo"
	}
	res := gjson.ParseBytes(body)
	if val := res.Get("error.message"); val.Exists() && val.String() != "" {
		return val.String()
	}
	if val := res.Get("message"); val.Exists() && val.String() != "" {
		return val.String()
	}
	if val := res.Get("detail"); val.Exists() {
		if s := val.String(); s != "" && s != "null" {
			return s
		}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 512 {
		return msg[:512] + "..."
	}
	if msg == "" {
		return "unknown upstream halo error"
	}
	return msg
}

// ExtractJobID picks the first job id from a submit response. candidate_count
// > 1 yields multiple jobs; the task engine tracks one, the rest are billed
// upstream but not surfaced (single-candidate requests are the norm).
func ExtractJobID(body []byte) (string, error) {
	res := gjson.ParseBytes(body)
	if id := res.Get("jobs.0.id"); id.Exists() && id.String() != "" {
		return id.String(), nil
	}
	return "", fmt.Errorf("no job id in halo submit response: %s", truncate(string(body), 256))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
