package loadbalancer

import (
	"errors"
	"net/http"
	"testing"

	"github.com/dengyie/apihub/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本文件把四类**线上实测过**的上游报文钉在分类器上。每一类都对应一次
// 真实的误判，且误判后果彼此不同：
//
//	#274 跨账号推理水合失败  —— 不剥密文、不换渠道，多轮历史每次重放都失败
//	Codex 会话身份冲突       —— 落在 400 就按参数错误停掉重试
//	#368 Responses 不支持    —— 被当成「模型不存在」持续收集不可逆禁用佐证
//	#377 / #394 周期与并发额度 —— 被当成欠费，攒够佐证把健康渠道摘掉
//
// 报文一律逐字取自生产日志/工单，不加工：分类器靠子串匹配活着，只有原文
// 才能证明它认得住。

func newTextError(message string, statusCode int) *types.NewAPIError {
	return types.NewErrorWithStatusCode(errors.New(message), types.ErrorCodeBadResponseBody, statusCode)
}

// TestHydrationErrorWording_Issue274 覆盖 #274 的两种措辞。
//
// 生产日志里同一件事有两种说法：
//  1. 经典措辞 "encrypted content ... could not be verified + reasoning hydration failed"；
//  2. 「这个 item 是在另一个 OpenAI 资源下创建的，必须用创建它的资源访问」——
//     一个字都不含 hydration，旧判据完全认不出。
func TestHydrationErrorWording_Issue274(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    bool
	}{
		{
			name:    "经典 encrypted content 校验失败",
			message: "The encrypted content for item rs_07f1 could not be verified. Reason: reasoning hydration failed: Encrypted content could not be decrypted or parsed.",
			want:    true,
		},
		{
			name:    "OpenAI 资源归属不一致",
			message: "This item was created under a different OpenAI resource. Please use the same resource that created the item to access it.",
			want:    true,
		},
		{
			name:    "中文解密失败",
			message: "该加密内容无法解密，请移除历史推理项后重试。",
			want:    true,
		},
		{
			name:    "无关 400 不得误认",
			message: "Invalid API key provided: sk-xxx",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := newTextError(tc.message, http.StatusBadRequest)
			assert.Equal(t, tc.want, IsReasoningHydrationError(err))
			if tc.want {
				// 水合失败绝不能落进「中继代理失效」，否则一次跨账号历史
				// 就把整条渠道熔了。
				assert.False(t, IsUpstreamRelayError(err), "水合失败不是中继代理故障")
			}
		})
	}

	// 结构化字段里的措辞同样要认：部分中转站把原文塞进 error.message，
	// 而 Error() 只回一个泛化外壳。
	structured := types.NewErrorWithStatusCode(errors.New("upstream error"), types.ErrorCodeBadResponseBody, http.StatusBadRequest)
	structured.RelayError = types.OpenAIError{
		Type:    "invalid_request_error",
		Message: "This item was created under a different OpenAI resource. Please use the same resource that created the item to access it.",
	}
	assert.True(t, IsReasoningHydrationError(structured), "结构化 message 里的措辞也必须识别")
}

// TestCodexSessionIdentityConflictIsRetryable 钉住 Codex 会话身份冲突。
//
// 上游原话（生产实测）：
//
//	code: codex_session_identity_conflict
//	message: 会话标识、来源或请求类型字段存在冲突
//
// 它落 400，但成因是「同一会话被复用到不同身份」，换渠道/重连即自愈。
// 一旦被当成普通 400 停掉重试，客户端只会拿到一个无法自救的 400。
func TestCodexSessionIdentityConflictIsRetryable(t *testing.T) {
	cases := []string{
		"codex_session_identity_conflict: 会话标识、来源或请求类型字段存在冲突",
		"会话标识、来源或请求类型字段存在冲突",
		"session identity conflict detected for this request",
	}
	for _, message := range cases {
		t.Run(message, func(t *testing.T) {
			err := newTextError(message, http.StatusBadRequest)
			assert.True(t, IsCodexValidationBadRequest(err), "身份冲突必须可重试")
			assert.False(t, IsUpstreamRelayError(err), "身份冲突不是渠道故障")
		})
	}

	// 结构化 code 单独出现时也要认（上游只回 code，不回中文描述）。
	structured := types.NewErrorWithStatusCode(errors.New("bad request"), types.ErrorCodeBadResponseBody, http.StatusBadRequest)
	structured.RelayError = types.OpenAIError{Type: "invalid_request_error", Code: "codex_session_identity_conflict", Message: "bad request"}
	assert.True(t, IsCodexValidationBadRequest(structured))

	// 反向守卫：普通 400 不能被这条判据吞掉，否则所有客户端错误都会被
	// 白白换渠道重试一整轮。
	assert.False(t, IsCodexValidationBadRequest(newTextError("Invalid API key provided", http.StatusBadRequest)))
}

// TestResponsesAPINotSupported_Issue368 钉住 #368。
//
// 生产实测（cpolar 隧道后的 Codex 兼容层）：
//
//	400 api_not_supported
//	Responses API does not supported, for Codex users: Downgrade to 0.80.0.
//	| 不支持Responses API，Codex用户请降级到0.80.0。
func TestResponsesAPINotSupported_Issue368(t *testing.T) {
	const message = "api_not_supported: Responses API does not supported, for Codex users: Downgrade to 0.80.0. 不支持Responses API，Codex用户请降级到0.80.0。"
	err := newTextError(message, http.StatusBadRequest)

	assert.True(t, IsResponsesAPINotSupportedError(err))
	// 这条判据的全部意义在于：模型在、协议不在。
	assert.False(t, IsUpstreamModelUnavailableError(err),
		"必须与「模型不存在」区分开 —— 后者不可逆自动禁用，前者只是换个渠道")
	// 也不是参数问题：报文里的 "does not supported" 会被散文化模式误抓成
	// 参数名 "ed"，进而学到一条不存在的裁剪，并绕开本次新增的模型级冷却。
	param, ok := IsParamNotSupportedError(err)
	assert.False(t, ok, "不得被误判成参数不支持，误抓到的参数名是 %q", param)

	// 只认 Responses 的那个 chunk，别的 api_not_supported 不在此列。
	assert.False(t, IsResponsesAPINotSupportedError(newTextError("api_not_supported: model not supported", http.StatusBadRequest)))
}

// TestConcurrencyFullIsRateLimit_Issue377 钉住 #377。
//
// 原文：503 当前并发已满（合法性 2/2），请稍后重试。
// 它是**并发槽位瞬时打满**，与账号额度无关，几秒内即自愈。误判成额度耗尽
// 会按 1800s 硬熔断，误判成中继代理故障会熔整条渠道，两种都会把一条健康
// 渠道踢出轮转。
func TestConcurrencyFullIsRateLimit_Issue377(t *testing.T) {
	for _, message := range []string{
		"当前并发已满（合法性 2/2），请稍后重试",
		"当前并发已满（6/6），请稍后重试",
		"concurrency limit reached, please retry later",
	} {
		t.Run(message, func(t *testing.T) {
			err := newTextError(message, http.StatusServiceUnavailable)
			assert.True(t, IsUpstreamRateLimitError(err), "并发打满属于瞬时限流")
			assert.False(t, IsUpstreamQuotaError(err), "不得当成额度耗尽")
			assert.False(t, IsUpstreamRelayError(err), "不得当成渠道故障熔整条渠道")
		})
	}
}

// TestSelfHealingQuota_Issues394 钉住「会自动重置的额度」。
//
// 这类额度耗尽**不做任何动作它自己就会好**，因此绝不能计入不可逆的自动
// 禁用判据；而真正的欠费（insufficient_quota）仍然必须能识别出来。
func TestSelfHealingQuota_Issues394(t *testing.T) {
	selfHealing := []string{
		"The shared daily limit for the free models is exhausted. It resets at 00:00 Moscow time.",
		"free_daily_limit_exceeded",
		"今日可用额度不足，请明天再试",
		"quota_7d exceeded",
		"Daily usage limit exceeded, please try again in 24 hours",
		"Throttling.BurstRate: Rate exceeded",
		"You have hit your weekly limit. Resets in 3 days.",
	}
	for _, message := range selfHealing {
		t.Run("self-healing/"+message, func(t *testing.T) {
			assert.True(t, IsSelfHealingQuotaError(newTextError(message, http.StatusTooManyRequests)))
		})
	}

	// 反向守卫：真正的欠费不会自愈，必须留给自动禁用那条路。
	permanent := []string{
		"insufficient_quota: You exceeded your current quota, please check your plan and billing details.",
		"Incorrect API key provided",
	}
	for _, message := range permanent {
		t.Run("permanent/"+message, func(t *testing.T) {
			assert.False(t, IsSelfHealingQuotaError(newTextError(message, http.StatusTooManyRequests)),
				"欠费不会自愈，不能豁免自动禁用")
		})
	}
}

// TestParseMaxCompletionTokensLimitRangeWording 覆盖 #394 的另一种报文：
// 上游不给「supports at most」，而是给一个闭区间。
//
//	For this route max_tokens must be an integer from 1 to 65536.
//
// 旧正则只认 "supports at most N"，这类报文完全解析不到，于是学习从未发生，
// 后续每个请求继续原样 400。
func TestParseMaxCompletionTokensLimitRangeWording(t *testing.T) {
	cases := []struct {
		message string
		want    uint
		ok      bool
	}{
		{"For this route max_tokens must be an integer from 1 to 65536.", 65536, true},
		{"max_output_tokens must be an integer between 1 and 8192", 8192, true},
		{"max_completion_tokens is too large: 384000. This model supports at most 262144 completion tokens.", 262144, true},
		{"max_tokens: 999999 is greater than the maximum value of 65536", 65536, true},
		{"invalid request: missing field model", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.message, func(t *testing.T) {
			limit, ok := ParseMaxCompletionTokensLimit(newTextError(tc.message, http.StatusBadRequest))
			require.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, limit)
		})
	}
}
