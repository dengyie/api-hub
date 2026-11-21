package fal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dengyie/apihub/common"
	"github.com/dengyie/apihub/constant"
	"github.com/dengyie/apihub/relay/channel"
	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relaykit/dto"
	"github.com/dengyie/apihub/relaykit/types"
	"github.com/dengyie/apihub/service"
	"github.com/gin-gonic/gin"
)

type Adaptor struct{}

func (a *Adaptor) Init(info *relaycommon.RelayInfo) {}

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	if info == nil {
		return "", errors.New("relay info is nil")
	}
	baseURL := info.ChannelBaseUrl
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(constant.ChannelTypeFal)
	}
	baseURL = NormalizeBaseURL(baseURL)

	var modelName string
	if info.ChannelMeta != nil {
		modelName = strings.TrimSpace(info.UpstreamModelName)
	}
	if modelName == "" {
		modelName = strings.TrimSpace(info.OriginModelName)
	}
	if modelName == "" {
		return "", errors.New("model is required")
	}

	return strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(modelName, "/"), nil
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, req *http.Header, info *relaycommon.RelayInfo) error {
	if info == nil {
		return errors.New("relay info is nil")
	}
	if info.ApiKey == "" {
		return errors.New("fal api key is required")
	}
	if c != nil && c.Request != nil {
		channel.SetupApiRequestHeader(info, c, req)
	}
	// Critical: Fal.ai requires "Key <API_KEY>", never "Bearer"
	req.Set("Authorization", "Key "+info.ApiKey)
	if req.Get("Content-Type") == "" {
		req.Set("Content-Type", "application/json")
	}
	if req.Get("Accept") == "" {
		req.Set("Accept", "application/json")
	}
	return nil
}

func (a *Adaptor) ConvertOpenAIRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) (any, error) {
	return CleanOpenAIRequest(c, info, request)
}

func (a *Adaptor) ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ClaudeRequest) (any, error) {
	return nil, errors.New("fal adaptor does not support claude requests")
}

func (a *Adaptor) ConvertGeminiRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.GeminiChatRequest) (any, error) {
	return nil, errors.New("fal adaptor does not support gemini requests")
}

func (a *Adaptor) ConvertImageRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.ImageRequest) (any, error) {
	return nil, errors.New("fal adaptor does not support direct image requests")
}

func (a *Adaptor) ConvertRerankRequest(c *gin.Context, relayMode int, request dto.RerankRequest) (any, error) {
	return nil, errors.New("fal adaptor does not support rerank requests")
}

func (a *Adaptor) ConvertEmbeddingRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.EmbeddingRequest) (any, error) {
	return nil, errors.New("fal adaptor does not support embedding requests")
}

func (a *Adaptor) ConvertAudioRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.AudioRequest) (io.Reader, error) {
	return nil, errors.New("fal adaptor does not support audio requests")
}

func (a *Adaptor) ConvertOpenAIResponsesRequest(c *gin.Context, info *relaycommon.RelayInfo, request dto.OpenAIResponsesRequest) (any, error) {
	return nil, errors.New("fal adaptor does not support responses requests")
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (any, error) {
	url, err := a.GetRequestURL(info)
	if err != nil {
		return nil, fmt.Errorf("fal adaptor get request url failed: %w", err)
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, url, requestBody)
	if err != nil {
		return nil, fmt.Errorf("fal adaptor create request failed: %w", err)
	}
	channel.ApplyUpstreamBodyMetadata(req, requestBody)

	header := req.Header
	if err := a.SetupRequestHeader(c, &header, info); err != nil {
		return nil, fmt.Errorf("fal adaptor setup request header failed: %w", err)
	}
	req.Header = header

	client := service.GetHttpClient()
	if client == nil {
		client = http.DefaultClient
	}
	if info != nil && info.ChannelSetting.Proxy != "" {
		if proxyClient, err := service.GetHttpClientWithProxy(info.ChannelSetting.Proxy); err == nil && proxyClient != nil {
			client = proxyClient
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fal adaptor do request failed: %w", err)
	}
	return resp, nil
}

func (a *Adaptor) DoResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (usage any, err *types.NewAPIError) {
	if resp == nil {
		return nil, types.NewError(errors.New("upstream response is nil"), types.ErrorCodeBadResponse)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, types.NewError(fmt.Errorf("failed to read fal submit response: %w", readErr), types.ErrorCodeBadResponse)
	}

	if resp.StatusCode >= 400 {
		return nil, types.NewError(errors.New(ExtractErrorMsg(body)), types.ErrorCodeBadResponse)
	}

	var submitResp FalQueueSubmitResponse
	if unmarshalErr := common.Unmarshal(body, &submitResp); unmarshalErr != nil {
		return nil, types.NewError(fmt.Errorf("failed to parse fal submit response: %w", unmarshalErr), types.ErrorCodeBadResponse)
	}

	if submitResp.StatusURL == "" {
		return nil, types.NewError(errors.New("fal upstream response missing status_url"), types.ErrorCodeBadResponse)
	}
	responseURL := submitResp.ResponseURL

	// Mode B Chat Completion Polling loop
	client := service.GetHttpClient()
	if client == nil {
		client = http.DefaultClient
	}
	if info != nil && info.ChannelSetting.Proxy != "" {
		if proxyClient, pErr := service.GetHttpClientWithProxy(info.ChannelSetting.Proxy); pErr == nil && proxyClient != nil {
			client = proxyClient
		}
	}

	if info.IsStream {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(http.StatusOK)
		c.Writer.Flush()
	}

	maxWait := 5 * time.Minute
	ctx, cancel := context.WithTimeout(c.Request.Context(), maxWait)
	defer cancel()

	pollTicker := time.NewTicker(2 * time.Second)
	defer pollTicker.Stop()

	heartbeatTicker := time.NewTicker(10 * time.Second)
	defer heartbeatTicker.Stop()

	completed := false
	var finalFailReason string

	for !completed {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, types.NewError(errors.New("fal video generation timed out after 5 minutes"), types.ErrorCodeBadResponse)
			}
			return nil, types.NewError(ctx.Err(), types.ErrorCodeClientAborted)

		case <-heartbeatTicker.C:
			if info.IsStream {
				_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
				c.Writer.Flush()
			}

		case <-pollTicker.C:
			// Verbatim status_url must be used
			statusReq, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, submitResp.StatusURL, nil)
			if reqErr != nil {
				continue
			}
			statusReq.Header.Set("Authorization", "Key "+info.ApiKey)
			statusReq.Header.Set("Accept", "application/json")

			statusResp, doErr := client.Do(statusReq)
			if doErr != nil {
				continue
			}

			statusBody, sReadErr := io.ReadAll(statusResp.Body)
			statusResp.Body.Close()
			if sReadErr != nil || statusResp.StatusCode >= 400 {
				continue
			}

			var queueStatus FalQueueStatusResponse
			if jErr := common.Unmarshal(statusBody, &queueStatus); jErr != nil {
				continue
			}

			if queueStatus.ResponseURL != "" {
				responseURL = queueStatus.ResponseURL
			}

			if strings.EqualFold(queueStatus.Status, "COMPLETED") {
				completed = true
				break
			}
			if strings.EqualFold(queueStatus.Status, "FAILED") {
				finalFailReason = ExtractErrorMsg(statusBody)
				if finalFailReason == "" {
					finalFailReason = "upstream fal video task failed"
				}
				return nil, types.NewError(errors.New(finalFailReason), types.ErrorCodeBadResponse)
			}
		}
	}

	if responseURL == "" {
		return nil, types.NewError(errors.New("fal upstream completed but missing response_url"), types.ErrorCodeBadResponse)
	}

	// Fetch final result from verbatim response_url
	respReq, rErr := http.NewRequestWithContext(ctx, http.MethodGet, responseURL, nil)
	if rErr != nil {
		return nil, types.NewError(fmt.Errorf("failed to create fal result request: %w", rErr), types.ErrorCodeBadResponse)
	}
	respReq.Header.Set("Authorization", "Key "+info.ApiKey)
	respReq.Header.Set("Accept", "application/json")

	resultResp, rDoErr := client.Do(respReq)
	if rDoErr != nil {
		return nil, types.NewError(fmt.Errorf("failed to fetch fal final result: %w", rDoErr), types.ErrorCodeBadResponse)
	}
	defer resultResp.Body.Close()

	resultBody, rReadErr := io.ReadAll(resultResp.Body)
	if rReadErr != nil {
		return nil, types.NewError(fmt.Errorf("failed to read fal final result: %w", rReadErr), types.ErrorCodeBadResponse)
	}
	if resultResp.StatusCode >= 400 {
		return nil, types.NewError(errors.New(ExtractErrorMsg(resultBody)), types.ErrorCodeBadResponse)
	}

	videoURL, vErr := ExtractVideoURL(resultBody)
	if vErr != nil {
		return nil, types.NewError(fmt.Errorf("fal result parsing error: %w", vErr), types.ErrorCodeBadResponse)
	}

	// Render output as Markdown link and <video> tag
	markdownOutput := fmt.Sprintf("[Click to watch video](%s)\n\n<video src=\"%s\" controls=\"controls\" width=\"100%%\"></video>", videoURL, videoURL)

	chatUsage := &dto.Usage{
		PromptTokens:     1,
		CompletionTokens: 1,
		TotalTokens:      2,
	}

	chatCompletionID := "chatcmpl-" + common.GetUUID()
	createdTime := time.Now().Unix()

	if info.IsStream {
		// Send content delta chunk
		chunkContent := gin.H{
			"id":      chatCompletionID,
			"object":  "chat.completion.chunk",
			"created": createdTime,
			"model":   info.OriginModelName,
			"choices": []gin.H{
				{
					"index": 0,
					"delta": gin.H{
						"role":    "assistant",
						"content": markdownOutput,
					},
					"finish_reason": nil,
				},
			},
		}
		chunkContentBytes, _ := common.Marshal(chunkContent)
		_, _ = c.Writer.Write([]byte(fmt.Sprintf("data: %s\n\n", chunkContentBytes)))

		// Send finish chunk
		chunkFinish := gin.H{
			"id":      chatCompletionID,
			"object":  "chat.completion.chunk",
			"created": createdTime,
			"model":   info.OriginModelName,
			"choices": []gin.H{
				{
					"index":         0,
					"delta":         gin.H{},
					"finish_reason": "stop",
				},
			},
		}
		chunkFinishBytes, _ := common.Marshal(chunkFinish)
		_, _ = c.Writer.Write([]byte(fmt.Sprintf("data: %s\n\n", chunkFinishBytes)))
		_, _ = c.Writer.Write([]byte("data: [DONE]\n\n"))
		c.Writer.Flush()
	} else {
		c.JSON(http.StatusOK, gin.H{
			"id":      chatCompletionID,
			"object":  "chat.completion",
			"created": createdTime,
			"model":   info.OriginModelName,
			"choices": []gin.H{
				{
					"index": 0,
					"message": gin.H{
						"role":    "assistant",
						"content": markdownOutput,
					},
					"finish_reason": "stop",
				},
			},
			"usage": chatUsage,
		})
	}

	return chatUsage, nil
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}
