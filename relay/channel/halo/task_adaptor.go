package halo

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/dengyie/apihub/common"
	"github.com/dengyie/apihub/constant"
	taskdto "github.com/dengyie/apihub/dto"
	"github.com/dengyie/apihub/model"
	"github.com/dengyie/apihub/relay/channel"
	relaycommon "github.com/dengyie/apihub/relay/common"
	relaykitdto "github.com/dengyie/apihub/relaykit/dto"
	"github.com/dengyie/apihub/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// TaskAdaptor implements the halo.video conversation workflow on the task
// engine. One Submit call fans out into three upstream requests (preview →
// conversation → message); polling is a plain GET per round.
type TaskAdaptor struct {
	cachedRawBody []byte
	cachedAPIKey  string
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
	if info != nil && info.ChannelMeta != nil && info.ChannelMeta.ApiKey != "" {
		a.cachedAPIKey = info.ChannelMeta.ApiKey
	}
}

func (a *TaskAdaptor) GetModelList() []string { return ModelList }

func (a *TaskAdaptor) GetChannelName() string { return ChannelName }

func (a *TaskAdaptor) ValidateRequestAndSetAction(c *gin.Context, info *relaycommon.RelayInfo) *taskdto.TaskError {
	if c == nil || c.Request == nil {
		return service.TaskErrorWrapperLocal(errors.New("request context is nil"), "invalid_request", http.StatusBadRequest)
	}

	if info != nil {
		if info.ChannelMeta == nil {
			info.ChannelMeta = &relaycommon.ChannelMeta{}
		}
		if info.TaskRelayInfo == nil {
			info.TaskRelayInfo = &relaycommon.TaskRelayInfo{}
		}
	}

	var bodyBytes []byte
	if c.Request.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(io.LimitReader(c.Request.Body, 10<<20))
		if err != nil {
			return service.TaskErrorWrapperLocal(err, "read_request_body_failed", http.StatusBadRequest)
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	} else if storage, err := common.GetBodyStorage(c); err == nil && storage != nil {
		bodyBytes, _ = storage.Bytes()
	}

	a.cachedRawBody = bodyBytes

	if len(bodyBytes) > 0 {
		res := gjson.ParseBytes(bodyBytes)
		if info.OriginModelName == "" {
			if m := res.Get("model").String(); m != "" {
				info.OriginModelName = m
				info.UpstreamModelName = m
			}
		}
		// Duration drives per-second pre-charge; reject absurd values before
		// any quota multiplication happens (upstream products cap at ~10s).
		if d := res.Get("duration"); d.Exists() && (d.Float() <= 0 || d.Float() > MaxDurationSeconds) {
			return service.TaskErrorWrapperLocal(
				fmt.Errorf("halo adaptor: duration must be within (0, %v] seconds", MaxDurationSeconds),
				"invalid_request", http.StatusBadRequest)
		}
		if s := res.Get("seconds"); !res.Get("duration").Exists() && s.Exists() && (s.Float() <= 0 || s.Float() > MaxDurationSeconds) {
			return service.TaskErrorWrapperLocal(
				fmt.Errorf("halo adaptor: seconds must be within (0, %v]", MaxDurationSeconds),
				"invalid_request", http.StatusBadRequest)
		}
	}

	if info != nil && info.Action == "" {
		info.Action = ActionForModel(info.OriginModelName, bodyBytes)
	}

	return nil
}

// ActionForModel maps the halo model name onto the task engine's action
// vocabulary. Image-conditioned modes are detected from the model suffix or
// the presence of an image reference in the request.
func ActionForModel(modelName string, body []byte) string {
	switch modelName {
	case "halo-video-i2v", "halo-video-fl2v", "halo-video-ref2v":
		return constant.TaskActionImageToVideo
	case "halo-video-edit":
		return constant.TaskActionRemix
	case "halo-image":
		return constant.TaskActionTextToVideo
	}
	if len(body) > 0 {
		res := gjson.ParseBytes(body)
		if res.Get("image_url").Exists() || res.Get("image").Exists() || res.Get("input_reference").Exists() {
			return constant.TaskActionImageToVideo
		}
	}
	return constant.TaskActionTextToVideo
}

func (a *TaskAdaptor) EstimateBilling(c *gin.Context, info *relaycommon.RelayInfo) map[string]float64 {
	if len(a.cachedRawBody) == 0 && c != nil && c.Request != nil && c.Request.Body != nil {
		a.cachedRawBody, _ = io.ReadAll(c.Request.Body)
		c.Request.Body = io.NopCloser(bytes.NewReader(a.cachedRawBody))
	}
	if len(a.cachedRawBody) == 0 {
		return nil
	}
	res := gjson.ParseBytes(a.cachedRawBody)
	var duration float64
	if d := res.Get("duration"); d.Exists() {
		duration = d.Float()
	} else if d := res.Get("seconds"); d.Exists() {
		duration = d.Float()
	}
	if duration > 0 {
		return map[string]float64{"seconds": duration}
	}
	return nil
}

func (a *TaskAdaptor) AdjustBillingOnSubmit(info *relaycommon.RelayInfo, taskData []byte) map[string]float64 {
	return nil
}

func (a *TaskAdaptor) AdjustBillingOnComplete(task *model.Task, taskResult *relaycommon.TaskInfo) int {
	return 0
}

// BuildRequestURL returns the /api/v1 root; DoRequest walks the full
// preview → conversation → message chain beneath it.
func (a *TaskAdaptor) BuildRequestURL(info *relaycommon.RelayInfo) (string, error) {
	if info == nil {
		return "", errors.New("relay info is nil")
	}
	baseURL := ""
	if info.ChannelMeta != nil {
		baseURL = info.ChannelBaseUrl
	}
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(constant.ChannelTypeHalo)
	}
	baseURL = NormalizeBaseURL(baseURL)
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return baseURL + "/api/v1", nil
}

func (a *TaskAdaptor) BuildRequestHeader(c *gin.Context, req *http.Request, info *relaycommon.RelayInfo) error {
	if info == nil {
		return errors.New("relay info is nil")
	}
	if info.ApiKey == "" {
		return errors.New("halo api key is required")
	}
	req.Header.Set("Authorization", "Bearer "+info.ApiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return nil
}

// doUpstream performs one JSON round trip with the channel proxy applied.
func (a *TaskAdaptor) doUpstream(c *gin.Context, info *relaycommon.RelayInfo, method, url string, body []byte, limit int64) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), method, url, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("halo adaptor create request failed: %w", err)
	}
	if err := a.BuildRequestHeader(c, req, info); err != nil {
		return nil, 0, err
	}

	client := service.GetHttpClient()
	if client == nil {
		client = http.DefaultClient
	}
	if info != nil && info.ChannelSetting.Proxy != "" {
		proxyClient, proxyErr := service.GetHttpClientWithProxy(info.ChannelSetting.Proxy)
		if proxyErr != nil || proxyClient == nil {
			// Proxy misconfig must not silently degrade to direct egress: for
			// channels whose upstream is only reachable via the proxy pool a
			// silent fallback turns every request into a confusing timeout.
			common.SysLog(fmt.Sprintf("halo adaptor: proxy client unavailable (proxy=%s): %v — falling back to direct egress", info.ChannelSetting.Proxy, proxyErr))
		} else {
			client = proxyClient
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return respBody, resp.StatusCode, nil
}

// BuildRequestBody returns the cached client body; the real upstream payload
// chain is assembled in DoRequest.
func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	raw := a.cachedRawBody
	if len(raw) == 0 && c != nil && c.Request != nil && c.Request.Body != nil {
		var err error
		raw, err = io.ReadAll(io.LimitReader(c.Request.Body, 10<<20))
		if err != nil {
			return nil, err
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		a.cachedRawBody = raw
	}
	if len(raw) == 0 {
		return nil, errors.New("empty request body")
	}
	return bytes.NewReader(raw), nil
}

// DoRequest executes the halo submit chain:
//  1. POST /generation/preview with the quoted parameters → quote_token
//  2. POST /conversations with a derived idempotency key → conversation id
//  3. POST /conversations/{id}/messages with content+parameters+quote_token
//
// Any failing step aborts submission before credits are committed (preview is
// free; the message submit is the charged call).
func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	raw, err := io.ReadAll(io.LimitReader(requestBody, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("halo adaptor read body failed: %w", err)
	}
	a.cachedRawBody = raw

	apiRoot, err := a.BuildRequestURL(info)
	if err != nil {
		return nil, err
	}

	modelName := strings.TrimSpace(info.UpstreamModelName)
	if modelName == "" {
		modelName = strings.TrimSpace(info.OriginModelName)
	}

	payload, err := BuildGenerationPayload(raw)
	if err != nil {
		return nil, err
	}
	SetInputMode(payload, ModeForModel(modelName))

	// Step 1: free quote (quote_token binds to these parameters).
	quoteReq := map[string]any{
		"parameters":      QuoteParameters(payload),
		"candidate_count": 1,
	}
	if _, ok := payload["image_url"]; ok {
		// image-conditioned modes carry the reference via the message body;
		// the preview call only needs parameters.
		_ = ok
	}
	quoteBody, err := common.Marshal(quoteReq)
	if err != nil {
		return nil, err
	}
	quoteRespBody, status, err := a.doUpstream(c, info, http.MethodPost, apiRoot+"/generation/preview", quoteBody, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("halo preview request failed: %w", err)
	}
	if status >= 400 {
		return nil, fmt.Errorf("halo preview failed (%d): %s", status, ExtractErrorMsg(quoteRespBody))
	}
	quoteToken := gjson.GetBytes(quoteRespBody, "quote_token").String()
	available := gjson.GetBytes(quoteRespBody, "available")
	if quoteToken == "" {
		return nil, fmt.Errorf("halo preview returned no quote_token: %s", ExtractErrorMsg(quoteRespBody))
	}
	if available.Exists() && !available.Bool() {
		return nil, fmt.Errorf("halo quote unavailable: %s", ExtractErrorMsg(quoteRespBody))
	}

	// Step 2: create conversation with a deterministic idempotency key. The
	// gateway-generated PublicTaskID survives channel retries within one
	// logical request, so a retried submit reuses the same conversation
	// instead of creating a second charged one. Client-supplied keys win.
	idempotencyKey := strings.TrimSpace(gjson.GetBytes(raw, "idempotency_key").String())
	if idempotencyKey == "" && info != nil && info.TaskRelayInfo != nil {
		idempotencyKey = strings.TrimSpace(info.PublicTaskID)
	}
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("t%d", common.GetTimestamp())
	}
	convBody, err := common.Marshal(map[string]any{
		"idempotency_key": "mangoapi-" + idempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	convRespBody, status, err := a.doUpstream(c, info, http.MethodPost, apiRoot+"/conversations", convBody, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("halo conversation request failed: %w", err)
	}
	if status >= 400 {
		return nil, fmt.Errorf("halo conversation failed (%d): %s", status, ExtractErrorMsg(convRespBody))
	}
	conversationID := gjson.GetBytes(convRespBody, "id").String()
	if conversationID == "" {
		return nil, fmt.Errorf("halo conversation returned no id: %s", ExtractErrorMsg(convRespBody))
	}

	// Step 3: the charged submit.
	msgPayload := map[string]any{}
	for k, v := range payload {
		msgPayload[k] = v
	}
	msgPayload["quote_token"] = quoteToken
	msgBody, err := common.Marshal(msgPayload)
	if err != nil {
		return nil, err
	}
	msgRespBody, status, err := a.doUpstream(c, info, http.MethodPost,
		apiRoot+"/conversations/"+conversationID+"/messages", msgBody, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("halo message request failed: %w", err)
	}

	// Wrap the submit response in a synthetic http.Response so ParseResponse
	// keeps the standard signature and error handling.
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(msgRespBody)),
		Header:     header,
		Request:    c.Request,
	}, nil
}

func (a *TaskAdaptor) ParseResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*channel.TaskSubmitResponse, *taskdto.TaskError) {
	if resp == nil {
		return nil, service.TaskErrorWrapperLocal(errors.New("halo submit response is nil"), "upstream_response_nil", http.StatusBadGateway)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "read_upstream_response_failed", http.StatusBadGateway)
	}
	if resp.StatusCode >= 400 {
		return nil, service.TaskErrorWrapper(errors.New(ExtractErrorMsg(body)), "upstream_submit_failed", resp.StatusCode)
	}

	jobID, err := ExtractJobID(body)
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "missing_job_id", http.StatusBadGateway)
	}

	taskData := TaskData{
		JobID:     jobID,
		MediaPath: fmt.Sprintf("/api/v1/jobs/%s/media", jobID),
	}
	return &channel.TaskSubmitResponse{
		UpstreamTaskID: jobID,
		TaskData:       taskData.Marshal(),
		PluginState:    taskData.Marshal(),
	}, nil
}

// FetchTask long-polls the job endpoint. wait_seconds keeps each poll
// productive without hammering the upstream; the task engine's own interval
// still governs round cadence.
func (a *TaskAdaptor) FetchTask(baseURL, key string, task *model.Task, proxy string) (*http.Response, error) {
	if task == nil {
		return nil, errors.New("task is nil")
	}

	var taskData TaskData
	if len(task.PrivateData.PluginState) > 0 {
		_ = common.Unmarshal(task.PrivateData.PluginState, &taskData)
	}
	if taskData.JobID == "" {
		_ = task.GetData(&taskData)
	}
	if taskData.JobID == "" {
		return nil, errors.New("halo task state missing job_id")
	}

	root := NormalizeBaseURL(baseURL)
	if root == "" {
		root = DefaultBaseURL
	}

	req, err := http.NewRequest(http.MethodGet, root+"/api/v1/jobs/"+taskData.JobID+"?wait_seconds=45", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create halo status request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")

	client := service.GetHttpClient()
	if client == nil {
		client = http.DefaultClient
	}
	if proxy != "" {
		proxyClient, proxyErr := service.GetHttpClientWithProxy(proxy)
		if proxyErr != nil || proxyClient == nil {
			common.SysLog(fmt.Sprintf("halo adaptor: poll proxy client unavailable (proxy=%s): %v — falling back to direct egress", proxy, proxyErr))
		} else {
			client = proxyClient
		}
	}
	return client.Do(req)
}

// ParseTaskResult maps halo job states onto the task engine. On success the
// media path is stored and the gateway-relative content proxy URL is returned
// (the raw media URL requires our Bearer key and must never leak to clients).
func (a *TaskAdaptor) ParseTaskResult(task *model.Task, resp *http.Response, respBody []byte) (*relaycommon.TaskInfo, error) {
	if resp == nil {
		return nil, errors.New("halo poll response is nil")
	}
	if resp.StatusCode >= 400 {
		return &relaycommon.TaskInfo{
			Status: string(model.TaskStatusFailure),
			Reason: ExtractErrorMsg(respBody),
		}, nil
	}

	var taskData TaskData
	if len(task.PrivateData.PluginState) > 0 {
		_ = common.Unmarshal(task.PrivateData.PluginState, &taskData)
	}
	if taskData.JobID == "" {
		_ = task.GetData(&taskData)
	}

	status := gjson.GetBytes(respBody, "status").String()

	switch status {
	case "succeeded":
		mediaPath := gjson.GetBytes(respBody, "download_url").String()
		if mediaPath == "" {
			mediaPath = gjson.GetBytes(respBody, "video_url").String()
		}
		if mediaPath == "" {
			mediaPath = fmt.Sprintf("/api/v1/jobs/%s/media", taskData.JobID)
		}
		taskData.MediaPath = mediaPath
		taskDataBytes := taskData.Marshal()
		task.SetData(taskData)
		return &relaycommon.TaskInfo{
			Status:      string(model.TaskStatusSuccess),
			Progress:    "100%",
			Url:         ContentProxyURL(task.TaskID),
			PluginState: taskDataBytes,
		}, nil
	case "failed":
		reason := ExtractErrorMsg(respBody)
		return &relaycommon.TaskInfo{
			Status: string(model.TaskStatusFailure),
			Reason: reason,
		}, nil
	case "running":
		progress := "50%"
		if p := gjson.GetBytes(respBody, "progress.stage"); p.Exists() && p.String() != "" {
			progress = p.String()
		}
		return &relaycommon.TaskInfo{
			Status:      string(model.TaskStatusInProgress),
			Progress:    progress,
			PluginState: taskData.Marshal(),
		}, nil
	case "queued", "":
		return &relaycommon.TaskInfo{
			Status:      string(model.TaskStatusQueued),
			Progress:    "10%",
			PluginState: taskData.Marshal(),
		}, nil
	default:
		return &relaycommon.TaskInfo{
			Status:      string(model.TaskStatusInProgress),
			Progress:    "30%",
			PluginState: taskData.Marshal(),
		}, nil
	}
}

// ContentProxyURL routes clients at the gateway's own media endpoint; the
// content proxy then fetches the upstream media path with the channel key.
func ContentProxyURL(taskID string) string {
	return fmt.Sprintf("/v1/videos/%s/content", taskID)
}

// BuildContentRequest serves the final video through the content proxy:
// halo's media endpoint requires the channel Bearer key, which must not be
// forwarded to clients, so the proxy injects it here. The key is resolved
// per-call from the channel cache (falling back to task.PrivateData.Key) —
// never from shared adaptor state, since the polling/content paths may reuse
// one adaptor instance across tasks and channels.
func (a *TaskAdaptor) BuildContentRequest(task *model.Task, artifactKey string, clientRequest channel.TaskArtifactClientRequest) (*channel.TaskContentRequest, error) {
	if task == nil {
		return nil, errors.New("task is nil")
	}
	var taskData TaskData
	if len(task.PrivateData.PluginState) > 0 {
		_ = common.Unmarshal(task.PrivateData.PluginState, &taskData)
	}
	if taskData.JobID == "" {
		_ = task.GetData(&taskData)
	}
	if taskData.JobID == "" {
		return nil, errors.New("halo task state missing job_id")
	}

	apiKey := task.PrivateData.Key
	baseURL := ""
	if channelModel, err := model.CacheGetChannel(task.ChannelId); err == nil && channelModel != nil {
		baseURL = channelModel.GetBaseURL()
		if apiKey == "" {
			apiKey = channelModel.Key
		}
	}
	if apiKey == "" {
		return nil, errors.New("halo channel key unavailable for content proxy")
	}
	root := NormalizeBaseURL(baseURL)
	if root == "" {
		root = DefaultBaseURL
	}

	mediaURL := root + "/api/v1/jobs/" + taskData.JobID + "/media"
	headers := map[string]string{
		"Authorization": "Bearer " + apiKey,
	}
	for k, v := range clientRequest.Headers {
		headers[k] = v
	}

	return &channel.TaskContentRequest{
		URL:     mediaURL,
		Method:  clientRequest.Method,
		Headers: headers,
	}, nil
}

func (a *TaskAdaptor) ConvertToOpenAIVideo(originTask *model.Task) ([]byte, error) {
	if originTask == nil {
		return nil, errors.New("origin task is nil")
	}
	openAIVideo := originTask.ToOpenAIVideo()
	if originTask.FailReason != "" {
		openAIVideo.Error = &relaykitdto.OpenAIVideoError{
			Code:    "task_failed",
			Message: originTask.FailReason,
		}
	}
	return common.Marshal(openAIVideo)
}

var _ channel.TaskAdaptor = (*TaskAdaptor)(nil)
var _ channel.OpenAIVideoConverter = (*TaskAdaptor)(nil)
var _ channel.TaskContentRequestProvider = (*TaskAdaptor)(nil)
