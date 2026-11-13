package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dengyie/apihub/common"
	"github.com/dengyie/apihub/constant"
	"github.com/dengyie/apihub/dto"
	"github.com/dengyie/apihub/loadbalancer"
	"github.com/dengyie/apihub/logger"
	"github.com/dengyie/apihub/model"
	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relaykit/types"
	"github.com/dengyie/apihub/setting/operation_setting"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// BreakerEffectType 表示失败对熔断器产生的显式效应类型。
type BreakerEffectType string

const (
	BreakerNone                     BreakerEffectType = "none"
	BreakerRateLimit                BreakerEffectType = "rate_limit"
	BreakerCurfew                   BreakerEffectType = "curfew"
	BreakerEOL                      BreakerEffectType = "eol"
	BreakerStreamBroken             BreakerEffectType = "stream_broken"
	BreakerQuota                    BreakerEffectType = "quota"
	BreakerRouting                  BreakerEffectType = "routing"
	BreakerThinkingHistory          BreakerEffectType = "thinking_history"
	BreakerModelUnavailable         BreakerEffectType = "model_unavailable"
	BreakerResponsesAPINotSupported BreakerEffectType = "responses_api_not_supported"
	BreakerPermission               BreakerEffectType = "permission"
	BreakerRelay                    BreakerEffectType = "relay"
)

// FailureDecision 是网关层对单次上游失败的完整结构化评定结果：
// 包含重试决策 (PolicyDecision)、熔断效应 (BreakerEffect)、自愈参数信息与预流硬故障标志。
type FailureDecision struct {
	PolicyDecision          PolicyDecision
	BreakerEffect           BreakerEffectType
	CurfewUntil             time.Time
	LearnedParam            string
	LearnedLimit            uint
	StripResponsesReasoning bool
	IsPreStreamNonTTFT      bool
}

// ClassifyBreakerEffect 对上游错误执行严格特异性排序的熔断分类（防多重熔断）。
func ClassifyBreakerEffect(err *types.NewAPIError) (BreakerEffectType, time.Time) {
	if err == nil {
		return BreakerNone, time.Time{}
	}
	// 智能负载：参数不支持（无论是 400、422、5xx 还是流中断 502 返回）属于请求形状问题，
	// 换渠道/裁剪参数即可自愈，绝不计入渠道故障熔断。
	if _, ok := loadbalancer.IsParamNotSupportedError(err); ok {
		return BreakerNone, time.Time{}
	}
	// Responses API 不被支持：模型在、协议不在。按 (渠道, 模型) 冷却即可，
	// 既不能当「模型不存在」走不可逆自动禁用，也不能因为落在 400 就完全不管
	// —— 不管的话高优先级渠道会吞掉每一个 Responses 请求。
	if loadbalancer.IsResponsesAPINotSupportedError(err) {
		return BreakerResponsesAPINotSupported, time.Time{}
	}
	if loadbalancer.IsUpstreamRateLimitError(err) {
		return BreakerRateLimit, time.Time{}
	}
	if loadbalancer.IsCurfewError(err) {
		return BreakerCurfew, loadbalancer.CurfewEndTime(time.Now())
	}
	if loadbalancer.IsEOLError(err) {
		return BreakerEOL, time.Time{}
	}
	if loadbalancer.IsStreamBroken(err) {
		return BreakerStreamBroken, time.Time{}
	}
	if loadbalancer.IsUpstreamQuotaError(err) {
		return BreakerQuota, time.Time{}
	}
	if loadbalancer.IsUpstreamRoutingError(err) {
		return BreakerRouting, time.Time{}
	}
	if loadbalancer.IsThinkingModeHistoryError(err) {
		return BreakerThinkingHistory, time.Time{}
	}
	if loadbalancer.IsUpstreamModelUnavailableError(err) {
		return BreakerModelUnavailable, time.Time{}
	}
	if loadbalancer.IsUpstreamPermissionError(err) {
		return BreakerPermission, time.Time{}
	}
	if loadbalancer.IsUpstreamRelayError(err) {
		return BreakerRelay, time.Time{}
	}
	return BreakerNone, time.Time{}
}

// IsContextualReasoningHydration 判断一次失败是否属于「请求本身带着跨账号
// reasoning/encrypted_content 历史，上游因无法解密而以不透明 400 打回」的水合失败族。
//
// 为什么需要这条额外判据：像 anyrouter 这类 new-api 中转站，其上游对跨账号密文
// 返回的 400 会被它自己再包一层 —— message 变成完全无信息量的
// "bad response status code 400 (request id: ...)"，code 可能是空串或
// bad_response_status_code，`invalid_encrypted_content` 这个唯一可靠信号就此丢失
// （实测：对 anyrouter 直接回放外账号密文，它有时干脆 520 断流）。IsReasoningHydrationError
// 对这种被抹掉 code 的包装无能为力，于是健康渠道被误判为「上游中继代理异常」并
// 软降级 5 分钟。
//
// 判据只用请求级事实 + 错误形状，不依赖不可靠的 code：
//   - 该 /v1/responses 请求原始 input 里确实带 reasoning/encrypted_content 历史；
//   - 上游返回的是 400（水合失败的固定形态；402/403/404 等额度、权限、路由类
//     4xx 有各自的专属分类，绝不能被这条判据劫持——生产实测放宽到 4xx 会让
//     同一条重试链上后续渠道的额度 403 也打出「推理水合失败」的误导日志）；
//   - 且错误属于中继包装族（IsUpstreamRelayError，即 bad response status code 等）。
//
// 命中后按水合失败处理：换渠道重试并剥离密文（DecideRelayRetry 的 retry 分支已有），
// 且不计渠道故障（EvaluateRelayFailure / relay_breaker 会把 BreakerRelay 降为 None）。
func IsContextualReasoningHydration(c *gin.Context, err *types.NewAPIError) bool {
	if c == nil || err == nil {
		return false
	}
	if !common.GetContextKeyBool(c, constant.ContextKeyResponsesHadReasoning) {
		return false
	}
	if err.StatusCode != http.StatusBadRequest {
		return false
	}
	return loadbalancer.IsUpstreamRelayError(err)
}

// EvaluateRelayFailure 执行单次评定、多维输出的完整容灾评定。
func EvaluateRelayFailure(c *gin.Context, err *types.NewAPIError, retryTimes int) FailureDecision {
	decision := FailureDecision{
		PolicyDecision: DecideRelayRetry(c, err, retryTimes),
		BreakerEffect:  BreakerNone,
	}
	if err == nil {
		return decision
	}

	decision.BreakerEffect, decision.CurfewUntil = ClassifyBreakerEffect(err)
	// 不透明 400 水合失败：请求带跨账号密文历史，但中转站把上游 400 重新包装成
	// "bad response status code 400"（code 被抹掉/为空），IsReasoningHydrationError
	// 认不出，会误落 BreakerRelay。这里按请求事实把它降为「不计渠道故障」。
	if decision.BreakerEffect == BreakerRelay && IsContextualReasoningHydration(c, err) {
		decision.BreakerEffect = BreakerNone
		decision.CurfewUntil = time.Time{}
	}

	if param, ok := loadbalancer.IsParamNotSupportedError(err); ok {
		decision.LearnedParam = param
	}
	if limit, ok := loadbalancer.ParseMaxCompletionTokensLimit(err); ok {
		decision.LearnedLimit = limit
	}
	if loadbalancer.IsReasoningHydrationError(err) || IsContextualReasoningHydration(c, err) {
		decision.StripResponsesReasoning = true
	}

	decision.IsPreStreamNonTTFT = err.StatusCode != 400 &&
		!loadbalancer.IsTTFTTimeout(err) &&
		!loadbalancer.IsEmptyStream(err) &&
		!loadbalancer.IsEmptyStreamBudget(err) &&
		!loadbalancer.IsStreamBroken(err) &&
		!loadbalancer.IsUpstreamRateLimitError(err) &&
		!types.IsClientAbortedError(err)

	return decision
}

// DecideTaskRetry is the single retry decision for task submissions.
func DecideTaskRetry(c *gin.Context, taskErr *dto.TaskError, retryTimes int) PolicyDecision {
	stop := PolicyDecision{Action: "stop", Source: "system"}
	retry := PolicyDecision{Action: "retry", Reason: "retry_status_matched", Source: "system"}
	switch {
	// 宵禁 403 换渠道重试（不能直接返回给客户端），但仍受重试预算约束，
	// 避免所有渠道都宵禁时无限循环。
	case taskErr != nil && taskErr.StatusCode == 403 &&
		(strings.Contains(taskErr.Message, "system_curfew") || strings.Contains(taskErr.Message, "宵禁")):
		if retryTimes <= 0 {
			stop.Reason, stop.Source = "attempt_budget_exhausted", "global"
			return stop
		}
		retry.Reason = "curfew_retry"
		return retry
	case taskErr == nil:
		stop.Reason = "request_completed"
	case taskErr.NoRetry:
		stop.Reason = "task_accepted"
	case ShouldSkipRetryAfterChannelAffinityFailure(c):
		stop.Reason, stop.Source = "strict_session", "session_rule"
		if source := RequestPolicy(c).SessionModeSource; source != "" {
			stop.Source = source
		}
	case retryTimes <= 0:
		stop.Reason, stop.Source = "attempt_budget_exhausted", "global"
	case GetChannelConstraints(c).SuppressesRetry():
		stop.Reason, stop.Source = "pinned_channel", "channel_constraint"
	case taskErr.StatusCode == http.StatusTooManyRequests, taskErr.StatusCode == 307:
		return retry
	case taskErr.StatusCode/100 == 5:
		if operation_setting.IsAlwaysSkipRetryStatusCode(taskErr.StatusCode) {
			stop.Reason = "system_retry_exclusion"
			break
		}
		return retry
	case taskErr.StatusCode == http.StatusBadRequest, taskErr.StatusCode == 408, taskErr.StatusCode == http.StatusUnprocessableEntity:
		stop.Reason = "status_not_retryable"
	case taskErr.LocalError:
		stop.Reason = "local_rejection"
	case taskErr.StatusCode/100 == 2:
		stop.Reason = "system_retry_exclusion"
	default:
		return retry
	}
	return stop
}

// IsClientAbort 判断本次失败是否由客户端主动断开引起。
//
// 取消是下游行为，不是渠道故障。判据用请求级事实，不依赖错误链——
// OpenAIError 构造经常丢掉 Unwrap。DeadlineExceeded 是网关预算 / TTFT，
// 不得当成 abort。
func IsClientAbort(c *gin.Context, relayInfo *relaycommon.RelayInfo, err error) bool {
	if types.IsClientAbortedError(err) {
		return true
	}
	if relayInfo != nil && relayInfo.StreamStatus != nil && relayInfo.StreamStatus.IsClientAbort() {
		return true
	}
	if c != nil && c.Request != nil && c.Request.Context() != nil {
		ctxErr := c.Request.Context().Err()
		if ctxErr != nil && errors.Is(ctxErr, context.Canceled) {
			return true
		}
	}
	return false
}

// isStreamContentCommitted 判断响应是否已向客户端提交了不可撤销的有效业务内容。
// 规则：
//  1. 若 c == nil 或 c.Writer == nil 或 !c.Writer.Written()，响应未提交。
//  2. 非流式请求一旦 Written() 即视为已向客户端写出 HTTP 状态或数据，不可换渠道重试。
//  3. 流式请求中：
//     若仅仅写入了 HTTP 响应头、SSE 保活心跳（如 ": PING\n\n"）或结构性帧
//     （role-only delta、message_start/status 等用户不可见的事件），
//     只要尚未向客户端交付任何可见内容（ReceivedVisibleContentBytes == 0），
//     则不视为 response_committed，依然允许网关换渠道重试！
//     下一个渠道的输出会继续流向这个已建立的 SSE 连接，客户端 JSON 解析器无感且连接得以保活。
func isStreamContentCommitted(c *gin.Context) bool {
	if c == nil || c.Writer == nil || !c.Writer.Written() {
		return false
	}
	relayInfo, ok := common.GetContextKeyType[*relaycommon.RelayInfo](c, constant.ContextKeyRelayInfo)
	if !ok || relayInfo == nil || !relayInfo.IsStream {
		return true
	}
	return relayInfo.ReceivedVisibleContentBytes > 0
}

// DecideRelayRetry is the single retry decision for relay attempts. The reason
// is recorded in the request policy decision events of the log details.
func DecideRelayRetry(c *gin.Context, err *types.NewAPIError, retryTimes int) PolicyDecision {
	if err == nil {
		return PolicyDecision{Action: "stop", Reason: "request_completed", Source: "system"}
	}
	// 响应已提交（首字节/SSE 事件已写出给下游客户端）：不能再向同一个连接缝合其他渠道的响应流，
	// 必须立即停止换渠道重试，交由终结错误处理器向客户端发送标准 SSE 错误帧。
	// 注意：流式请求中若仅发送了保活心跳（如 ": PING\n\n"）或结构性帧（role/typed
	// 状态事件），未向客户端交付任何可见业务内容（ReceivedVisibleContentBytes == 0），
	// 则不视为 response_committed，允许继续换渠道重试。
	if isStreamContentCommitted(c) {
		return PolicyDecision{Action: "stop", Reason: "response_committed", Source: "system"}
	}
	if ShouldSkipRetryAfterChannelAffinityFailure(c) {
		source := RequestPolicy(c).SessionModeSource
		if source == "" {
			source = "session_rule"
		}
		return PolicyDecision{Action: "stop", Reason: "strict_session", Source: source}
	}
	if GetChannelConstraints(c).SuppressesRetry() {
		return PolicyDecision{Action: "stop", Reason: "pinned_channel", Source: "channel_constraint"}
	}
	// skipRetry 是错误构造点留下的「此错误重试无意义」显式标记（本地参数校验、
	// 额度不足、令牌无权使用该模型等），必须先于负载类启发式判断。否则任何命中
	// 启发式的本地错误都会被反复换渠道重试：例如网关自身的 403（令牌模型白名单
	// 拒绝）会被 IsUpstreamPermissionError 的「403 即上游权限错误」规则当作上游
	// 故障，白白烧完整轮重试预算、消耗半开探测配额，最后返回的还是同一个 403。
	// 负载类错误（TTFT 超时/空流/断流）由 ToNewAPIError 构造，从不携带该标记，
	// 因此前移不影响它们的换渠道重试。
	if types.IsClientAbortedError(err) {
		return PolicyDecision{Action: "stop", Reason: "client_aborted", Source: "local"}
	}
	if loadbalancer.IsEmptyStreamBudget(err) {
		return PolicyDecision{Action: "stop", Reason: "empty_stream_budget_exhausted", Source: "loadbalancer"}
	}
	if types.IsSkipRetryError(err) {
		return PolicyDecision{Action: "stop", Reason: "non_retryable_error", Source: "system"}
	}
	// 重试预算检查前置：负载类的可重试错误（超时/空流/断流）同样受预算约束
	if retryTimes <= 0 {
		return PolicyDecision{Action: "stop", Reason: "attempt_budget_exhausted", Source: "global"}
	}
	// 智能负载：max_completion_tokens 超出该模型上限（如
	// "max_completion_tokens is too large: 384000. This model supports at most
	// 262144 completion tokens."）。这不是渠道故障而是请求参数与模型能力不匹配，
	// 不计入熔断（controller 的 lbAttempt.End 已把 400 排除在失败计数外）。
	//
	// 必须排在下面两道闸门之前，否则这个分支是死代码：
	//  1. alwaysSkipCodes 默认含 ErrorCodeBadResponseBody——上游 400 绝大多数
	//     落在这个错误码上，会在闸门处直接 stop；
	//  2. 即便错误码不在其中，默认 retryRanges 明确排除 400，最后也会落到
	//     status_not_retryable。
	// 死代码的代价不只是少一个 reason：controller 的重试循环是
	// 「processChannelError（学到上限）→ 继续下一次尝试」，下一轮
	// ConvertOpenAIRequest 出站前就会用刚学到的上限钳制，请求当场成功。
	// 分支死了就等于第一次请求必定把 400 打给客户端，只有第二次请求才受益。
	if _, ok := loadbalancer.ParseMaxCompletionTokensLimit(err); ok {
		return PolicyDecision{Action: "retry", Reason: "max_completion_tokens_clamped", Source: "loadbalancer"}
	}
	// 以下三类必须**先于** alwaysSkipCodes / 状态码区间闸门判定（即位于
	// 下面第 328 行那两道闸门之前）。
	//
	// 理由与上面那条完全相同：它们覆盖的上游错误落在 `bad_response_body` /
	// `bad_response_status_code` 这几个错误码上 —— 前者在
	// operation_setting.alwaysSkipCodes 的默认集合里，后者落到「400 不在
	// retryRanges」的 status_not_retryable 兜底。放在闸门之后这些分支就是死代码：
	// 生产实测渠道 #274 的 Responses 水合 400 因此从未换渠道重试，同一段多轮
	// 历史每次重放都失败；又因为不换渠道、不清亲和性，连自愈的机会都没有。
	if loadbalancer.IsReasoningHydrationError(err) {
		return PolicyDecision{Action: "retry", Reason: "reasoning_hydration_failed", Source: "loadbalancer"}
	}
	// 不透明 400 水合失败（中转站抹掉了 code）：同属水合族，换渠道重试并剥离密文，
	// 不能因 message 只剩 "bad response status code 400" 就被下面的中继包装分支
	// 当成普通上游中继故障（见 IsContextualReasoningHydration 注释）。
	if IsContextualReasoningHydration(c, err) {
		return PolicyDecision{Action: "retry", Reason: "reasoning_hydration_failed", Source: "loadbalancer"}
	}
	if loadbalancer.IsCodexValidationBadRequest(err) {
		return PolicyDecision{Action: "retry", Reason: "codex_validation_error", Source: "loadbalancer"}
	}
	if loadbalancer.IsResponsesAPINotSupportedError(err) {
		return PolicyDecision{Action: "retry", Reason: "responses_api_not_supported", Source: "loadbalancer"}
	}
	// 智能负载：首字超时视为可重试，触发切换到下一个渠道。
	//
	// 必须排在 IsChannelError 之前：TTFT 超时的错误码是
	// ErrorCodeChannelResponseTimeExceeded = "channel:response_time_exceeded"，
	// 而 IsChannelError 判的就是 "channel:" 前缀，所以放在它后面这个分支永远
	// 走不到——既丢掉了 ttft_timeout 这个可观测的原因，也把上面两道闸门
	// （skipRetry 标记、重试预算）一并绕过了：TTFT 超时可以在预算耗尽后继续换
	// 渠道重试，无限循环在只有一个渠道的分组上。
	if loadbalancer.IsTTFTTimeout(err) {
		return PolicyDecision{Action: "retry", Reason: "ttft_timeout", Source: "loadbalancer"}
	}
	// 其余 "channel:" 前缀错误（模型映射失效、参数/请求头覆写非法、密钥无效、
	// AWS 客户端错误等）：换渠道重试。上面的 skipRetry 与预算闸门对它们同样有效。
	if types.IsChannelError(err) {
		return PolicyDecision{Action: "retry", Reason: "channel_error", Source: "system"}
	}
	// 智能负载：参数不支持的错误（无论是 400、422、5xx 还是流中断 502 返回）换渠道重试并自动学习裁剪参数，但不计入熔断。
	// 必须排在 IsStreamBroken 之前：当流式响应因上游不认 reasoning_effort/thinking 等参数而在流首/流中返回错误帧时，
	// 错误会被流扫描器包装为 StreamBrokenError；若不在此处优先截获，将被判为 stream_broken 渠道故障并误熔断健康渠道。
	if _, ok := loadbalancer.IsParamNotSupportedError(err); ok {
		return PolicyDecision{Action: "retry", Reason: "bad_request_retry", Source: "loadbalancer"}
	}
	// 智能负载：空流视同渠道失败，换渠道重试（客户端尚未收到任何数据）
	if loadbalancer.IsEmptyStream(err) {
		return PolicyDecision{Action: "retry", Reason: "empty_stream", Source: "loadbalancer"}
	}
	// 智能负载：流中断（上游传输异常断开，如 RST_STREAM 等）视同渠道失败，换渠道重试
	if loadbalancer.IsStreamBroken(err) {
		return PolicyDecision{Action: "retry", Reason: "stream_broken", Source: "loadbalancer"}
	}
	// 智能负载：上游额度耗尽（即使返回 400/402/403 等，属于渠道不可用，换渠道重试）
	if loadbalancer.IsUpstreamQuotaError(err) {
		return PolicyDecision{Action: "retry", Reason: "upstream_quota_exhausted", Source: "loadbalancer"}
	}
	// 智能负载：上游路由/会话头缺失（部分网关剥离 x-opencode-session 返回 400，换渠道重试）
	if loadbalancer.IsUpstreamRoutingError(err) {
		return PolicyDecision{Action: "retry", Reason: "upstream_routing_error", Source: "loadbalancer"}
	}
	// 智能负载：上游渠道权限受限/分组无权访问/TokenPlan不支持（换渠道重试）
	if loadbalancer.IsUpstreamPermissionError(err) {
		return PolicyDecision{Action: "retry", Reason: "upstream_permission_denied", Source: "loadbalancer"}
	}
	code := err.StatusCode
	if code >= 200 && code < 300 {
		return PolicyDecision{Action: "stop", Reason: "system_retry_exclusion", Source: "system"}
	}
	if code < 100 || code > 599 {
		return PolicyDecision{Action: "retry", Reason: "unrecognized_status", Source: "system"}
	}
	if operation_setting.IsAlwaysSkipRetryCode(err.GetErrorCode()) || operation_setting.IsAlwaysSkipRetryStatusCode(code) {
		return PolicyDecision{Action: "stop", Reason: "system_retry_exclusion", Source: "system"}
	}
	// 智能负载：思考模式历史消息校验不兼容（如 "The `reasoning_content` in the thinking mode must be passed back"），换渠道重试
	if loadbalancer.IsThinkingModeHistoryError(err) {
		return PolicyDecision{Action: "retry", Reason: "thinking_history_incompatible", Source: "loadbalancer"}
	}
	// 水合失败 / Codex 校验 / Responses API 不支持这三类**不在这里判**：
	// 它们必须在 alwaysSkipCodes 与状态码区间两道闸门之前就截获，见上方
	// ParseMaxCompletionTokensLimit 之后的那组分支（此处若再写一遍就是
	// 永远走不到的死代码，且会让读者误以为闸门之后仍有兜底）。
	// 智能负载：上游模型不可用/已禁用/未配置（如 "model not found"、"model is disabled on this gateway"），换渠道重试
	if loadbalancer.IsUpstreamModelUnavailableError(err) {
		return PolicyDecision{Action: "retry", Reason: "model_unavailable_retry", Source: "loadbalancer"}
	}
	// 智能负载：上游中继站报告代理异常（如 "来自上游渠道的报错: bad response status code 400"，换渠道重试）
	if loadbalancer.IsUpstreamRelayError(err) {
		return PolicyDecision{Action: "retry", Reason: "upstream_relay_error", Source: "loadbalancer"}
	}
	// 明确的结构化容量错误即使返回 5xx，也要保留容量专用 reason；否则会被通用
	// 状态码规则记为 retry_status_matched，丢失对应的可观测语义。普通 429 仍保留
	// 既有的 retry_status_matched 契约，其他文本限流也继续走原有状态码/限流分支。
	if loadbalancer.IsUpstreamCapacityError(err) {
		return PolicyDecision{Action: "retry", Reason: "upstream_rate_limited", Source: "loadbalancer"}
	}
	if operation_setting.ShouldRetryByStatusCode(code) {
		return PolicyDecision{Action: "retry", Reason: "retry_status_matched", Source: "global"}
	}
	// 智能负载：上游限流/并发超限（如 400 包装的并发限制、RPM 限流），换渠道重试。
	if loadbalancer.IsUpstreamRateLimitError(err) {
		return PolicyDecision{Action: "retry", Reason: "upstream_rate_limited", Source: "loadbalancer"}
	}
	return PolicyDecision{Action: "stop", Reason: "status_not_retryable", Source: "global"}
}

func ShouldRetryRelayError(c *gin.Context, openaiErr *types.NewAPIError, retryTimes int) bool {
	return DecideRelayRetry(c, openaiErr, retryTimes).Action == "retry"
}

func ProcessChannelError(c *gin.Context, channelError types.ChannelError, err *types.NewAPIError, relayInfo *relaycommon.RelayInfo) {
	if err == nil {
		return
	}
	// 智能负载：记住该渠道该上游模型的 max_completion_tokens 真实上限，
	// 后续出站前直接钳制（见 openai adaptor），客户端要的长回复一次就成。
	//
	// 放在这里而不是 DecideRelayRetry 里，两个理由都是必须的：
	//  1. 上限是「上游模型」的属性，必须用实际发出去的那个名字
	//     （relayInfo.UpstreamModelName，已经过 model_mapping 与 reasoning
	//     后缀剥离）。此前写在 DecideRelayRetry 里用的是
	//     ContextKeyOriginalModel（客户端请求名），而出站钳制读的是
	//     UpstreamModelName——渠道 #111 配了 model_mapping 把 deepseek-v4-flash
	//     映射成 Deepseek-v4-flash，两个名字就此分叉，钳制永远查不到，等于没修。
	//  2. 学习是「观测到事实」的副作用，与「这次要不要重试」无关。写在重试
	//     决策里会被 client_aborted / skipRetry / retryTimes<=0 等闸门挡掉，
	//     而恰恰是反复失败到预算耗尽的请求最需要把上限学到手。
	//
	// ChannelMeta 也要判空：UpstreamModelName 是从内嵌的 *ChannelMeta 提升上来
	// 的字段，而它是**指针**内嵌。渠道选定之前就失败（选渠道失败、令牌中途失效
	// 等）时 relayInfo 非 nil 但 ChannelMeta 仍为 nil，直接取字段会空指针 panic，
	// 把一个本该正常记账的失败变成 500。
	if relayInfo != nil && relayInfo.ChannelMeta != nil {
		if limit, ok := loadbalancer.ParseMaxCompletionTokensLimit(err); ok {
			loadbalancer.RecordMaxCompletionTokensLimit(channelError.ChannelId, relayInfo.UpstreamModelName, limit)
		}
	}
	logger.LogError(c, fmt.Sprintf("channel error (channel #%d, status code: %d, code: %s): %s", channelError.ChannelId, err.StatusCode, err.GetErrorCode(), common.LocalLogPreview(err.MaskSensitiveErrorWithStatusCode())))
	// 佐证计数只在这一处发生，四条禁用路径全部汇流到 ProcessChannelError。
	// 换到任何一处调用点去计数都会被重复触发：relay 路径上同一次失败先被
	// RecordPolicyFailure 判一次（那次不计数，见 ShouldDisableChannel 的注释），
	// 测活路径也会先算一遍 shouldBanChannel 再走到这里。
	//
	// 必须在 gopool.Go 之前把模型名取成字符串：gin.Context 随请求结束被回收，
	// 闭包里再读 c 拿到的是已复用的内存，读出来的模型名会是别的请求的。
	// modelScoped / modelName 都是值类型，进闭包是安全的。
	modelScoped := isModelScopedAutoDisable(err)
	modelName := c.GetString("original_model")
	// AutoBan 必须写在左边：ShouldDisableChannelCorroborated 不是纯读，它内部
	// 会 RecordAutoDisableSignal（计数 +1）并打出「holding back disable」那行
	// 日志。写在左边时 Go 从左往右求值，一个 auto_ban=0 的渠道也会照常累加
	// 计数、并被告知「闸门在拦我」——而它永远不会被摘掉。两处后果：日志在
	// 排障时把人引向错误方向；以及运维日后给这条渠道打开 auto_ban，第一次
	// 真实失败就顶到阈值被摘掉，恰好是佐证机制要防的「一次即禁」。
	if channelError.AutoBan && ShouldDisableChannelCorroborated(channelError.ChannelId, modelName, err) {
		reason := err.MaskSensitiveErrorWithStatusCode()
		gopool.Go(func() {
			if modelScoped {
				DisableChannelForModel(channelError, modelName, reason)
				return
			}
			DisableChannel(channelError, reason)
		})
	}

	if constant.ErrorLogEnabled && types.IsRecordErrorLog(err) {
		writeErrorLogRow(c, channelError.ChannelId, err, relayInfo)
	}
}

// writeErrorLogRow 写入 type=5 行并落去重标记。渠道失败与请求级失败共用这一条
// 记账路径，所以「谁写谁打标记」必须收在这里：调用方只负责判断要不要写。
//
// 名字里没有「只写一次」：去重靠的是 ContextKeyErrorLogRecorded 标记，而
// ProcessChannelError 每次渠道尝试都会调到这里，所以 N 次尝试仍然落 N 行。
func writeErrorLogRow(c *gin.Context, channelId int, err *types.NewAPIError, relayInfo *relaycommon.RelayInfo) {
	userId := c.GetInt("id")
	tokenName := c.GetString("token_name")
	modelName := c.GetString("original_model")
	tokenId := c.GetInt("token_id")
	userGroup := c.GetString("group")
	other := model.NewLogOther()
	if c.Request != nil && c.Request.URL != nil {
		other.SetPublic("request_path", c.Request.URL.Path)
	}
	other.SetPublic("error_type", err.GetErrorType())
	other.SetPublic("error_code", err.GetErrorCode())
	other.SetPublic("status_code", err.StatusCode)
	AppendRelayLogAdminInfo(c, relayInfo, other)
	AppendResponseModelLogInfo(relayInfo, other)
	AppendTaskPluginContextAuditInfo(c, other)
	startTime := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
	if startTime.IsZero() {
		startTime = time.Now()
	}
	useTimeSeconds := int(time.Since(startTime).Seconds())
	// 标记必须在写库之前落：写库失败（DB 抖动）时宁可这次不重试，也不能让同一次
	// 请求在终结 defer 里再补一行。
	common.SetContextKey(c, constant.ContextKeyErrorLogRecorded, true)
	model.RecordErrorLog(c, userId, channelId, modelName, tokenName, err.MaskSensitiveErrorWithStatusCode(), tokenId, useTimeSeconds, common.GetContextKeyBool(c, constant.ContextKeyIsStream), userGroup, other)
}

// RecordRequestErrorLog 为「没有走到渠道」就失败的请求补一条 type=5 行：
// 参数校验、模型/映射解析、选渠道、预扣费等路径都不会经过 ProcessChannelError，
// 通用日志页此前只看得到成功的 type=2，请求级失败完全不可见。
//
// 去重是硬要求：渠道重试里每次尝试失败都已由 ProcessChannelError 落一行，
// 终结路径再补一行就是 N+1 行。因此命中 ContextKeyErrorLogRecorded 就直接跳过。
//
// 渠道号一律从 c 上读（ContextKeyChannelId），不接受调用方传参：所有调用点的
// 值都来自同一份 gin context，参数化等于开一个编译器管不着的第二数据源，
// 不一致时不会编译失败，只会静默写出归因错误的日志行。
func RecordRequestErrorLog(c *gin.Context, err *types.NewAPIError, relayInfo *relaycommon.RelayInfo) {
	if c == nil || err == nil {
		return
	}
	if !constant.ErrorLogEnabled || !types.IsRecordErrorLog(err) {
		return
	}
	// 客户端主动取消不记：取消是下游行为不是上游故障，与 Relay 终结 defer 的判断同源。
	//
	// 这道闸门**不能**当成 !IsRecordErrorLog 的重复而删掉：本地构造的
	// NewClientAbortedError 确实自带 opt-out，但 errorCode 的判定依据是「错误码等于
	// client_aborted」，而不是「谁构造的」。relaykit/types/error.go 里
	// WithOpenAIError (:359) 与 WithClaudeError (:383) 会把**上游响应体里的
	// code/type 字符串**直接写成 errorCode，且都不施加 opt-out。上游于是可以自己决定
	// 「这条失败要不要进通用日志页」。真实形状见 relay/responses_websocket.go:439 的
	// 上游 error 帧拒绝：上游给什么 code 就是什么 code，没有 opt-out，也不经过
	// ProcessChannelError（所以去重标记同样拦不住），三道闸门里只有这里拦得住。
	if types.IsClientAbortedError(err) {
		return
	}
	if common.GetContextKeyBool(c, constant.ContextKeyErrorLogRecorded) {
		return
	}
	writeErrorLogRow(c, common.GetContextKeyInt(c, constant.ContextKeyChannelId), err, relayInfo)
}
