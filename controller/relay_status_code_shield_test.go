package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dengyie/apihub/common"
	"github.com/dengyie/apihub/constant"
	"github.com/dengyie/apihub/loadbalancer"
	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpstreamChannelErrorStatusShielding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name           string
		useChannel     []string
		errorCode      types.ErrorCode
		statusCode     int
		errMessage     string
		expectedStatus int
	}{
		{
			name:           "upstream 403 group permission denied mapped to 503",
			useChannel:     []string{"165"},
			errorCode:      types.ErrorCodeBadResponseStatusCode,
			statusCode:     http.StatusForbidden,
			errMessage:     "无权访问 按量分组 分组 (request id: 202609280523093326619788268d9d6owlmt1Nf)",
			expectedStatus: http.StatusServiceUnavailable,
		},
		{
			name:           "upstream 404 TokenPlan model unsupported mapped to 503",
			useChannel:     []string{"150"},
			errorCode:      types.ErrorCodeBadResponseStatusCode,
			statusCode:     http.StatusNotFound,
			errMessage:     "deepseek-v4-flash is not supported by TokenPlan",
			expectedStatus: http.StatusServiceUnavailable,
		},
		{
			name:           "upstream 401 invalid key mapped to 503",
			useChannel:     []string{"120"},
			errorCode:      types.ErrorCodeBadResponseStatusCode,
			statusCode:     http.StatusUnauthorized,
			errMessage:     "Invalid API Key",
			expectedStatus: http.StatusServiceUnavailable,
		},
		{
			name:           "upstream 410 EOL model mapped to 503",
			useChannel:     []string{"56"},
			errorCode:      types.ErrorCodeBadResponseStatusCode,
			statusCode:     410,
			errMessage:     "The model deepseek-ai/deepseek-v4-flash-0731 has reached its end of life",
			expectedStatus: http.StatusServiceUnavailable,
		},
		{
			name:           "downstream client insufficient quota remains 403",
			useChannel:     nil,
			errorCode:      types.ErrorCodeInsufficientUserQuota,
			statusCode:     http.StatusForbidden,
			errMessage:     "用户额度不足, 剩余额度: 0",
			expectedStatus: http.StatusForbidden,
		},
		{
			name:           "upstream 400 parameter level max not supported mapped to 503",
			useChannel:     []string{"19"},
			errorCode:      types.ErrorCodeBadResponseStatusCode,
			statusCode:     http.StatusBadRequest,
			errMessage:     `level "max" not supported, valid levels: low, medium, high`,
			expectedStatus: http.StatusServiceUnavailable,
		},
		{
			name:           "upstream 400 thinking mode history error mapped to 503",
			useChannel:     []string{"24"},
			errorCode:      types.ErrorCodeBadResponseStatusCode,
			statusCode:     http.StatusBadRequest,
			errMessage:     "The `reasoning_content` in the thinking mode must be passed back to the API.",
			expectedStatus: http.StatusServiceUnavailable,
		},
		{
			// 2026-11-10 根修：stale tool-call 错误终结为 400，不再盾标 503。
			// 503 会让客户端 SDK 指数退避重试一个永远不可能成功的请求（生产实测
			// #248 同一死 call_id 两小时被重打 90 次）；4xx 才能让客户端停下。
			name:           "upstream 400 tool call state lost stays 400 (no retry bait)",
			useChannel:     []string{"155"},
			errorCode:      types.ErrorCodeBadResponseStatusCode,
			statusCode:     http.StatusBadRequest,
			errMessage:     "找不到可消费的工具调用状态：call_id=call_qGRzoQMDDF3pXgOX7gt6TFaw",
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "downstream client malformed request remains 400",
			useChannel:     nil,
			errorCode:      types.ErrorCodeInvalidRequest,
			statusCode:     http.StatusBadRequest,
			errMessage:     "invalid json body",
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			if len(tc.useChannel) > 0 {
				c.Set("use_channel", tc.useChannel)
			}
			newAPIError := types.NewErrorWithStatusCode(
				errors.New(tc.errMessage),
				tc.errorCode,
				tc.statusCode,
			)

			// 直接调用生产函数，不再内联复制一遍，避免盾标改了这里却看不出来。
			applyRelayTerminalStatusShield(c, newAPIError)
			assert.Equal(t, tc.expectedStatus, newAPIError.StatusCode)
		})
	}
}

func TestClientAbortedNotMappedByTerminalShield(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("use_channel", []string{"19"})
	err := types.NewClientAbortedError(errors.New("context canceled"))
	applyRelayTerminalStatusShield(c, err)
	assert.Equal(t, types.StatusClientClosedRequest, err.StatusCode)
	assert.Equal(t, types.ErrorCodeClientAborted, err.GetErrorCode())
}

func TestWriteRelayTerminalErrorEmitsClientAbortedJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	err := types.NewClientAbortedError(errors.New("context canceled"))
	writeRelayTerminalError(c, nil, types.RelayFormatOpenAI, err)
	assert.Equal(t, types.StatusClientClosedRequest, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"client_aborted"`)
	assert.NotContains(t, w.Body.String(), `"code":500`)
}

func TestWriteRelayTerminalErrorSkipsSSEFrameWhenAlreadyWrittenAndAborted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	_, err := c.Writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	require.NoError(t, err)
	before := w.Body.String()
	// abort + already written is handled by Relay defer, not writeRelayTerminalError.
	// Guard the helper itself still writes SSE for non-abort errors.
	streamErr := types.NewErrorWithStatusCode(
		&loadbalancer.StreamBrokenError{ChannelID: 1, Reason: "rst"},
		types.ErrorCodeBadResponseBody,
		http.StatusBadGateway,
	)
	writeRelayTerminalError(c, nil, types.RelayFormatOpenAI, streamErr)
	assert.True(t, strings.Contains(w.Body.String(), before))
	// 内部错误码仍是 502，但 SSE 帧里回给客户端的码被改写成 503
	// （Cloudflare 会替换源站 502 的响应体，见 clientFacingGatewayStatus）。
	assert.Contains(t, w.Body.String(), `"code":503`)
	assert.NotContains(t, w.Body.String(), `"code":502`)
}

func TestZeroByteRetryableAndStreamDeliveredContent(t *testing.T) {
	assert.False(t, streamDeliveredContent(nil))
	assert.False(t, streamDeliveredContent(&relaycommon.RelayInfo{}))
	// 结构帧（role/typed 状态事件）对用户不可见，不构成"已交付内容"：
	// 断流后仍需允许换渠道重试。
	assert.False(t, streamDeliveredContent(&relaycommon.RelayInfo{ReceivedResponseCount: 1}))
	assert.False(t, streamDeliveredContent(&relaycommon.RelayInfo{ReceivedContentBytes: 8}))
	assert.True(t, streamDeliveredContent(&relaycommon.RelayInfo{ReceivedVisibleContentBytes: 8}))

	assert.False(t, zeroByteRetryable(nil))
	assert.False(t, zeroByteRetryable(types.NewClientAbortedError(errors.New("canceled"))))
	assert.True(t, zeroByteRetryable(types.NewErrorWithStatusCode(
		&loadbalancer.EmptyStreamError{ChannelID: 1},
		types.ErrorCodeBadResponseBody,
		http.StatusBadGateway,
	)))
	assert.True(t, zeroByteRetryable(types.NewErrorWithStatusCode(
		&loadbalancer.TTFTTimeoutError{ChannelID: 1, TimeoutMs: 5000},
		types.ErrorCodeBadResponseBody,
		http.StatusBadGateway,
	)))
	assert.True(t, zeroByteRetryable(types.NewError(errors.New("dial"), types.ErrorCodeDoRequestFailed)))
	assert.False(t, zeroByteRetryable(types.NewErrorWithStatusCode(
		errors.New("bad json"),
		types.ErrorCodeInvalidRequest,
		http.StatusBadRequest,
		types.ErrOptionWithSkipRetry(),
	)))
}

func TestIsClientAbortUpstreamHang(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 1. 下游已写出响应（流式或已有数据） -> 不是假死逼退
	w1 := httptest.NewRecorder()
	c1, _ := gin.CreateTestContext(w1)
	_, _ = c1.Writer.Write([]byte("data: hi\n\n"))
	assert.False(t, isClientAbortUpstreamHang(c1, &relaycommon.RelayInfo{}, time.Now().Add(-60*time.Second)))

	// 2. relayInfo 只记录结构帧（role/typed 状态）-> 用户画面仍空白，
	//    等待 60s 后离开仍属上游死挂
	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	assert.True(t, isClientAbortUpstreamHang(c2, &relaycommon.RelayInfo{ReceivedResponseCount: 2}, time.Now().Add(-60*time.Second)))

	// 3. relayInfo 记录结构帧字节 -> 同上，不构成可见交付
	w3 := httptest.NewRecorder()
	c3, _ := gin.CreateTestContext(w3)
	assert.True(t, isClientAbortUpstreamHang(c3, &relaycommon.RelayInfo{ReceivedContentBytes: 100}, time.Now().Add(-60*time.Second)))

	// 3b. 已交付可见内容 -> 不是假死逼退
	w3b := httptest.NewRecorder()
	c3b, _ := gin.CreateTestContext(w3b)
	assert.False(t, isClientAbortUpstreamHang(c3b, &relaycommon.RelayInfo{ReceivedVisibleContentBytes: 100}, time.Now().Add(-60*time.Second)))

	// 4. 用户快速主动取消（耗时未达 30s，例如 5s） -> 正常交互取消，不计入假死逼退
	w4 := httptest.NewRecorder()
	c4, _ := gin.CreateTestContext(w4)
	assert.False(t, isClientAbortUpstreamHang(c4, &relaycommon.RelayInfo{}, time.Now().Add(-5*time.Second)))

	// 5. 上游死挂逼退（零字节交付，且等待时间达 35s） -> 判定为上游死挂，计入渠道故障
	w5 := httptest.NewRecorder()
	c5, _ := gin.CreateTestContext(w5)
	assert.True(t, isClientAbortUpstreamHang(c5, &relaycommon.RelayInfo{}, time.Now().Add(-35*time.Second)))

	// 6. 渠道配置较短的 TTFT 阈值（如 10s）：下游等待 15s（未达 30s 默认但超过渠道阈值） -> 判定为上游死挂
	policy := loadbalancer.DefaultPolicy()
	policy.Enabled = true
	policy.Channels = map[int]loadbalancer.ChannelPolicy{
		88: {TTFTTimeoutMs: 10000},
	}
	loadbalancer.SetPolicy(policy)
	defer loadbalancer.SetPolicy(nil)

	w6 := httptest.NewRecorder()
	c6, _ := gin.CreateTestContext(w6)
	info6 := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 88},
	}
	assert.True(t, isClientAbortUpstreamHang(c6, info6, time.Now().Add(-15*time.Second)))

	// 7. 深度思考模型配置大 TTFT 阈值（如 60s）：下游等待 35s 主动取消 -> 处于合法思考窗口内，不得误判为上游死挂
	policyDeep := loadbalancer.DefaultPolicy()
	policyDeep.Enabled = true
	policyDeep.Channels = map[int]loadbalancer.ChannelPolicy{
		99: {TTFTTimeoutMs: 60000},
	}
	loadbalancer.SetPolicy(policyDeep)

	w7 := httptest.NewRecorder()
	c7, _ := gin.CreateTestContext(w7)
	info7 := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 99},
	}
	assert.False(t, isClientAbortUpstreamHang(c7, info7, time.Now().Add(-35*time.Second)))
}

func TestRetryLoop_ContextDeadlineExceededDoesNotMarkClientAborted(t *testing.T) {
	deadlineCtx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-deadlineCtx.Done()

	assert.True(t, errors.Is(deadlineCtx.Err(), context.DeadlineExceeded))
	assert.False(t, errors.Is(deadlineCtx.Err(), context.Canceled))
}

// TestToolCallStateLostMessageIsSelfExplanatory 钉住用户最终读到的文案：
// 上游原文只描述现象，必须被替换成有成因、有出路的说明，原始报错附在句尾备查。
// 出路必须是「新建会话」而非「重试」——重试同一请求永远失败（call_id 已死），
// 提示重试会喂出客户端无限重试循环（生产实测 #248 同一 call_id 90 次重打）。
func TestToolCallStateLostMessageIsSelfExplanatory(t *testing.T) {
	raw := "找不到可消费的工具调用状态：call_id=call_qGRzoQMDDF3pXgOX7gt6TFaw"
	err := types.NewErrorWithStatusCode(
		errors.New(raw),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusBadRequest,
	)
	require.True(t, loadbalancer.IsUpstreamToolCallStateLostError(err), "分类器必须命中上游原文")

	msg := clientFacingRelayMessage(err)
	assert.Contains(t, msg, "工具调用续接失败")
	assert.Contains(t, msg, "会话粘性")
	assert.Contains(t, msg, "重试不会再成功", "必须明确告知重试无效")
	assert.Contains(t, msg, "新建会话")
	assert.NotContains(t, msg, "请重试", "不得暗示重试同一请求")
	assert.Contains(t, msg, raw, "原始报错要保留，便于对日志排查")
}

// TestToolCallStateLostShieldKeeps400 终结盾不得把「上游丢失工具调用会话状态」
// 改写成 5xx：4xx 语义（错误随请求体的历史内容走）才能让客户端 SDK 停止重试。
// 生产实测：503 + 「请重试」文案曾让 codex CLI 对同一死 call_id 指数退避重打
// 两小时（#248 渠道 90 次）。
func TestToolCallStateLostShieldKeeps400(t *testing.T) {
	raw := "找不到可消费的工具调用状态：call_id=call_qGRzoQMDDF3pXgOX7gt6TFaw"
	err := types.NewErrorWithStatusCode(
		errors.New(raw),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusBadRequest,
	)
	require.True(t, loadbalancer.IsUpstreamToolCallStateLostError(err))

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set("use_channel", []string{"248"})

	applyRelayTerminalStatusShield(c, err)
	assert.Equal(t, http.StatusBadRequest, err.StatusCode,
		"盾必须放行这类错误保持 400，改写成 5xx 会喂出客户端重试循环")
}

// TestReasoningHydrationErrorMessageIsSelfExplanatory 验证跨账号推理水合报错文案被友好改写。
func TestReasoningHydrationErrorMessageIsSelfExplanatory(t *testing.T) {
	raw := "The encrypted content for item rs_01fd could not be verified. Reason: reasoning hydration failed: Encrypted content could not be decrypted or parsed."
	err := types.NewErrorWithStatusCode(
		errors.New(raw),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusBadRequest,
	)
	require.True(t, loadbalancer.IsReasoningHydrationError(err), "分类器必须命中推理水合错误")

	msg := clientFacingRelayMessage(err)
	assert.Contains(t, msg, "跨账号推理思考密文校验失败")
	assert.Contains(t, msg, "自愈重试")
	assert.Contains(t, msg, raw, "原始报错要保留，便于对日志排查")
}

// TestTTFTTimeoutMessageIsActionable 首字/响应头等待超时是**网关侧**的延迟归因，
// 不是客户端的请求或凭据问题。此前原文案把 "loadbalancer: time to first token
// timeout" 直接透给客户端：读不出「已尝试换渠道」，也读不出该不该重试
// （2026-11-12 生产实测：/v1/responses 的三个 500 全部是这句话）。
func TestTTFTTimeoutMessageIsActionable(t *testing.T) {
	err := types.NewErrorWithStatusCode(
		&loadbalancer.TTFTTimeoutError{ChannelID: 355, TimeoutMs: 18482},
		types.ErrorCodeChannelResponseTimeExceeded,
		http.StatusGatewayTimeout,
	)
	require.True(t, loadbalancer.IsTTFTTimeout(err), "504 仍必须被 ttft_timeout 重试分支识别")

	msg := clientFacingRelayMessage(err)
	assert.Contains(t, msg, "首字超时")
	assert.Contains(t, msg, "自动尝试切换其它渠道")
	assert.Contains(t, msg, "不是你的请求或凭据问题")
	assert.Contains(t, msg, "可以直接重试")
	assert.Contains(t, msg, "time to first token timeout", "原始报错要保留，便于对日志排查")
}

// TestClientFacingRelayMessagePassesThroughOthers 确保只有这一类被改写。
func TestClientFacingRelayMessagePassesThroughOthers(t *testing.T) {
	err := types.NewErrorWithStatusCode(
		errors.New("The model has reached its end of life"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusGone,
	)
	assert.Equal(t, err.Error(), clientFacingRelayMessage(err))
	assert.Equal(t, "", clientFacingRelayMessage(nil))
}

// TestClientFacingGatewayStatusRewritesOnly502 Cloudflare 会把源站的 502 响应体
// 换成它自己的 "error code: 502"，所以回给客户端的最后一跳不能出现 502。
func TestClientFacingGatewayStatusRewritesOnly502(t *testing.T) {
	assert.Equal(t, http.StatusServiceUnavailable, clientFacingGatewayStatus(http.StatusBadGateway))
	for _, code := range []int{http.StatusOK, http.StatusBadRequest, http.StatusUnauthorized,
		http.StatusForbidden, http.StatusGone, http.StatusTooManyRequests,
		http.StatusGatewayTimeout, http.StatusServiceUnavailable} {
		assert.Equal(t, code, clientFacingGatewayStatus(code))
	}
}

func TestStreamScannerTTFTTimeoutPreservedInRelayError(t *testing.T) {
	status := relaycommon.NewStreamStatus()
	ttftErr := &loadbalancer.TTFTTimeoutError{ChannelID: 88, TimeoutMs: 25000}
	status.SetEndReason(relaycommon.StreamEndReasonTimeout, ttftErr)

	assert.True(t, status.IsUpstreamStreamFault())
	_, endErr := status.EndState()
	require.NotNil(t, endErr)
	assert.True(t, loadbalancer.IsTTFTTimeout(endErr))

	// 模拟 controller/relay.go:315 逻辑构造的错误
	var underlyingErr error
	var errCode types.ErrorCode
	statusCode := http.StatusBadGateway
	if endErr != nil && loadbalancer.IsTTFTTimeout(endErr) {
		underlyingErr = endErr
		errCode = types.ErrorCodeChannelResponseTimeExceeded
		statusCode = http.StatusGatewayTimeout
	} else {
		underlyingErr = &loadbalancer.StreamBrokenError{
			ChannelID: 88,
			Reason:    status.Summary(),
			Err:       endErr,
		}
		errCode = types.ErrorCodeBadResponseBody
	}
	apiErr := types.NewErrorWithStatusCode(underlyingErr, errCode, statusCode)

	assert.True(t, loadbalancer.IsTTFTTimeout(apiErr), "构造后的 API 错误必须能被 IsTTFTTimeout 解包")
	assert.True(t, zeroByteRetryable(apiErr), "流式首字超时必须允许零字节换渠道重试")
	assert.Equal(t, http.StatusGatewayTimeout, apiErr.StatusCode)
}

func TestRetryLoop_ClientCanceledOverwritesPriorAttemptError(t *testing.T) {
	priorError := types.NewErrorWithStatusCode(errors.New("upstream internal error"), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	require.False(t, types.IsClientAbortedError(priorError))

	// 模拟客户端在准备下一次重试前主动断开
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()

	var finalAPIError *types.NewAPIError = priorError
	if errors.Is(cancelCtx.Err(), context.Canceled) {
		finalAPIError = types.NewClientAbortedError(cancelCtx.Err())
	}

	assert.True(t, types.IsClientAbortedError(finalAPIError), "客户端取消必须覆盖前序尝试的 500 错误，返回 499 client_aborted")
	assert.Equal(t, 499, finalAPIError.StatusCode)
}

func TestWriteRelayTerminalErrorShieldsUpstreamQuotaInNonSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 1. 模拟上游渠道 #65 返回 429，内部携带错配的 code: "insufficient_quota"，status 被盾标映射为 503
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set("use_channel", []string{"281", "238", "318", "157", "150", "65"})

	upstreamErr := types.WithOpenAIError(types.OpenAIError{
		Code:    "insufficient_quota",
		Message: "inference exceeds tpm/rpm limit",
		Type:    "rate_limit_error",
	}, http.StatusTooManyRequests)

	applyRelayTerminalStatusShield(c, upstreamErr)
	// 即使因为其他判定映射为 503，或者未被映射保持 429
	writeRelayTerminalError(c, nil, types.RelayFormatOpenAI, upstreamErr)

	assert.NotEqual(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.NotContains(t, body, `"insufficient_quota"`, "上游渠道的 insufficient_quota 绝不能泄漏给客户端")
	assert.Contains(t, body, "inference exceeds tpm/rpm limit")

	// 2. 验证真实用户欠费时（ErrorCodeInsufficientUserQuota），必须正常返回 403 和 insufficient_user_quota
	wUser := httptest.NewRecorder()
	cUser, _ := gin.CreateTestContext(wUser)
	cUser.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	userErr := types.NewErrorWithStatusCode(
		errors.New("用户额度不足, 剩余额度: 0"),
		types.ErrorCodeInsufficientUserQuota,
		http.StatusForbidden,
	)
	writeRelayTerminalError(cUser, nil, types.RelayFormatOpenAI, userErr)
	assert.Equal(t, http.StatusForbidden, wUser.Code)
	assert.Contains(t, wUser.Body.String(), "insufficient_user_quota", "客户端用户自身欠费必须如实告知客户端")
}

func TestWriteRelayTerminalErrorShieldsClaudeAndSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Claude 格式下上游 503
	wClaude := httptest.NewRecorder()
	cClaude, _ := gin.CreateTestContext(wClaude)
	cClaude.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	cClaude.Set("use_channel", []string{"120"})

	upstreamErr := types.WithOpenAIError(types.OpenAIError{
		Code:    "insufficient_quota",
		Message: "upstream out of credits",
		Type:    "insufficient_quota",
	}, http.StatusServiceUnavailable)

	writeRelayTerminalError(cClaude, nil, types.RelayFormatClaude, upstreamErr)
	assert.Equal(t, http.StatusServiceUnavailable, wClaude.Code)
	assert.NotContains(t, wClaude.Body.String(), `"insufficient_quota"`)
	assert.Contains(t, wClaude.Body.String(), `"api_error"`)

	// SSE 流式下已写入部分数据后发生故障
	wSSE := httptest.NewRecorder()
	cSSE, _ := gin.CreateTestContext(wSSE)
	cSSE.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	cSSE.Set("use_channel", []string{"65"})
	_, _ = cSSE.Writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"test\"}}]}\n\n"))

	sseErr := types.WithOpenAIError(types.OpenAIError{
		Code:    "insufficient_quota",
		Message: "inference exceeds tpm/rpm limit",
		Type:    "rate_limit_error",
	}, http.StatusServiceUnavailable)

	writeRelayTerminalError(cSSE, nil, types.RelayFormatOpenAI, sseErr)
	assert.NotContains(t, wSSE.Body.String(), `"insufficient_quota"`)
	assert.Contains(t, wSSE.Body.String(), `"code":503`)

	// 401 鉴权失败上游错误被盾标映射为 503 时，Code 和 Type 必须清洗为 upstream_error
	wAuth := httptest.NewRecorder()
	cAuth, _ := gin.CreateTestContext(wAuth)
	cAuth.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	cAuth.Set("use_channel", []string{"77"})

	authErr := types.WithOpenAIError(types.OpenAIError{
		Code:    "invalid_api_key",
		Message: "Incorrect API key provided",
		Type:    "authentication_error",
	}, http.StatusUnauthorized)

	applyRelayTerminalStatusShield(cAuth, authErr)
	assert.Equal(t, relayGatewayFaultStatus, authErr.StatusCode)

	writeRelayTerminalError(cAuth, nil, types.RelayFormatOpenAI, authErr)
	assert.Equal(t, relayGatewayFaultStatus, wAuth.Code)
	authBody := wAuth.Body.String()
	assert.NotContains(t, authBody, `"invalid_api_key"`)
	assert.NotContains(t, authBody, `"authentication_error"`)
	assert.Contains(t, authBody, `"upstream_error"`)
	assert.Contains(t, authBody, `"code":503`)
}

func TestClientSafetyCeilingStreamVsNonStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 1. 流式请求：耗时逼近 105s 安全死线（例如已过去 96s，剩余 < 10s），触发主动 504 熔断
	wStream := httptest.NewRecorder()
	cStream, _ := gin.CreateTestContext(wStream)
	cStream.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(cStream, constant.ContextKeyRequestStartTime, time.Now().Add(-96*time.Second))

	streamInfo := &relaycommon.RelayInfo{
		IsStream:              true,
		ReceivedResponseCount: 0,
		ReceivedContentBytes:  0,
	}

	safetyLimit := loadbalancer.GetClientSafetyDeadline()
	assert.Equal(t, 105*time.Second, safetyLimit)

	var streamCeiling time.Time
	if streamInfo != nil && streamInfo.IsStream {
		if safetyLimit > 0 {
			reqStart := common.GetContextKeyTime(cStream, constant.ContextKeyRequestStartTime)
			streamCeiling = reqStart.Add(safetyLimit)
		}
	}
	assert.False(t, streamCeiling.IsZero())
	assert.True(t, time.Now().Add(10*time.Second).After(streamCeiling), "剩余不足 10s，必须判定逼近死线")

	// 2. 非流式请求：同样耗时 96s，绝不能生成流式 safetyCeiling，不受流式死线误杀
	wNonStream := httptest.NewRecorder()
	cNonStream, _ := gin.CreateTestContext(wNonStream)
	cNonStream.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(cNonStream, constant.ContextKeyRequestStartTime, time.Now().Add(-96*time.Second))

	nonStreamInfo := &relaycommon.RelayInfo{
		IsStream: false,
	}

	var nonStreamCeiling time.Time
	if nonStreamInfo != nil && nonStreamInfo.IsStream {
		if safetyLimit > 0 {
			reqStart := common.GetContextKeyTime(cNonStream, constant.ContextKeyRequestStartTime)
			nonStreamCeiling = reqStart.Add(safetyLimit)
		}
	}
	assert.True(t, nonStreamCeiling.IsZero(), "非流式请求 safetyCeiling 必须为零值")
}

// 2026-11-12 生产证据（request 202611120119339817576915890cb63VCYO5Lt3）：
// 渠道 #238 的 403 报文是「余额不足：剩余 ＄0.140610」，body 里还带
// code/type=insufficient_user_quota。WithOpenAIError 把这个 code 原样写成
// errorCode，于是盾标把它当成「用户自己欠费」而跳过改写，客户端最终收到
// 403 + insufficient_user_quota + 上游渠道的余额原文。这组测试钉住：
// 渠道级账目错误只能变成网关侧 503，正文里不能留下任何上游账目信息。
func TestChannelScopedQuotaErrorNeverReachesClient(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newCtx := func() (*gin.Context, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", nil)
		c.Set("use_channel", []string{"278", "238"})
		return c, w
	}

	upstreamErr := func() *types.NewAPIError {
		return types.WithOpenAIError(types.OpenAIError{
			Code: "insufficient_user_quota",
			Type: "insufficient_user_quota",
			Message: "余额不足：剩余 ＄0.140610，这次请求预估需要 ＄0.201990（按输入长度和 max_tokens 估算）。" +
				"可以调低 max_tokens、改用免费模型，或到「喵乐园」赚罐罐换额度 📖 https://api.ilovecat520.me/docs#error-quota" +
				" (request id: 202611120119344938502398268d9d6KrIhp1v3)",
		}, http.StatusForbidden)
	}

	for _, format := range []struct {
		name           string
		relayFormat    types.RelayFormat
		path           string
		wantStatusText string
	}{
		{name: "claude messages", relayFormat: types.RelayFormatClaude, path: "/v1/messages", wantStatusText: "\"api_error\""},
		{name: "openai chat", relayFormat: types.RelayFormatOpenAI, path: "/v1/chat/completions", wantStatusText: "\"upstream_error\""},
	} {
		t.Run(format.name, func(t *testing.T) {
			c, w := newCtx()
			c.Request = httptest.NewRequest(http.MethodPost, format.path, nil)
			c.Set("use_channel", []string{"278", "238"})

			err := sanitizeChannelScopedTerminalError(c, upstreamErr())
			applyRelayTerminalStatusShield(c, err)
			writeRelayTerminalError(c, nil, format.relayFormat, err)

			assert.Equal(t, http.StatusServiceUnavailable, w.Code, "渠道欠费必须终结为网关侧 503")
			body := w.Body.String()
			for _, leak := range []string{"0.140610", "0.201990", "余额不足", "insufficient_user_quota", "ilovecat520", "202611120119344938502398268d9d6KrIhp1v3"} {
				assert.NotContains(t, body, leak, "上游渠道的账目/身份信息绝不能出现在客户端响应里: %s", leak)
			}
			assert.Contains(t, body, "上游渠道账号额度不足")
			assert.Contains(t, body, format.wantStatusText)
		})
	}
}

// 用户自己的欠费必须如实告知：本地计费在选渠道之前构造该错误，use_channel
// 为空，清洗与盾标都不得改写它的状态码、type 或文案。
func TestLocalUserQuotaErrorStillReachesClient(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	localErr := types.NewErrorWithStatusCode(
		errors.New("用户额度不足, 剩余额度: 0"),
		types.ErrorCodeInsufficientUserQuota,
		http.StatusForbidden,
	)

	sanitized := sanitizeChannelScopedTerminalError(c, localErr)
	require.Same(t, localErr, sanitized, "本地用户欠费不得被渠道清洗逻辑改写")
	applyRelayTerminalStatusShield(c, sanitized)
	writeRelayTerminalError(c, nil, types.RelayFormatOpenAI, sanitized)

	assert.Equal(t, http.StatusForbidden, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "insufficient_user_quota")
	assert.Contains(t, body, "用户额度不足")
}

// 渠道鉴权/分组权限失败同样不得透出上游凭据语义；401 与 403 都要归到网关侧。
func TestChannelScopedPermissionErrorRewritten(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name       string
		statusCode int
		message    string
	}{
		{name: "401 invalid key", statusCode: http.StatusUnauthorized, message: "Incorrect API key provided: sk-live-abcdef"},
		{name: "403 group permission", statusCode: http.StatusForbidden, message: "无权访问 按量分组 分组 (request id: 202609280523093326619788268d9d6owlmt1Nf)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Set("use_channel", []string{"120"})

			err := types.WithOpenAIError(types.OpenAIError{
				Code:    "invalid_api_key",
				Type:    "authentication_error",
				Message: tc.message,
			}, tc.statusCode)

			err = sanitizeChannelScopedTerminalError(c, err)
			applyRelayTerminalStatusShield(c, err)
			writeRelayTerminalError(c, nil, types.RelayFormatOpenAI, err)

			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
			body := w.Body.String()
			assert.NotContains(t, body, "invalid_api_key")
			assert.NotContains(t, body, "authentication_error")
			assert.NotContains(t, body, "sk-live-abcdef")
			assert.Contains(t, body, "上游渠道账号鉴权或分组权限异常")
		})
	}
}

// 工具调用会话丢失是请求级的不可恢复错误，刻意保留 400 与原文让客户端停下
// （见 applyRelayTerminalStatusShield 的说明）。它可能同时带有 403 状态码，
// 清洗逻辑不能把它卷成 503。
func TestChannelScopedToolCallStateLostStays400(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set("use_channel", []string{"155"})

	err := types.NewErrorWithStatusCode(
		errors.New("找不到可消费的工具调用状态：call_id=call_qGRzoQMDDF3pXgOX7gt6TFaw"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusBadRequest,
	)

	sanitized := sanitizeChannelScopedTerminalError(c, err)
	require.Same(t, err, sanitized)
	applyRelayTerminalStatusShield(c, sanitized)
	writeRelayTerminalError(c, nil, types.RelayFormatOpenAI, sanitized)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "call_qGRzoQMDDF3pXgOX7gt6TFaw")
}
