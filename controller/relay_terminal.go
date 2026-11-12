package controller

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dengyie/apihub/common"
	"github.com/dengyie/apihub/constant"
	"github.com/dengyie/apihub/loadbalancer"
	"github.com/dengyie/apihub/logger"
	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relay/helper"
	"github.com/dengyie/apihub/relaykit/types"
	"github.com/dengyie/apihub/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// relayGatewayFaultStatus 是本网关回给客户端的「上游/网关侧故障」状态码。
//
// 不能用 502：Cloudflare 会把源站的 502 响应体整个替换成它自己的纯文本
// "error code: 502"，我们写进 body 的失败原因（哪个渠道、为什么失败）根本到不了
// 客户端，使用者只看到一句无从下手的通用错误。生产实测同一次请求直连源站能拿到
// {"error":{"message":"找不到可消费的工具调用状态：call_id=..."}}，经 CF 只剩
// "error code: 502"；而 503 及其响应体 CF 原样透传。两者同为 5xx，客户端 SDK 的
// 重试语义一致，也不会把网关故障误判成自己的凭证错误。
const relayGatewayFaultStatus = http.StatusServiceUnavailable

const upstreamToolCallStateLostMessage = "工具调用续接失败：当前渠道没有这次工具调用的会话记录，无法继续。" +
	"这通常不是你的请求或凭据有问题，而是网关侧的会话粘性记录失效（重启或发版后清空），" +
	"续接请求被路由到了当初产生该 call_id 的渠道之外。这条请求重试不会再成功，请新建会话重新开始。"

const upstreamReasoningHydrationErrorMessage = "跨账号推理思考密文校验失败：多轮会话中的加密思考项与上游账号不匹配。" +
	"网关已自动尝试剥离加密项并自愈重试，若重试耗尽仍失败，建议新建会话重新开始。"

// 首字/响应头等待超时是**网关侧**的延迟归因，不是客户端请求或凭据的问题。
// 原文案直接透出 "loadbalancer: time to first token timeout"，客户端只看到
// 内部组件名与超时字段，既读不出「已自动换过渠道」，也无法判断该不该重试
// （2026-11-12 生产实测：/v1/responses 的 500 全部是这句话）。这里给出结论
// 与下一步动作，原始报错附在后面供排障转述。
const upstreamTTFTTimeoutMessage = "网关等待上游渠道首字超时：已自动尝试切换其它渠道，但本次请求在所有可用渠道上都没能在时限内拿到首个响应字节。" +
	"这不是你的请求或凭据问题（超长上下文、高峰时段更容易触发），可以直接重试；若持续出现请联系管理员检查上游渠道响应速度。"

// 渠道账号级故障（上游渠道自己欠费 / 凭据失效 / 分组无权）在池子耗尽后绝不能
// 原样透给客户端：用户会把它读成「我的账户余额不足」或「我的 API Key 无效」，
// 很多 SDK 据此判定 retryable: false 直接终止。客户端要的是「网关此刻没有能服务
// 的渠道」这个路由结论，以及「可以重试」。渠道的真实报错（剩余余额、上游
// request id 等）只属于排障信息，留在网关日志里。
const (
	upstreamChannelQuotaMessage = "上游渠道账号额度不足，网关已自动尝试其它渠道，但当前分组下暂时没有能服务该模型的可用渠道。" +
		"这不是你的账户额度问题，请稍后重试；若持续出现请联系管理员补充上游渠道额度。"
	upstreamChannelAccountMessage = "上游渠道账号鉴权或分组权限异常，网关已自动尝试其它渠道，但当前分组下暂时没有能服务该模型的可用渠道。" +
		"这不是你的 API 凭据问题，请稍后重试；若持续出现请联系管理员检查上游渠道。"
)

// sanitizeChannelScopedTerminalError 在回给客户端的最后一跳，把「渠道账号级」
// 失败改写成本网关自己的错误对象。必须在 applyRelayTerminalStatusShield 之前
// 调用：盾标只改状态码，改不掉正文里的上游账目原文。
//
// 为什么必须整条替换而不是只改 message：ToOpenAIError / ToClaudeError 直接返回
// RelayError 里的原始报文结构（relaykit/types/error.go），SetMessage 对它们
// 无效；状态码与 code 也仍然来自上游（例如上游把 body 的 code 写成
// insufficient_user_quota，WithOpenAIError 会把它当成 errorCode）。
//
// 只处理「已经走过渠道」的失败（use_channel 非空）。本地计费构造的用户欠费
// （ErrorCodeInsufficientUserQuota）发生在选渠道之前，use_channel 为空，不受影响。
// 工具调用会话丢失的 400 刻意保留原文：它是请求级的不可恢复错误，必须让客户端
// 停下来（见 applyRelayTerminalStatusShield）。
func sanitizeChannelScopedTerminalError(c *gin.Context, newAPIError *types.NewAPIError) *types.NewAPIError {
	if newAPIError == nil || types.IsClientAbortedError(newAPIError) {
		return newAPIError
	}
	if c == nil || len(c.GetStringSlice("use_channel")) == 0 {
		return newAPIError
	}
	if loadbalancer.IsUpstreamToolCallStateLostError(newAPIError) {
		return newAPIError
	}
	var message string
	switch {
	case loadbalancer.IsUpstreamQuotaError(newAPIError):
		message = upstreamChannelQuotaMessage
	case loadbalancer.IsUpstreamPermissionError(newAPIError) || newAPIError.StatusCode == http.StatusUnauthorized:
		message = upstreamChannelAccountMessage
	default:
		return newAPIError
	}
	// 状态码固定 503：这是「本轮所有渠道都不可用」的网关侧结论，不是 4xx 的
	// 客户端错误，也不是 CF 会吞掉正文的 502（见 relayGatewayFaultStatus）。
	return types.NewErrorWithStatusCode(
		errors.New(message),
		types.ErrorCodeNoAvailableChannel,
		relayGatewayFaultStatus,
	)
}

// clientFacingGatewayStatus 把内部构造的 502 统一改写成 relayGatewayFaultStatus。
// 只在「回给客户端」的最后一跳改写：内部错误分类、熔断判定、日志仍按原始状态码进行。
func clientFacingGatewayStatus(statusCode int) int {
	if statusCode == http.StatusBadGateway {
		return relayGatewayFaultStatus
	}
	return statusCode
}

// clientFacingRelayMessage 决定客户端最终读到的错误正文。
//
// 默认原样透出上游/内部的报错，只改写「上游丢失工具调用会话状态」这一类：
// 上游原文（"找不到可消费的工具调用状态：call_id=..."）只描述现象，对使用者没有
// 任何可操作信息，而这条错误在生产里几乎全由本网关自己的渠道亲和失效引起
// （见 loadbalancer.IsUpstreamToolCallStateLostError）。说明成因与出路，
// 用户才知道该重试还是该新开会话。原始报错保留在句尾，便于对着日志排查。
func clientFacingRelayMessage(newAPIError *types.NewAPIError) string {
	if newAPIError == nil {
		return ""
	}
	if loadbalancer.IsUpstreamToolCallStateLostError(newAPIError) {
		return upstreamToolCallStateLostMessage + "上游原始报错：" + newAPIError.Error()
	}
	if loadbalancer.IsReasoningHydrationError(newAPIError) {
		return upstreamReasoningHydrationErrorMessage + "上游原始报错：" + newAPIError.Error()
	}
	if loadbalancer.IsTTFTTimeout(newAPIError) {
		return upstreamTTFTTimeoutMessage + "上游原始报错：" + newAPIError.Error()
	}
	return newAPIError.Error()
}

// applyRelayTerminalStatusShield 把上游渠道的 401/403/410/400 映射成网关侧故障码，
// 避免客户端 SDK 把网关故障当成自己的凭证错误。客户端断开不得映射。
func applyRelayTerminalStatusShield(c *gin.Context, newAPIError *types.NewAPIError) {
	if newAPIError == nil || types.IsClientAbortedError(newAPIError) {
		return
	}
	isUpstreamChannelError := c != nil && len(c.GetStringSlice("use_channel")) > 0
	// 「用户自己欠费」只能由本地计费路径判定。上游响应体里的 code 会被
	// WithOpenAIError/WithClaudeError 原样写成 errorCode（relaykit/types/error.go），
	// 于是 errorCode == insufficient_user_quota 既可能是本地真实欠费，也可能是
	// 上游渠道自己的欠费报文。用 use_channel 区分：已经走过渠道的失败一律按
	// 渠道侧处理，绝不让上游的欠费冒充用户的欠费（2026-11-12 生产实测 #238
	// 的 403 因此以 403 + insufficient_user_quota 原样回给了客户端）。
	isUserQuotaDepleted := !isUpstreamChannelError &&
		newAPIError.GetErrorCode() == types.ErrorCodeInsufficientUserQuota
	if isUpstreamChannelError && !isUserQuotaDepleted {
		// 「上游丢失工具调用会话状态」终结为原始 400，绝不盾标成 5xx：这份会话
		// 历史里的 call_id 在上游已不存在，重试同一请求 100% 无效（生产实测 #248
		// 同一 call_id 两小时被客户端重打 90 次）。客户端 SDK 对 4xx 停止重试、
		// 对 5xx 指数退避重试——盾标 503 等于把不可恢复错误包装成「可重试故障」，
		// 喂出无限重试循环。400 语义也准确：错误随这份请求体走，换渠道/换时间
		// 都救不了，只有客户端换一份会话历史才能解。
		if loadbalancer.IsUpstreamToolCallStateLostError(newAPIError) {
			return
		}
		if loadbalancer.IsEOLError(newAPIError) ||
			loadbalancer.IsUpstreamPermissionError(newAPIError) ||
			loadbalancer.IsUpstreamQuotaError(newAPIError) ||
			loadbalancer.IsUpstreamRoutingError(newAPIError) ||
			loadbalancer.IsUpstreamModelUnavailableError(newAPIError) ||
			loadbalancer.IsUpstreamRelayError(newAPIError) ||
			loadbalancer.IsThinkingModeHistoryError(newAPIError) ||
			loadbalancer.IsReasoningHydrationError(newAPIError) ||
			newAPIError.StatusCode == http.StatusForbidden ||
			newAPIError.StatusCode == http.StatusUnauthorized ||
			newAPIError.StatusCode == http.StatusGone ||
			newAPIError.StatusCode == http.StatusBadRequest {
			newAPIError.StatusCode = relayGatewayFaultStatus
		}
	} else if loadbalancer.IsEOLError(newAPIError) {
		newAPIError.StatusCode = relayGatewayFaultStatus
	}
}

// clientFacingOpenAIError 构造向客户端透出的 OpenAIError。
//
// 核心原则：绝不能把渠道上游的额度耗尽（insufficient_quota）、鉴权失败（invalid_api_key）
// 或账号停用（account_deactivated）泄漏给客户端，除非本次请求确认是客户端用户自身的余额不足（ErrorCodeInsufficientUserQuota）。
// 很多 AI SDK（如 Vercel AI SDK、ZCode、Cursor）一旦在响应中看到 code="insufficient_quota"，
// 无论 HTTP 状态码是 429 还是 503，都会判定为不可恢复的账户欠费，直接终止执行并禁用后续重试（retryable: false）。
//
//  1. 若终结状态码被盾标改写为 5xx（如 503 relayGatewayFaultStatus），错误码必须对齐到该服务端状态码，
//     且 type 统一清洗为 upstream_error，确保客户端正确识别为服务端瞬时故障并正常重试。
//  2. 若走到了上游渠道（len(use_channel) > 0）且非用户自身欠费，即使状态码为 429（如聚合站 CPA 错报 code="insufficient_quota"），
//     也必须清洗渠道侧的误导性错误码，改写为 rate_limit_exceeded / rate_limit_error。
func clientFacingOpenAIError(c *gin.Context, newAPIError *types.NewAPIError, statusCode int) types.OpenAIError {
	openAIErr := newAPIError.ToOpenAIError()
	isUpstreamChannelError := c != nil && len(c.GetStringSlice("use_channel")) > 0
	// 与 applyRelayTerminalStatusShield 同口径：errorCode 只是上游响应体 code 的
	// 副本（见 WithOpenAIError），见过渠道失败之后它不代表用户欠费。
	isUserQuotaDepleted := !isUpstreamChannelError && newAPIError.GetErrorCode() == types.ErrorCodeInsufficientUserQuota

	if statusCode >= http.StatusInternalServerError {
		openAIErr.Code = statusCode
		// 5xx 的 type 必须是客户端认得的服务端类别。我们自己的内部类型
		// （new_api_error）和上游自造的 type（insufficient_quota /
		// authentication_error …）都不能透出：前者是内部实现泄漏，后者会让
		// SDK 把网关故障判成账户问题。标准的服务端类别原样保留。
		switch strings.ToLower(openAIErr.Type) {
		case "server_error", "api_error", "rate_limit_error", "overloaded_error":
		default:
			openAIErr.Type = "upstream_error"
		}
	} else if isUpstreamChannelError && !isUserQuotaDepleted {
		// 上游渠道自造的 type 不得透出（见上）：一律按服务端故障类别清洗。
		openAIErr.Type = "upstream_error"
		// 上游把瞬时限流包成 code="insufficient_quota" 时（CPA/One-API 常见），
		// 客户端 SDK 会据此判定账户欠费并停止重试；按真实语义改写成限流码。
		if loadbalancer.IsUpstreamRateLimitError(newAPIError) {
			openAIErr.Code = "rate_limit_exceeded"
		}
	}
	return openAIErr
}

// clientFacingClaudeError 构造向客户端透出的 ClaudeError。
//
// Claude 错误协议中没有独立的 code 字段，错误类别全靠 type（如 api_error、rate_limit_error、overloaded_error）。
// 同样必须防止渠道侧的 upstream 4xx 错误码（如 insufficient_quota、invalid_api_key）污染 type。
func clientFacingClaudeError(c *gin.Context, newAPIError *types.NewAPIError, statusCode int) types.ClaudeError {
	claudeErr := newAPIError.ToClaudeError()
	isUpstreamChannelError := c != nil && len(c.GetStringSlice("use_channel")) > 0
	// 与 applyRelayTerminalStatusShield 同口径：errorCode 只是上游响应体 code 的
	// 副本（见 WithClaudeError / WithOpenAIError），见过渠道失败之后它不代表用户欠费。
	isUserQuotaDepleted := !isUpstreamChannelError && newAPIError.GetErrorCode() == types.ErrorCodeInsufficientUserQuota

	// 渠道侧的「额度不足 / 凭据无效」type 只在上游自报 429（瞬时频次）时讲得通；
	// 其它状态码一律 api_error，不能让上游自造的 type 决定客户端是否重试。
	if statusCode >= http.StatusInternalServerError ||
		(isUpstreamChannelError && !isUserQuotaDepleted && statusCode != http.StatusTooManyRequests) {
		claudeErr.Type = "api_error"
	} else if isUpstreamChannelError && !isUserQuotaDepleted {
		typeStr := strings.ToLower(claudeErr.Type)
		if strings.Contains(typeStr, "quota") || strings.Contains(typeStr, "insufficient") ||
			strings.Contains(typeStr, "invalid_api_key") || strings.Contains(typeStr, "account_deactivated") {
			if statusCode == http.StatusTooManyRequests || loadbalancer.IsUpstreamRateLimitError(newAPIError) {
				claudeErr.Type = "rate_limit_error"
			} else {
				claudeErr.Type = "api_error"
			}
		}
	}
	return claudeErr
}

func writeRelayTerminalError(c *gin.Context, ws *websocket.Conn, relayFormat types.RelayFormat, newAPIError *types.NewAPIError) {
	if c == nil || newAPIError == nil {
		return
	}
	// 客户端读到的状态码在最后一跳统一改写，见 clientFacingGatewayStatus。
	statusCode := clientFacingGatewayStatus(newAPIError.StatusCode)
	openAIErr := clientFacingOpenAIError(c, newAPIError, statusCode)
	claudeErr := clientFacingClaudeError(c, newAPIError, statusCode)

	if !c.Writer.Written() {
		switch relayFormat {
		case types.RelayFormatOpenAIRealtime:
			helper.WssError(c, ws, openAIErr)
		case types.RelayFormatClaude:
			c.JSON(statusCode, gin.H{
				"type":  "error",
				"error": claudeErr,
			})
		default:
			c.JSON(statusCode, gin.H{
				"error": openAIErr,
			})
		}
		return
	}
	// SSE 流已经输出部分数据（headers 已发送），无法再发送 HTTP 状态码或普通 JSON。
	// 必须通过 SSE 协议发送标准错误帧，通知客户端请求失败并携带错误码，
	// 这样客户端 SDK（如 ZCode、OpenAI SDK）能正确解析出错误并标记 retryable: true。
	var sseErrData string
	switch relayFormat {
	case types.RelayFormatClaude:
		errJSON, _ := common.Marshal(gin.H{
			"type":  "error",
			"error": claudeErr,
		})
		sseErrData = fmt.Sprintf("event: error\ndata: %s\n\n", string(errJSON))
	default:
		errJSON, _ := common.Marshal(gin.H{
			"error": openAIErr,
		})
		sseErrData = fmt.Sprintf("data: %s\n\n", string(errJSON))
	}
	_, _ = c.Writer.Write([]byte(sseErrData))
	c.Writer.Flush()
}

// recordRequestErrorLogSafely 把这次请求的失败补记成一条 type=5 行，否则通用日志页
// 只看得见成功的 type=2，请求级失败完全不可见。判开关、判 opt-out、判去重标记都在
// service.RecordRequestErrorLog 里面，这里只负责一件事：不让观测代码弄坏响应。
//
// HTTP 路径上这一行跑在终结 defer 里、writeRelayTerminalError 之前；WebSocket 路径上
// 它跑在逐轮终结的 middleware 里，而 responsesWSRequestEngine 是裸 gin.New()，全仓
// 唯一的 gin.CustomRecovery 挂在 main.go:258 的主 server 上，覆盖不到它。此处一旦
// panic 就会一路穿过 engine.ServeHTTP → s.runCall → ResponsesWebSocketHelper 掀掉整条
// WebSocket 连接。所以两条路径都必须过这一层包装，绝不能裸调 RecordRequestErrorLog。
//
// recover 只在本层自己 panic 时才拦得住：外层正在展开的 panic 传进来时，这里是
// 非 panic 帧，recover() 返回 nil，下面的 recover 分支不成立，外层 panic 照常继续
// 向上传播。反过来（HTTP 终结 defer 里本层自己 panic）也正是这里要拦的情况。
// 静默吞掉会让日志缺口变成无人知晓的缺口，所以 recover 之后留一条带定位信息的痕迹。
func recordRequestErrorLogSafely(c *gin.Context, relayInfo *relaycommon.RelayInfo, err *types.NewAPIError) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// recover 存在的唯一意义就是事故时刻，所以这行必须自带定位信息：
			// 只有报错文本的话，值班的人还得先反查这次请求是谁、用什么模型、走的哪个渠道。
			logger.LogError(c, fmt.Sprintf("record request error log panicked: %v, user_id=%d, model=%s, channel_id=%d",
				recovered,
				c.GetInt("id"),
				common.GetContextKeyString(c, constant.ContextKeyOriginalModel),
				common.GetContextKeyInt(c, constant.ContextKeyChannelId)))
		}
	}()
	service.RecordRequestErrorLog(c, err, relayInfo)
}
