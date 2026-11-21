package fal

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dengyie/apihub/common"
	"github.com/dengyie/apihub/model"
	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeBaseURL(t *testing.T) {
	assert.Equal(t, "https://queue.fal.run", NormalizeBaseURL(""))
	assert.Equal(t, "https://queue.fal.run", NormalizeBaseURL("https://fal.run"))
	assert.Equal(t, "https://queue.fal.run", NormalizeBaseURL("https://fal.run/"))
	assert.Equal(t, "https://queue.fal.run", NormalizeBaseURL("http://fal.run"))
	assert.Equal(t, "https://queue.fal.run", NormalizeBaseURL("https://queue.fal.run/"))
	assert.Equal(t, "https://custom.fal.proxy/v1", NormalizeBaseURL("https://custom.fal.proxy/v1/"))
	assert.Equal(t, "https://fal.run/openrouter/router/openai/v1", NormalizeBaseURL("https://fal.run/fal-ai/any-llm/openai"))
	assert.Equal(t, "https://fal.run/openrouter/router/openai/v1", NormalizeBaseURL("https://fal.run/openrouter/router/openai"))
}

func TestBuildRequestURLMultiSlash(t *testing.T) {
	adaptor := &Adaptor{}
	info := &relaycommon.RelayInfo{
		OriginModelName: "fal-ai/kling-video/v3/pro/text-to-video",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl:    "https://queue.fal.run",
			UpstreamModelName: "fal-ai/kling-video/v3/pro/text-to-video",
		},
	}

	url, err := adaptor.GetRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, "https://queue.fal.run/fal-ai/kling-video/v3/pro/text-to-video", url)

	// Test OpenRouter endpoint
	openRouterInfo := &relaycommon.RelayInfo{
		OriginModelName: "openai/gpt-4o-mini",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl: "https://fal.run/openrouter/router/openai/v1",
		},
	}
	openRouterURL, err := adaptor.GetRequestURL(openRouterInfo)
	require.NoError(t, err)
	assert.Equal(t, "https://fal.run/openrouter/router/openai/v1/chat/completions", openRouterURL)

	taskAdaptor := &TaskAdaptor{}
	taskURL, err := taskAdaptor.BuildRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, "https://queue.fal.run/fal-ai/kling-video/v3/pro/text-to-video", taskURL)
}

func TestSetupRequestHeaderUsesKeyAuth(t *testing.T) {
	adaptor := &Adaptor{}
	header := make(http.Header)
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey: "test-fal-key-12345",
		},
	}

	err := adaptor.SetupRequestHeader(nil, &header, info)
	require.NoError(t, err)
	assert.Equal(t, "Key test-fal-key-12345", header.Get("Authorization"))
	assert.NotContains(t, header.Get("Authorization"), "Bearer")
	assert.Equal(t, "application/json", header.Get("Content-Type"))
	assert.Equal(t, "application/json", header.Get("Accept"))
}

func TestCleanOpenAIRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Test 1: text-to-video cleaning
	req1 := &dto.GeneralOpenAIRequest{
		Model: "fal-ai/kling-video/v3/pro/text-to-video",
		Messages: []dto.Message{
			{
				Role:    "user",
				Content: "A serene lake surrounded by autumn trees at sunset",
			},
		},
		Temperature: func(f float64) *float64 { return &f }(0.7),
		Stream:      func(b bool) *bool { return &b }(true),
	}

	cleaned1, err := CleanOpenAIRequest(nil, nil, req1)
	require.NoError(t, err)
	assert.Equal(t, "A serene lake surrounded by autumn trees at sunset", cleaned1["prompt"])
	assert.Nil(t, cleaned1["model"])
	assert.Nil(t, cleaned1["messages"])
	assert.Nil(t, cleaned1["temperature"])
	assert.Nil(t, cleaned1["stream"])

	// Test 2: multimodal image-to-video cleaning
	req2 := &dto.GeneralOpenAIRequest{
		Model: "fal-ai/kling-video/v3/pro/image-to-video",
		Messages: []dto.Message{
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type": "text",
						"text": "Make the waterfall flow smoothly",
					},
					map[string]any{
						"type": "image_url",
						"image_url": map[string]any{
							"url": "https://example.com/waterfall.png",
						},
					},
				},
			},
		},
	}

	cleaned2, err := CleanOpenAIRequest(nil, nil, req2)
	require.NoError(t, err)
	assert.Equal(t, "Make the waterfall flow smoothly", cleaned2["prompt"])
	assert.Equal(t, "https://example.com/waterfall.png", cleaned2["image_url"])
	assert.Nil(t, cleaned2["messages"])
}

func TestExtractVideoURLFormats(t *testing.T) {
	// Format 1: video.url
	body1 := []byte(`{"video": {"url": "https://v.fal.media/output1.mp4"}}`)
	url1, err := ExtractVideoURL(body1)
	require.NoError(t, err)
	assert.Equal(t, "https://v.fal.media/output1.mp4", url1)

	// Format 2: videos[0].url
	body2 := []byte(`{"videos": [{"url": "https://v.fal.media/output2.mp4"}]}`)
	url2, err := ExtractVideoURL(body2)
	require.NoError(t, err)
	assert.Equal(t, "https://v.fal.media/output2.mp4", url2)

	// Format 3: output.video.url
	body3 := []byte(`{"output": {"video": {"url": "https://v.fal.media/output3.mp4"}}}`)
	url3, err := ExtractVideoURL(body3)
	require.NoError(t, err)
	assert.Equal(t, "https://v.fal.media/output3.mp4", url3)

	// Format 4: output.videos[0].url
	body4 := []byte(`{"output": {"videos": [{"url": "https://v.fal.media/output4.mp4"}]}}`)
	url4, err := ExtractVideoURL(body4)
	require.NoError(t, err)
	assert.Equal(t, "https://v.fal.media/output4.mp4", url4)
}

type testRoundTripper func(req *http.Request) (*http.Response, error)

func (f testRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFalQueueEndToEndSimulation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pollCount := 0
	var receivedAuthHeader string
	baseURL := "https://queue.fal.run"

	oldTransport := http.DefaultTransport
	http.DefaultTransport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
		receivedAuthHeader = r.Header.Get("Authorization")

		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "fal-ai/kling-video"):
			// Task submission
			assert.Equal(t, "Key test-secret-key", receivedAuthHeader)
			resp := FalQueueSubmitResponse{
				RequestID:   "req_123456",
				StatusURL:   baseURL + "/fal-ai/kling-video/requests/req_123456/status",
				ResponseURL: baseURL + "/fal-ai/kling-video/requests/req_123456",
				Status:      "IN_QUEUE",
			}
			b, _ := common.Marshal(resp)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(b)),
				Header:     make(http.Header),
			}, nil

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/status"):
			// Polling status
			assert.Equal(t, "Key test-secret-key", receivedAuthHeader)
			pollCount++
			if pollCount < 2 {
				statusResp := FalQueueStatusResponse{
					Status: "IN_PROGRESS",
				}
				b, _ := common.Marshal(statusResp)
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(bytes.NewReader(b)),
					Header:     make(http.Header),
				}, nil
			}
			statusResp := FalQueueStatusResponse{
				Status:      "COMPLETED",
				ResponseURL: baseURL + "/fal-ai/kling-video/requests/req_123456",
			}
			b, _ := common.Marshal(statusResp)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(b)),
				Header:     make(http.Header),
			}, nil

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "req_123456"):
			// Fetch result
			assert.Equal(t, "Key test-secret-key", receivedAuthHeader)
			resultMap := map[string]any{
				"video": map[string]any{
					"url": "https://v.fal.media/rendered_video.mp4",
				},
			}
			b, _ := common.Marshal(resultMap)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(b)),
				Header:     make(http.Header),
			}, nil

		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("not found")),
				Header:     make(http.Header),
			}, nil
		}
	})
	defer func() { http.DefaultTransport = oldTransport }()

	// 1. Test Mode A (Task Engine)
	taskAdaptor := &TaskAdaptor{}
	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "fal-ai/kling-video/v3/pro/text-to-video",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl:    baseURL,
			ApiKey:            "test-secret-key",
			UpstreamModelName: "fal-ai/kling-video/v3/pro/text-to-video",
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	reqBody := `{"prompt": "A cat running on grass", "duration": 5}`
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(reqBody))

	// Estimate Billing
	assert.Nil(t, taskAdaptor.ValidateRequestAndSetAction(c, relayInfo))
	billingRatios := taskAdaptor.EstimateBilling(c, relayInfo)
	assert.Equal(t, 5.0, billingRatios["seconds"])

	// Submit
	bodyReader, err := taskAdaptor.BuildRequestBody(c, relayInfo)
	require.NoError(t, err)
	submitResp, err := taskAdaptor.DoRequest(c, relayInfo, bodyReader)
	require.NoError(t, err)

	parsedSubmit, taskErr := taskAdaptor.ParseResponse(c, submitResp, relayInfo)
	require.Nil(t, taskErr)
	assert.Equal(t, "req_123456", parsedSubmit.UpstreamTaskID)

	// Verify task data has verbatim status_url and response_url
	var savedData FalTaskData
	err = common.Unmarshal(parsedSubmit.TaskData, &savedData)
	require.NoError(t, err)
	assert.Equal(t, baseURL+"/fal-ai/kling-video/requests/req_123456/status", savedData.StatusURL)
	assert.Equal(t, baseURL+"/fal-ai/kling-video/requests/req_123456", savedData.ResponseURL)

	// Polling test with TaskAdaptor
	fakeTask := &model.Task{
		TaskID: "req_123456",
		Data:   parsedSubmit.TaskData,
		PrivateData: model.TaskPrivateData{
			Key: "test-secret-key",
		},
	}

	// Poll 1: IN_PROGRESS
	pollResp1, err := taskAdaptor.FetchTask(baseURL, "test-secret-key", fakeTask, "")
	require.NoError(t, err)
	b1, _ := io.ReadAll(pollResp1.Body)
	pollResp1.Body.Close()
	info1, err := taskAdaptor.ParseTaskResult(fakeTask, pollResp1, b1)
	require.NoError(t, err)
	assert.Equal(t, string(model.TaskStatusInProgress), info1.Status)

	// Poll 2: COMPLETED
	pollResp2, err := taskAdaptor.FetchTask(baseURL, "test-secret-key", fakeTask, "")
	require.NoError(t, err)
	b2, _ := io.ReadAll(pollResp2.Body)
	pollResp2.Body.Close()
	info2, err := taskAdaptor.ParseTaskResult(fakeTask, pollResp2, b2)
	require.NoError(t, err)
	assert.Equal(t, string(model.TaskStatusSuccess), info2.Status)
	assert.Equal(t, "https://v.fal.media/rendered_video.mp4", info2.Url)

	// ConvertToOpenAIVideo test
	fakeTask.Status = model.TaskStatusSuccess
	openAIVideoJSON, err := taskAdaptor.ConvertToOpenAIVideo(fakeTask)
	require.NoError(t, err)
	assert.Contains(t, string(openAIVideoJSON), "completed")

	// 2. Test Mode B (OpenAI Chat Completions Mode)
	adaptor := &Adaptor{}
	chatRelayInfo := &relaycommon.RelayInfo{
		OriginModelName: "fal-ai/kling-video/v3/pro/text-to-video",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl:    baseURL,
			ApiKey:            "test-secret-key",
			UpstreamModelName: "fal-ai/kling-video/v3/pro/text-to-video",
		},
	}

	chatW := httptest.NewRecorder()
	chatC, _ := gin.CreateTestContext(chatW)
	chatC.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	// Reset poll count for Mode B
	pollCount = 0
	submitReader := bytes.NewReader([]byte(`{"prompt": "A cat running on grass"}`))
	chatSubmitRespAny, err := adaptor.DoRequest(chatC, chatRelayInfo, submitReader)
	require.NoError(t, err)
	chatSubmitResp := chatSubmitRespAny.(*http.Response)

	usage, apiErr := adaptor.DoResponse(chatC, chatSubmitResp, chatRelayInfo)
	require.Nil(t, apiErr)
	assert.NotNil(t, usage)
	assert.Equal(t, http.StatusOK, chatW.Code)
	assert.Contains(t, chatW.Body.String(), "https://v.fal.media/rendered_video.mp4")
	assert.Contains(t, chatW.Body.String(), "video src=")
}

func TestFalQueueFailureAndErrorHandling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	baseURL := "https://queue.fal.run"

	oldTransport := http.DefaultTransport
	http.DefaultTransport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "failed-submit"):
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Body:       io.NopCloser(strings.NewReader(`{"detail": "Invalid duration"}`)),
				Header:     make(http.Header),
			}, nil

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "failed-status"):
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"status": "FAILED", "error": "GPU memory allocation failed"}`)),
				Header:     make(http.Header),
			}, nil

		default:
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(bytes.NewReader([]byte(`{
					"request_id": "req_fail",
					"status_url": "https://queue.fal.run/failed-status",
					"response_url": "https://queue.fal.run/failed-res"
				}`))),
				Header: make(http.Header),
			}, nil
		}
	})
	defer func() { http.DefaultTransport = oldTransport }()

	// Test 1: Task submit failure
	taskAdaptor := &TaskAdaptor{}
	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "fal-ai/kling-video/v3/pro/text-to-video",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl:    baseURL,
			ApiKey:            "test-secret-key",
			UpstreamModelName: "failed-submit",
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"prompt":"test"}`))
	require.Nil(t, taskAdaptor.ValidateRequestAndSetAction(c, relayInfo))
	bodyReader, _ := taskAdaptor.BuildRequestBody(c, relayInfo)
	submitResp, err := taskAdaptor.DoRequest(c, relayInfo, bodyReader)
	require.NoError(t, err)
	_, taskErr := taskAdaptor.ParseResponse(c, submitResp, relayInfo)
	require.NotNil(t, taskErr)
	assert.Contains(t, taskErr.Message, "Invalid duration")

	// Test 2: Polling status failure
	fakeTask := &model.Task{
		TaskID: "req_fail",
		Data:   []byte(`{"request_id":"req_fail","status_url":"https://queue.fal.run/failed-status"}`),
	}
	pollResp, err := taskAdaptor.FetchTask(baseURL, "key", fakeTask, "")
	require.NoError(t, err)
	b, _ := io.ReadAll(pollResp.Body)
	pollResp.Body.Close()
	info, err := taskAdaptor.ParseTaskResult(fakeTask, pollResp, b)
	require.NoError(t, err)
	assert.Equal(t, string(model.TaskStatusFailure), info.Status)
	assert.Contains(t, info.Reason, "GPU memory allocation failed")

	// Test 3: Chat completion mode failure returns error for billing refund
	adaptor := &Adaptor{}
	chatRelayInfo := &relaycommon.RelayInfo{
		OriginModelName: "fal-ai/kling-video/v3/pro/text-to-video",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl:    baseURL,
			ApiKey:            "test-secret-key",
			UpstreamModelName: "fal-ai/kling-video/v3/pro/text-to-video",
		},
	}
	chatW := httptest.NewRecorder()
	chatC, _ := gin.CreateTestContext(chatW)
	chatC.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	chatRespAny, err := adaptor.DoRequest(chatC, chatRelayInfo, strings.NewReader(`{"prompt":"test"}`))
	require.NoError(t, err)
	_, apiErr := adaptor.DoResponse(chatC, chatRespAny.(*http.Response), chatRelayInfo)
	require.NotNil(t, apiErr)
	assert.Contains(t, apiErr.Error(), "GPU memory allocation failed")
}

func TestFalStreamSSEOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	baseURL := "https://queue.fal.run"

	oldTransport := http.DefaultTransport
	http.DefaultTransport = testRoundTripper(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodPost:
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(bytes.NewReader([]byte(`{
					"request_id": "req_stream",
					"status_url": "https://queue.fal.run/stream-status",
					"response_url": "https://queue.fal.run/stream-result"
				}`))),
				Header: make(http.Header),
			}, nil
		case strings.HasSuffix(r.URL.Path, "stream-status"):
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(bytes.NewReader([]byte(`{
					"status": "COMPLETED",
					"response_url": "https://queue.fal.run/stream-result"
				}`))),
				Header: make(http.Header),
			}, nil
		case strings.HasSuffix(r.URL.Path, "stream-result"):
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(bytes.NewReader([]byte(`{
					"video": {"url": "https://v.fal.media/stream_out.mp4"}
				}`))),
				Header: make(http.Header),
			}, nil
		default:
			return &http.Response{StatusCode: http.StatusNotFound}, nil
		}
	})
	defer func() { http.DefaultTransport = oldTransport }()

	adaptor := &Adaptor{}
	info := &relaycommon.RelayInfo{
		IsStream:        true,
		OriginModelName: "fal-ai/kling-video/v3/pro/text-to-video",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl:    baseURL,
			ApiKey:            "test-secret-key",
			UpstreamModelName: "fal-ai/kling-video/v3/pro/text-to-video",
		},
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	respAny, err := adaptor.DoRequest(c, info, strings.NewReader(`{"prompt":"test"}`))
	require.NoError(t, err)

	usage, apiErr := adaptor.DoResponse(c, respAny.(*http.Response), info)
	require.Nil(t, apiErr)
	assert.NotNil(t, usage)

	body := w.Body.String()
	assert.Contains(t, body, "data: ")
	assert.Contains(t, body, "stream_out.mp4")
	assert.Contains(t, body, "data: [DONE]")
}

func BenchmarkCleanOpenAIRequest(b *testing.B) {
	req := &dto.GeneralOpenAIRequest{
		Model: "fal-ai/kling-video/v3/pro/text-to-video",
		Messages: []dto.Message{
			{Role: "system", Content: "You are a video generator."},
			{Role: "user", Content: "Generate a futuristic cyberpunk city at night with neon lights."},
		},
		Temperature: func(f float64) *float64 { return &f }(0.7),
		Stream:      func(b bool) *bool { return &b }(true),
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := CleanOpenAIRequest(nil, nil, req)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExtractVideoURL(b *testing.B) {
	body := []byte(`{"video": {"url": "https://v.fal.media/rendered_video.mp4"}, "seed": 42}`)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, err := ExtractVideoURL(body)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseTaskResultPolling(b *testing.B) {
	taskAdaptor := &TaskAdaptor{}
	fakeTask := &model.Task{
		TaskID: "req_bench",
		Data:   []byte(`{"request_id":"req_bench","status_url":"https://queue.fal.run/status"}`),
	}
	resp := &http.Response{StatusCode: http.StatusOK}
	statusBody := []byte(`{"status":"IN_PROGRESS","queue_position":3,"logs":[{"message":"rendering frame 120/300"}],"metrics":{"gpu_utilization":0.95}}`)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		info, err := taskAdaptor.ParseTaskResult(fakeTask, resp, statusBody)
		if err != nil || info.Status != string(model.TaskStatusInProgress) {
			b.Fatal(err)
		}
	}
}
