package service

import (
	"errors"
	"net/http"
	"testing"

	"github.com/dengyie/apihub/loadbalancer"
	"github.com/dengyie/apihub/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 这组用例覆盖三类**不应触发不可逆自动禁用**的线上报文。
//
// 它们的共同点：熔断能自愈，自动禁用不能。一个「换个渠道就好的协议不兼容」、
// 一个「几秒后就是空的并发槽位」、一个「明天 0 点自动清零的日额度」，任何
// 一条被记进佐证并攒到阈值，就等于每天要人工救一次，而线上不会有任何告警。
//
// 三条判据都已经在 classifyAutoDisable 里显式排除（顺序先于
// IsUpstreamModelUnavailableError 那条不可逆判据）。这里用 ShouldDisableChannelCorroborated
// 连跑 20 次来验证：即便重复命中，佐证计数也绝不能增长。

func newProtocolError(message string, code types.ErrorCode, status int) *types.NewAPIError {
	return types.NewErrorWithStatusCode(errors.New(message), code, status)
}

func assertNeverDisables(t *testing.T, channelID int, model string, err *types.NewAPIError, reason string) {
	t.Helper()
	for i := 0; i < 20; i++ {
		require.False(t, ShouldDisableChannelCorroborated(channelID, model, err),
			"第 %d 次命中：%s", i+1, reason)
	}
	assert.False(t, classifyAutoDisable(channelID, err).Disable, "%s：判定函数本身也必须拒绝禁用", reason)
	for _, class := range []string{
		loadbalancer.CorroborationClassChannelError,
		loadbalancer.CorroborationClassStatusCode,
		loadbalancer.CorroborationClassKeyword,
		loadbalancer.CorroborationClassModelUnavailable,
	} {
		assert.Equal(t, 0, loadbalancer.CorroborationCount(channelID, model, class),
			"%s：不得进任何佐证桶（class=%s）", reason, class)
	}
}

// TestResponsesAPINotSupportedNeverAutoDisables 覆盖 #368。
//
// 该渠道的 chat/completions 完全正常，只有 Responses 协议不被支持。它落 400
// 且报文里带 "api_not_supported"，若走通用判据会被当成确定性失效持续收集
// 佐证 —— 一条健康渠道被一个「协议开关」摘掉。
func TestResponsesAPINotSupportedNeverAutoDisables(t *testing.T) {
	newCorroborationFixture(t, 3)
	const channelID = 9301
	defer loadbalancer.ResetCorroborationForChannel(channelID)

	err := newProtocolError(
		"api_not_supported: Responses API does not supported, for Codex users: Downgrade to 0.80.0. 不支持Responses API，Codex用户请降级到0.80.0。",
		types.ErrorCodeBadResponseStatusCode,
		http.StatusBadRequest,
	)
	assertNeverDisables(t, channelID, "gpt-5-codex", err, "Responses API 不被支持只是协议差异")
}

// TestConcurrencyFullNeverAutoDisables 覆盖 #377。
//
// 原文：503 当前并发已满（合法性 2/2），请稍后重试。并发槽位几秒内就会空出来，
// 这是瞬时限流。按 1800s 熔断已属过重，若再记进不可逆的佐证则是纯损失。
func TestConcurrencyFullNeverAutoDisables(t *testing.T) {
	newCorroborationFixture(t, 3)
	const channelID = 9302
	defer loadbalancer.ResetCorroborationForChannel(channelID)

	err := newProtocolError(
		"当前并发已满（合法性 2/2），请稍后重试",
		types.ErrorCodeBadResponseStatusCode,
		http.StatusServiceUnavailable,
	)
	assertNeverDisables(t, channelID, "claude-sonnet-4-5", err, "并发已满属于瞬时限流")
}

// TestSelfHealingQuotaNeverAutoDisables 覆盖 #394。
//
// 免费模型池日额度耗尽："The shared daily limit for the free models is
// exhausted ... resets at 00:00 Moscow time"。这条渠道从没「缺钱」，只是
// 当天的额度用完了；到点自动归零。把它永久摘掉等于每天人工救一次。
func TestSelfHealingQuotaNeverAutoDisables(t *testing.T) {
	newCorroborationFixture(t, 3)
	const channelID = 9303
	defer loadbalancer.ResetCorroborationForChannel(channelID)

	err := newProtocolError(
		"The shared daily limit for the free models is exhausted. It resets at 00:00 Moscow time.",
		types.ErrorCodeBadResponseStatusCode,
		http.StatusTooManyRequests,
	)
	assertNeverDisables(t, channelID, "gpt-4o-mini", err, "周期额度会自动重置")
}

// TestPermanentQuotaStillAutoDisables 是上面三条的反向守卫。
//
// 把「自愈型额度」豁免掉之后，真正的欠费必须仍然能被摘掉 —— 否则这组修复
// 就从「误杀」变成「漏杀」，而漏杀的代价是所有请求持续撞一条没钱的上游。
func TestPermanentQuotaStillAutoDisables(t *testing.T) {
	newCorroborationFixture(t, 3)
	const channelID = 9304
	defer loadbalancer.ResetCorroborationForChannel(channelID)

	err := newProtocolError(
		"insufficient_quota: You exceeded your current quota, please check your plan and billing details.",
		types.ErrorCodeBadResponseStatusCode,
		http.StatusTooManyRequests,
	)
	require.True(t, loadbalancer.IsUpstreamQuotaError(err), "前置条件：这是真·额度耗尽")
	require.False(t, loadbalancer.IsSelfHealingQuotaError(err), "欠费不会自愈，不得落入豁免")

	// 额度耗尽本身不是「禁用状态码」，但熔断/重试必须先把它换走。
	assert.Equal(t, BreakerQuota, mustClassify(t, err))
}

func mustClassify(t *testing.T, err *types.NewAPIError) BreakerEffectType {
	t.Helper()
	effect, _ := ClassifyBreakerEffect(err)
	return effect
}
