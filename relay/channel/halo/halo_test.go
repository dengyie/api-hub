package halo

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/dengyie/apihub/common"
	"github.com/dengyie/apihub/constant"
	"github.com/dengyie/apihub/model"
	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeBaseURL(t *testing.T) {
	assert.Equal(t, "https://halo.video", NormalizeBaseURL(""))
	assert.Equal(t, "https://halo.video", NormalizeBaseURL("https://halo.video"))
	assert.Equal(t, "https://halo.video", NormalizeBaseURL("https://halo.video/"))
	assert.Equal(t, "https://halo.video", NormalizeBaseURL("https://halo.video/api"))
	assert.Equal(t, "https://halo.video", NormalizeBaseURL("https://halo.video/api/"))
	assert.Equal(t, "https://halo.video", NormalizeBaseURL("https://halo.video/api/v1"))
	assert.Equal(t, "https://custom.proxy", NormalizeBaseURL("https://custom.proxy/api"))
}

func TestModeForModel(t *testing.T) {
	assert.Equal(t, "t2va", ModeForModel("halo-video"))
	assert.Equal(t, "i2va", ModeForModel("halo-video-i2v"))
	assert.Equal(t, "fl2va", ModeForModel("halo-video-fl2v"))
	assert.Equal(t, "ref2va", ModeForModel("halo-video-ref2v"))
	assert.Equal(t, "source_edit", ModeForModel("halo-video-edit"))
	assert.Equal(t, "image", ModeForModel("halo-image"))
	assert.Equal(t, "t2va", ModeForModel("halo-unknown"))
}

func TestBuildGenerationPayload(t *testing.T) {
	raw := []byte(`{
		"model": "halo-video",
		"messages": [{"role":"user","content":"A cat walking on the beach"}],
		"duration":5, "resolution":"0.5mp", "aspect_ratio":"16:9",
		"text_strategy":"direct", "fps":24
	}`)
	payload, err := BuildGenerationPayload(raw)
	require.NoError(t, err)
	assert.Equal(t, "A cat walking on the beach", payload["content"])
	params, ok := payload["parameters"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(5), params["duration"])
	assert.Equal(t, "0.5mp", params["resolution"])
	assert.Equal(t, "16:9", params["aspect_ratio"])
	assert.Equal(t, "direct", params["text_strategy"])

	SetInputMode(payload, "i2va")
	assert.Equal(t, "i2va", params["input_mode"])

	quote := QuoteParameters(payload)
	assert.Equal(t, params, quote)
	// quote must be a copy, not the same map
	quote["duration"] = 99
	assert.Equal(t, float64(5), params["duration"])
}

func TestBuildGenerationPayloadMultimodal(t *testing.T) {
	raw := []byte(`{
		"messages": [
			{"role":"user","content":[
				{"type":"text","text":"Make the waterfall flow"},
				{"type":"image_url","image_url":{"url":"https://example.com/waterfall.png"}}
			]}
		]
	}`)
	payload, err := BuildGenerationPayload(raw)
	require.NoError(t, err)
	assert.Equal(t, "Make the waterfall flow", payload["content"])
	assert.Equal(t, "https://example.com/waterfall.png", payload["image_url"])
}

func TestBuildGenerationPayloadRequiresPrompt(t *testing.T) {
	_, err := BuildGenerationPayload([]byte(`{"model":"halo-video"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prompt is required")
}

func TestBuildRequestURL(t *testing.T) {
	adaptor := &TaskAdaptor{}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: "https://halo.video/api"},
	}
	url, err := adaptor.BuildRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, "https://halo.video/api/v1", url)

	url, err = adaptor.BuildRequestURL(&relaycommon.RelayInfo{})
	require.NoError(t, err)
	assert.Equal(t, "https://halo.video/api/v1", url)
}

func TestBuildRequestHeaderUsesBearer(t *testing.T) {
	adaptor := &TaskAdaptor{}
	req, err := http.NewRequest(http.MethodPost, "https://halo.video/api/v1/generation/preview", nil)
	require.NoError(t, err)
	err = adaptor.BuildRequestHeader(nil, req, &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ApiKey: "vgn_test_key"},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer vgn_test_key", req.Header.Get("Authorization"))
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
}

func TestExtractErrorMsg(t *testing.T) {
	assert.Equal(t, "insufficient credits", ExtractErrorMsg([]byte(`{"error":{"code":"quota","message":"insufficient credits"}}`)))
	assert.Equal(t, "bad request", ExtractErrorMsg([]byte(`{"message":"bad request"}`)))
	assert.Equal(t, "not found", ExtractErrorMsg([]byte(`not found`)))
	assert.Equal(t, "empty response from upstream halo", ExtractErrorMsg(nil))
}

func TestExtractJobID(t *testing.T) {
	id, err := ExtractJobID([]byte(`{"request":{"id":"req-1"},"jobs":[{"id":"job-abc","status":"queued"}]}`))
	require.NoError(t, err)
	assert.Equal(t, "job-abc", id)

	_, err = ExtractJobID([]byte(`{"request":{"id":"req-1"}}`))
	require.Error(t, err)
}

// roundTripperFunc is a pure in-memory transport — no upstream calls.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

func TestParseTaskResultStates(t *testing.T) {
	adaptor := &TaskAdaptor{}
	task := &model.Task{
		TaskID: "public-task-1",
		PrivateData: model.TaskPrivateData{
			PluginState: TaskData{JobID: "job-abc"}.Marshal(),
		},
	}

	// queued
	info, err := adaptor.ParseTaskResult(task, jsonResponse(200, `{"status":"queued"}`), []byte(`{"status":"queued"}`))
	require.NoError(t, err)
	assert.Equal(t, string(model.TaskStatusQueued), info.Status)

	// running
	info, err = adaptor.ParseTaskResult(task, jsonResponse(200, `{"status":"running","progress":{"stage":"rendering"}}`), []byte(`{"status":"running","progress":{"stage":"rendering"}}`))
	require.NoError(t, err)
	assert.Equal(t, string(model.TaskStatusInProgress), info.Status)
	assert.Equal(t, "rendering", info.Progress)

	// succeeded — URL must be gateway-relative, never the upstream signed path
	body := []byte(`{"status":"succeeded","download_url":"/api/v1/jobs/job-abc/media?download=1"}`)
	info, err = adaptor.ParseTaskResult(task, jsonResponse(200, string(body)), body)
	require.NoError(t, err)
	assert.Equal(t, string(model.TaskStatusSuccess), info.Status)
	assert.Equal(t, "/v1/videos/public-task-1/content", info.Url)
	assert.NotContains(t, info.Url, "halo.video")

	var state TaskData
	require.NoError(t, common.Unmarshal(info.PluginState, &state))
	assert.Equal(t, "/api/v1/jobs/job-abc/media?download=1", state.MediaPath)

	// failed
	body = []byte(`{"status":"failed","error":{"code":"x","message":"generation failed"}}`)
	info, err = adaptor.ParseTaskResult(task, jsonResponse(200, string(body)), body)
	require.NoError(t, err)
	assert.Equal(t, string(model.TaskStatusFailure), info.Status)
	assert.Contains(t, info.Reason, "generation failed")

	// HTTP 402 must not be mistaken for a task failure reason from body
	info, err = adaptor.ParseTaskResult(task, jsonResponse(402, `{"error":{"message":"payment required"}}`), []byte(`{"error":{"message":"payment required"}}`))
	require.NoError(t, err)
	assert.Equal(t, string(model.TaskStatusFailure), info.Status)
	assert.Equal(t, "payment required", info.Reason)
}

func TestParseResponse(t *testing.T) {
	adaptor := &TaskAdaptor{}
	gin.SetMode(gin.TestMode)
	resp := jsonResponse(200, `{"request":{"id":"req-1","status":"queued"},"jobs":[{"id":"job-abc","status":"queued"}]}`)
	out, taskErr := adaptor.ParseResponse(nil, resp, nil)
	require.Nil(t, taskErr)
	require.NotNil(t, out)
	assert.Equal(t, "job-abc", out.UpstreamTaskID)

	var data TaskData
	require.NoError(t, common.Unmarshal(out.TaskData, &data))
	assert.Equal(t, "job-abc", data.JobID)
	assert.Equal(t, "/api/v1/jobs/job-abc/media", data.MediaPath)

	bad := jsonResponse(402, `{"error":{"message":"out of credits"}}`)
	_, taskErr = adaptor.ParseResponse(nil, bad, nil)
	require.NotNil(t, taskErr)
	assert.Equal(t, 402, taskErr.StatusCode)
}

func TestActionForModel(t *testing.T) {
	assert.Equal(t, constant.TaskActionTextToVideo, ActionForModel("halo-video", nil))
	assert.Equal(t, constant.TaskActionImageToVideo, ActionForModel("halo-video-i2v", nil))
	assert.Equal(t, constant.TaskActionRemix, ActionForModel("halo-video-edit", nil))
	assert.Equal(t, constant.TaskActionImageToVideo, ActionForModel("halo-video", []byte(`{"image_url":"https://x/y.png"}`)))
}

func TestDoRequestChainWithMockTransport(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var paths []string
	var authHeaders []string
	oldTransport := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/v1/generation/preview":
			body, _ := io.ReadAll(r.Body)
			assert.Contains(t, string(body), "duration")
			assert.NotContains(t, string(body), "quote_token")
			return jsonResponse(200, `{"quote_token":"qt-123","available":true,"credits":9}`), nil
		case "/api/v1/conversations":
			return jsonResponse(200, `{"id":"conv-abc"}`), nil
		case "/api/v1/conversations/conv-abc/messages":
			body, _ := io.ReadAll(r.Body)
			assert.Contains(t, string(body), "A cat on a beach")
			assert.Contains(t, string(body), "qt-123")
			assert.Contains(t, string(body), "t2va")
			return jsonResponse(200, `{"request":{"id":"req-1","status":"queued"},"jobs":[{"id":"job-abc","status":"queued"}]}`), nil
		default:
			return jsonResponse(404, `{"error":{"message":"unexpected path"}}`), nil
		}
	})
	defer func() { http.DefaultTransport = oldTransport }()

	c, _ := gin.CreateTestContext(nil)
	c.Request = httptestRequest(t, `{"model":"halo-video","prompt":"A cat on a beach","duration":5,"resolution":"0.5mp"}`)

	adaptor := &TaskAdaptor{}
	info := &relaycommon.RelayInfo{
		OriginModelName: "halo-video",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "halo-video",
			ChannelBaseUrl:    "https://halo.video/api",
			ApiKey:            "vgn_test_key",
		},
	}
	adaptor.Init(info)

	body, err := adaptor.BuildRequestBody(c, info)
	require.NoError(t, err)
	resp, err := adaptor.DoRequest(c, info, body)
	require.NoError(t, err)
	submit, taskErr := adaptor.ParseResponse(c, resp, info)
	require.Nil(t, taskErr)
	assert.Equal(t, "job-abc", submit.UpstreamTaskID)

	assert.Equal(t, []string{
		"/api/v1/generation/preview",
		"/api/v1/conversations",
		"/api/v1/conversations/conv-abc/messages",
	}, paths)
	for _, h := range authHeaders {
		assert.Equal(t, "Bearer vgn_test_key", h)
	}
}

// TestDoRequestStopsOnUnavailableQuote verifies preview failure is fail-fast
// (no conversation or charged message request is issued).
func TestDoRequestStopsOnUnavailableQuote(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var paths []string
	oldTransport := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/api/v1/generation/preview" {
			return jsonResponse(200, `{"available":false,"error":{"message":"insufficient credits"}}`), nil
		}
		return jsonResponse(500, `{"error":{"message":"should not be called"}}`), nil
	})
	defer func() { http.DefaultTransport = oldTransport }()

	c, _ := gin.CreateTestContext(nil)
	c.Request = httptestRequest(t, `{"model":"halo-video","prompt":"A cat on a beach"}`)

	adaptor := &TaskAdaptor{}
	info := &relaycommon.RelayInfo{
		OriginModelName: "halo-video",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "halo-video",
			ChannelBaseUrl:    "https://halo.video",
			ApiKey:            "vgn_test_key",
		},
	}
	adaptor.Init(info)
	body, err := adaptor.BuildRequestBody(c, info)
	require.NoError(t, err)
	_, err = adaptor.DoRequest(c, info, body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "quote_token")
	assert.Equal(t, []string{"/api/v1/generation/preview"}, paths)
}

// TestDoRequestUsesStableIdempotencyKey verifies that the same logical request
// reuses one conversation across retries (PublicTaskID is generated once per
// logical submit), so a retry cannot create a second charged message.
func TestDoRequestUsesStableIdempotencyKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenKeys []string
	oldTransport := http.DefaultTransport
	http.DefaultTransport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/generation/preview":
			return jsonResponse(200, `{"quote_token":"qt-1","available":true}`), nil
		case "/api/v1/conversations":
			body, _ := io.ReadAll(r.Body)
			seenKeys = append(seenKeys, gjson.GetBytes(body, "idempotency_key").String())
			return jsonResponse(200, `{"id":"conv-1"}`), nil
		case "/api/v1/conversations/conv-1/messages":
			return jsonResponse(200, `{"jobs":[{"id":"job-1"}]}`), nil
		}
		return jsonResponse(404, `{}`), nil
	})
	defer func() { http.DefaultTransport = oldTransport }()

	for i := 0; i < 2; i++ { // two gateway retries of the same logical request
		c, _ := gin.CreateTestContext(nil)
		c.Request = httptestRequest(t, `{"model":"halo-video","prompt":"a cat"}`)
		adaptor := &TaskAdaptor{}
		info := &relaycommon.RelayInfo{
			OriginModelName: "halo-video",
			TaskRelayInfo:   &relaycommon.TaskRelayInfo{PublicTaskID: "task_stable_123"},
			ChannelMeta: &relaycommon.ChannelMeta{
				UpstreamModelName: "halo-video",
				ChannelBaseUrl:    "https://halo.video",
				ApiKey:            "vgn_test_key",
			},
		}
		adaptor.Init(info)
		body, err := adaptor.BuildRequestBody(c, info)
		require.NoError(t, err)
		_, err = adaptor.DoRequest(c, info, body)
		require.NoError(t, err)
	}

	assert.Equal(t, []string{"mangoapi-task_stable_123", "mangoapi-task_stable_123"}, seenKeys)
}

func TestValidateRequestRejectsOutOfRangeDuration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, body := range []string{
		`{"model":"halo-video","prompt":"x","duration":999999}`,
		`{"model":"halo-video","prompt":"x","seconds":0}`,
	} {
		c, _ := gin.CreateTestContext(nil)
		c.Request = httptestRequest(t, body)
		adaptor := &TaskAdaptor{}
		taskErr := adaptor.ValidateRequestAndSetAction(c, &relaycommon.RelayInfo{})
		require.NotNil(t, taskErr, "body: %s", body)
		assert.Equal(t, http.StatusBadRequest, taskErr.StatusCode)
	}
}

func httptestRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/v1/videos", bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	return req
}
