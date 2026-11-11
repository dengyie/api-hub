package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dengyie/apihub/relaykit/types"
	"github.com/dengyie/apihub/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 这组用例钉的是 DecideRelayRetry 的**判定顺序**，而不是分类器本身。
//
// 分类器早就认得这些报文（#274 的 hydration 原话一直在 IsReasoningHydrationError
// 的判据里），但分支被放在了 alwaysSkipCodes / 状态码区间两道闸门之后，于是
// 永远走不到——生产上表现为「分类对了，行为没变」。
//
// 顺序敏感的成因值得写在测试里：上游绝大多数 400/502 都被包装成
// ErrorCodeBadResponseBody，而该错误码默认就在 alwaysSkipCodes 里，闸门直接
// stop；即便不在，默认 retryRanges 也明确排除 400。任何「本该重试」的新判据
// 只要放在闸门之后，就只能靠改配置生效，而线上没人会去改。
func newRetryDecisionContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c
}

func TestDecideRelayRetryHydrationBeforeAlwaysSkipGates(t *testing.T) {
	c := newRetryDecisionContext(t)
	err := types.NewErrorWithStatusCode(
		errors.New("The encrypted content for item rs_07f1 could not be verified. Reason: reasoning hydration failed: Encrypted content could not be decrypted or parsed."),
		types.ErrorCodeBadResponseBody,
		http.StatusBadRequest,
	)
	require.True(t, operation_setting.IsAlwaysSkipRetryCode(err.GetErrorCode()),
		"前置条件：bad_response_body 默认在 alwaysSkipCodes 里 —— 这正是旧分支成为死代码的原因")

	decision := DecideRelayRetry(c, err, 3)
	assert.Equal(t, "retry", decision.Action, "水合失败必须换渠道重试")
	assert.Equal(t, "reasoning_hydration_failed", decision.Reason)
}

func TestDecideRelayRetryCodexIdentityConflictBeforeAlwaysSkipGates(t *testing.T) {
	c := newRetryDecisionContext(t)
	err := types.NewErrorWithStatusCode(
		errors.New("codex_session_identity_conflict: 会话标识、来源或请求类型字段存在冲突"),
		types.ErrorCodeBadResponseBody,
		http.StatusBadRequest,
	)
	require.True(t, operation_setting.IsAlwaysSkipRetryCode(err.GetErrorCode()))

	decision := DecideRelayRetry(c, err, 3)
	assert.Equal(t, "retry", decision.Action, "会话身份冲突换渠道/重连即自愈，不能停在 400")
	assert.Equal(t, "codex_validation_error", decision.Reason)
}

func TestDecideRelayRetryResponsesAPINotSupportedBeforeAlwaysSkipGates(t *testing.T) {
	c := newRetryDecisionContext(t)
	// #368 生产原文。上游把 400 包进了 bad_response_status_code，
	// 旧路径会落到「400 不在 retryRanges」的 status_not_retryable。
	err := types.NewErrorWithStatusCode(
		errors.New("api_not_supported: Responses API does not supported, for Codex users: Downgrade to 0.80.0."),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusBadRequest,
	)

	decision := DecideRelayRetry(c, err, 3)
	assert.Equal(t, "retry", decision.Action)
	assert.Equal(t, "responses_api_not_supported", decision.Reason)
}

// TestClassifyBreakerEffectMatchesRetryClassification 保证「重试」与「熔断」
// 两侧对同一报文给出**同一个成因**。两侧分叉时最典型的症状是：请求被换渠道
// 重试了，但故障渠道没被冷却，下一个请求继续撞；或者反过来，一个参数问题把
// 整条渠道熔了。
func TestClassifyBreakerEffectMatchesRetryClassification(t *testing.T) {
	cases := []struct {
		name      string
		message   string
		code      types.ErrorCode
		status    int
		want      BreakerEffectType
		wantRetry string
	}{
		{
			name:      "responses api not supported (#368)",
			message:   "api_not_supported: Responses API does not supported, for Codex users: Downgrade to 0.80.0.",
			code:      types.ErrorCodeBadResponseStatusCode,
			status:    http.StatusBadRequest,
			want:      BreakerResponsesAPINotSupported,
			wantRetry: "retry",
		},
		{
			name:      "concurrency full (#377)",
			message:   "当前并发已满（2/2），请稍后重试",
			code:      types.ErrorCodeBadResponseStatusCode,
			status:    http.StatusServiceUnavailable,
			want:      BreakerRateLimit,
			wantRetry: "retry",
		},
		{
			name:      "hydration failure (#274)",
			message:   "This item was created under a different OpenAI resource. Please use the same resource that created the item to access it.",
			code:      types.ErrorCodeBadResponseBody,
			status:    http.StatusBadRequest,
			want:      BreakerNone,
			wantRetry: "retry",
		},
		{
			name:      "self-healing daily quota (#394)",
			message:   "The shared daily limit for the free models is exhausted. It resets at 00:00 Moscow time.",
			code:      types.ErrorCodeBadResponseStatusCode,
			status:    http.StatusTooManyRequests,
			want:      BreakerRateLimit,
			wantRetry: "retry",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := types.NewErrorWithStatusCode(errors.New(tc.message), tc.code, tc.status)
			effect, _ := ClassifyBreakerEffect(err)
			assert.Equal(t, tc.want, effect)

			c := newRetryDecisionContext(t)
			assert.Equal(t, tc.wantRetry, DecideRelayRetry(c, err, 3).Action)
		})
	}
}
