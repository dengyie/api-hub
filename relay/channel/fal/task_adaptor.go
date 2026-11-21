package fal

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

type TaskAdaptor struct {
	cachedRawBody []byte
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {}

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
		bodyBytes, err = io.ReadAll(c.Request.Body)
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

		if info.Action == "" {
			if res.Get("image_url").Exists() || res.Get("image").Exists() || res.Get("input_reference").Exists() {
				info.Action = constant.TaskActionImageToVideo
			} else {
				info.Action = constant.TaskActionTextToVideo
			}
		}
	}

	return nil
}

func (a *TaskAdaptor) EstimateBilling(c *gin.Context, info *relaycommon.RelayInfo) map[string]float64 {
	if len(a.cachedRawBody) == 0 && c != nil && c.Request != nil && c.Request.Body != nil {
		a.cachedRawBody, _ = io.ReadAll(c.Request.Body)
		c.Request.Body = io.NopCloser(bytes.NewReader(a.cachedRawBody))
	}

	if len(a.cachedRawBody) > 0 {
		res := gjson.ParseBytes(a.cachedRawBody)
		var duration float64
		if d := res.Get("duration"); d.Exists() {
			duration = d.Float()
		} else if s := res.Get("seconds"); s.Exists() {
			duration = s.Float()
		}
		if duration > 0 {
			return map[string]float64{
				"seconds": duration,
			}
		}
	}

	return nil
}

func (a *TaskAdaptor) AdjustBillingOnSubmit(info *relaycommon.RelayInfo, taskData []byte) map[string]float64 {
	return nil
}

func (a *TaskAdaptor) AdjustBillingOnComplete(task *model.Task, taskResult *relaycommon.TaskInfo) int {
	return 0
}

func (a *TaskAdaptor) BuildRequestURL(info *relaycommon.RelayInfo) (string, error) {
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
		return "", errors.New("fal task adaptor: model is required")
	}

	return strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(modelName, "/"), nil
}

func (a *TaskAdaptor) BuildRequestHeader(c *gin.Context, req *http.Request, info *relaycommon.RelayInfo) error {
	if info == nil {
		return errors.New("relay info is nil")
	}
	if info.ApiKey == "" {
		return errors.New("fal api key is required")
	}
	req.Header.Set("Authorization", "Key "+info.ApiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return nil
}

func (a *TaskAdaptor) BuildRequestBody(c *gin.Context, info *relaycommon.RelayInfo) (io.Reader, error) {
	rawBytes := a.cachedRawBody
	if len(rawBytes) == 0 && c != nil && c.Request != nil && c.Request.Body != nil {
		var err error
		rawBytes, err = io.ReadAll(io.LimitReader(c.Request.Body, 10<<20))
		if err != nil {
			return nil, err
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(rawBytes))
	}

	if len(rawBytes) == 0 {
		return nil, errors.New("empty request body")
	}

	// Clean OpenAI specific parameters using zero-allocation gjson scan
	cleanPayload := make(map[string]any)
	gjson.ParseBytes(rawBytes).ForEach(func(key, value gjson.Result) bool {
		k := key.String()
		if _, omit := openAIFieldsToOmit[k]; !omit {
			cleanPayload[k] = value.Value()
		}
		return true
	})

	// Map input_reference -> image_url if needed
	if _, hasImage := cleanPayload["image_url"]; !hasImage {
		if ref := gjson.GetBytes(rawBytes, "input_reference"); ref.Exists() {
			cleanPayload["image_url"] = ref.Value()
		} else if img := gjson.GetBytes(rawBytes, "image"); img.Exists() {
			cleanPayload["image_url"] = img.Value()
		}
	}

	// Map seconds -> duration if needed
	if _, hasDuration := cleanPayload["duration"]; !hasDuration {
		if sec := gjson.GetBytes(rawBytes, "seconds"); sec.Exists() {
			cleanPayload["duration"] = sec.Value()
		}
	}

	cleanedBytes, err := common.Marshal(cleanPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal clean payload: %w", err)
	}

	return bytes.NewReader(cleanedBytes), nil
}

func (a *TaskAdaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Response, error) {
	url, err := a.BuildRequestURL(info)
	if err != nil {
		return nil, fmt.Errorf("fal task adaptor build request url failed: %w", err)
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, url, requestBody)
	if err != nil {
		return nil, fmt.Errorf("fal task adaptor create request failed: %w", err)
	}

	if err := a.BuildRequestHeader(c, req, info); err != nil {
		return nil, fmt.Errorf("fal task adaptor build request header failed: %w", err)
	}

	client := service.GetHttpClient()
	if client == nil {
		client = http.DefaultClient
	}
	if info != nil && info.ChannelSetting.Proxy != "" {
		if proxyClient, err := service.GetHttpClientWithProxy(info.ChannelSetting.Proxy); err == nil && proxyClient != nil {
			client = proxyClient
		}
	}

	return client.Do(req)
}

func (a *TaskAdaptor) ParseResponse(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*channel.TaskSubmitResponse, *taskdto.TaskError) {
	if resp == nil {
		return nil, service.TaskErrorWrapperLocal(errors.New("fal submit response is nil"), "upstream_response_nil", http.StatusBadGateway)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "read_upstream_response_failed", http.StatusBadGateway)
	}

	if resp.StatusCode >= 400 {
		return nil, service.TaskErrorWrapper(errors.New(ExtractErrorMsg(body)), "upstream_submit_failed", resp.StatusCode)
	}

	var submitResp FalQueueSubmitResponse
	if err := common.Unmarshal(body, &submitResp); err != nil {
		return nil, service.TaskErrorWrapperLocal(err, "unmarshal_upstream_response_failed", http.StatusBadGateway)
	}

	if submitResp.RequestID == "" {
		return nil, service.TaskErrorWrapperLocal(errors.New("missing request_id in fal response"), "missing_request_id", http.StatusBadGateway)
	}

	// Critical: store status_url and response_url verbatim
	taskData := FalTaskData{
		RequestID:   submitResp.RequestID,
		StatusURL:   submitResp.StatusURL,
		ResponseURL: submitResp.ResponseURL,
	}
	taskDataBytes, _ := common.Marshal(taskData)

	return &channel.TaskSubmitResponse{
		UpstreamTaskID: submitResp.RequestID,
		TaskData:       taskDataBytes,
	}, nil
}

func (a *TaskAdaptor) GetModelList() []string {
	return ModelList
}

func (a *TaskAdaptor) GetChannelName() string {
	return ChannelName
}

func (a *TaskAdaptor) FetchTask(baseURL, key string, task *model.Task, proxy string) (*http.Response, error) {
	if task == nil {
		return nil, errors.New("task is nil")
	}

	var taskData FalTaskData
	_ = task.GetData(&taskData)

	// Verbatim status_url must be used
	statusURL := taskData.StatusURL
	if statusURL == "" {
		statusURL = fmt.Sprintf("%s/requests/%s/status", strings.TrimRight(baseURL, "/"), task.TaskID)
	}

	req, err := http.NewRequest(http.MethodGet, statusURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create fal status request: %w", err)
	}

	req.Header.Set("Authorization", "Key "+key)
	req.Header.Set("Accept", "application/json")

	client := service.GetHttpClient()
	if client == nil {
		client = http.DefaultClient
	}
	if proxy != "" {
		if proxyClient, err := service.GetHttpClientWithProxy(proxy); err == nil && proxyClient != nil {
			client = proxyClient
		}
	}

	return client.Do(req)
}

func (a *TaskAdaptor) ParseTaskResult(task *model.Task, resp *http.Response, respBody []byte) (*relaycommon.TaskInfo, error) {
	if resp == nil {
		return nil, errors.New("poll response is nil")
	}

	if resp.StatusCode >= 400 {
		return &relaycommon.TaskInfo{
			Status: string(model.TaskStatusFailure),
			Reason: ExtractErrorMsg(respBody),
		}, nil
	}

	status := gjson.GetBytes(respBody, "status").String()

	if strings.EqualFold(status, "COMPLETED") {
		// Use verbatim response_url
		responseURL := gjson.GetBytes(respBody, "response_url").String()
		if responseURL == "" {
			var taskData FalTaskData
			_ = task.GetData(&taskData)
			responseURL = taskData.ResponseURL
		}
		if responseURL == "" {
			return nil, errors.New("missing response_url for completed task")
		}

		key := task.PrivateData.Key
		req, err := http.NewRequest(http.MethodGet, responseURL, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create fal result request: %w", err)
		}
		req.Header.Set("Authorization", "Key "+key)
		req.Header.Set("Accept", "application/json")

		client := service.GetHttpClient()
		if client == nil {
			client = http.DefaultClient
		}
		resultResp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch fal final result: %w", err)
		}
		defer resultResp.Body.Close()

		resultBytes, err := io.ReadAll(io.LimitReader(resultResp.Body, 10<<20))
		if err != nil {
			return nil, fmt.Errorf("failed to read fal final result: %w", err)
		}
		if resultResp.StatusCode >= 400 {
			return &relaycommon.TaskInfo{
				Status: string(model.TaskStatusFailure),
				Reason: ExtractErrorMsg(resultBytes),
			}, nil
		}

		videoURL, err := ExtractVideoURL(resultBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to extract video url: %w", err)
		}

		var taskData FalTaskData
		_ = task.GetData(&taskData)
		taskData.VideoURL = videoURL
		task.SetData(taskData)
		return &relaycommon.TaskInfo{
			Status:   string(model.TaskStatusSuccess),
			Progress: "100%",
			Url:      videoURL,
		}, nil
	}

	if strings.EqualFold(status, "FAILED") {
		failReason := ExtractErrorMsg(respBody)
		if failReason == "" {
			failReason = "fal upstream task status failed"
		}
		return &relaycommon.TaskInfo{
			Status: string(model.TaskStatusFailure),
			Reason: failReason,
		}, nil
	}

	if strings.EqualFold(status, "IN_PROGRESS") {
		return &relaycommon.TaskInfo{
			Status:   string(model.TaskStatusInProgress),
			Progress: "50%",
		}, nil
	}

	if strings.EqualFold(status, "IN_QUEUE") {
		progress := "10%"
		if qPos := gjson.GetBytes(respBody, "queue_position"); qPos.Exists() {
			progress = fmt.Sprintf("queue pos: %d", qPos.Int())
		}
		return &relaycommon.TaskInfo{
			Status:   string(model.TaskStatusQueued),
			Progress: progress,
		}, nil
	}

	return &relaycommon.TaskInfo{
		Status:   string(model.TaskStatusInProgress),
		Progress: "30%",
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
