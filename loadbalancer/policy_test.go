package loadbalancer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPolicyClientSafetyDeadlineAndEmptyStreamBudget(t *testing.T) {
	// 1. 默认策略：ClientSafetyDeadline 为 105s
	policy := DefaultPolicy()
	assert.Equal(t, 105*time.Second, policy.ClientSafetyDeadline())

	// 2. 自适应计算超大 TTFT (例如 100s) + 2 次重试，原本 needed = 300s，必须被截断到 105s
	policy.Default.TTFTTimeoutMs = 100000
	policy.MaxRetries = 2
	budget := policy.EmptyStreamRetryBudget()
	assert.Equal(t, 105*time.Second, budget, "即使 needed 达到 300s，也必须被 105s 客户端安全死线截断")

	// 3. 显式配置更小的 ClientSafetyDeadlineMs (例如 50s)
	customSafety := 50000
	policy.ClientSafetyDeadlineMs = &customSafety
	assert.Equal(t, 50*time.Second, policy.ClientSafetyDeadline())
	assert.Equal(t, 50*time.Second, policy.EmptyStreamRetryBudget(), "必须被显式配置的 safety deadline 截断")

	// 4. 关闭 ClientSafetyDeadline (显式 0)
	disabledSafety := 0
	policy.ClientSafetyDeadlineMs = &disabledSafety
	assert.Equal(t, time.Duration(0), policy.ClientSafetyDeadline())
	// 此时不受 safety 截断，但受 RequestTimeout (默认 180s) 截断
	assert.Equal(t, 180*time.Second, policy.EmptyStreamRetryBudget())
}

func TestChannelPolicyResolveHeaderTimeout(t *testing.T) {
	// 1. TTFT 较大 (60s)，HeaderTimeout 未配置：跟随 TTFT，不再固定 15s。
	//    线上就是这条：ttft_timeout_ms=100s 被未配置的 15s header 门限压回 15s。
	cp1 := ChannelPolicy{TTFTTimeoutMs: 60000}
	assert.Equal(t, 60*time.Second, cp1.ResolveHeaderTimeout())

	// 1b. 连 TTFT 都没配：回落到内置默认 TTFT，而不是另有一个更小的默认值。
	assert.Equal(t, time.Duration(DefaultTTFTTimeoutMs)*time.Millisecond,
		ChannelPolicy{}.ResolveHeaderTimeout())

	// 2. TTFT 较小 (8s)，HeaderTimeout 未配置：跟随 8s
	cp2 := ChannelPolicy{TTFTTimeoutMs: 8000}
	assert.Equal(t, 8*time.Second, cp2.ResolveHeaderTimeout())

	// 3. 显式配置 HeaderTimeoutMs (3s) 优先
	cp3 := ChannelPolicy{TTFTTimeoutMs: 60000, HeaderTimeoutMs: 3000}
	assert.Equal(t, 3*time.Second, cp3.ResolveHeaderTimeout())

	// 3b. 显式 HeaderTimeoutMs 大于 TTFT 时仍以 TTFT 为上限
	cp4 := ChannelPolicy{TTFTTimeoutMs: 20000, HeaderTimeoutMs: 90000}
	assert.Equal(t, 20*time.Second, cp4.ResolveHeaderTimeout())

	// 4. Policy Resolve 合并覆盖
	policy := Policy{
		Enabled: true,
		Default: ChannelPolicy{
			TTFTTimeoutMs:   30000,
			HeaderTimeoutMs: 10000,
		},
		Channels: map[int]ChannelPolicy{
			101: {HeaderTimeoutMs: 5000},
			102: {TTFTTimeoutMs: 4000},
		},
	}

	resolved101 := policy.Resolve(101)
	assert.Equal(t, 5*time.Second, resolved101.ResolveHeaderTimeout())

	resolved102 := policy.Resolve(102)
	assert.Equal(t, 4*time.Second, resolved102.ResolveHeaderTimeout())

	resolvedDefault := policy.Resolve(999)
	assert.Equal(t, 10*time.Second, resolvedDefault.ResolveHeaderTimeout())
}

func TestPolicyAttemptFirstTokenDeadline(t *testing.T) {
	// 线上配置：ttft 100s、客户端安全死线 105s，重试预留走默认 50s。
	safety := 105000
	policy := Policy{
		Enabled:                true,
		Default:                ChannelPolicy{TTFTTimeoutMs: 100000},
		Channels:               map[int]ChannelPolicy{7: {TTFTTimeoutMs: 30000}},
		ClientSafetyDeadlineMs: &safety,
	}
	reqStart := time.Unix(1_000_000, 0)
	at := func(d time.Duration) time.Time { return reqStart.Add(d) }

	// 还能重试时，本次截止在安全死线前 RetryReserve：首轮卡死的渠道 55s 就让位，
	// 换上的渠道还有 ~50s 出首字（生产首字 94% 在 60s 内、72% 在 20s 内）。
	assert.Equal(t, at(55*time.Second), policy.AttemptFirstTokenDeadline(1, reqStart, reqStart, true))
	// 首轮在预留点超时后，controller 的预算闸门必须放行换渠道。
	timedOut := policy.AttemptFirstTokenDeadline(1, reqStart, reqStart, true)
	assert.True(t, timedOut.Add(MinRetryWindow).Before(at(105*time.Second)))
	// 预留后本次恰好还剩 MinRetryWindow：仍按预留截止。
	assert.Equal(t, at(55*time.Second), policy.AttemptFirstTokenDeadline(1, reqStart, at(45*time.Second), true))
	// 预留后本次不足 MinRetryWindow（首轮超时后换上的第二个渠道）：用满到死线。
	assert.Equal(t, at(105*time.Second), policy.AttemptFirstTokenDeadline(1, reqStart, at(55*time.Second), true))
	// 不能再重试：首字窗口只受渠道 TTFT 与安全死线约束。
	assert.Equal(t, at(100*time.Second), policy.AttemptFirstTokenDeadline(1, reqStart, reqStart, false))
	assert.Equal(t, at(105*time.Second), policy.AttemptFirstTokenDeadline(1, reqStart, at(40*time.Second), false))
	// TTFT 从本次尝试开始计时：前面的失败尝试不占用本次的首字预算。
	assert.Equal(t, at(30*time.Second), policy.AttemptFirstTokenDeadline(7, reqStart, reqStart, true))
	assert.Equal(t, at(90*time.Second), policy.AttemptFirstTokenDeadline(7, reqStart, at(60*time.Second), false))

	// 显式配置预留 20s：首轮可等 85s（旧行为，留给偏好等慢渠道的部署）。
	reserve := 20000
	policy.RetryReserveMs = &reserve
	assert.Equal(t, at(85*time.Second), policy.AttemptFirstTokenDeadline(1, reqStart, reqStart, true))
	// 预留不大于 MinRetryWindow 时按 2×MinRetryWindow，保证闸门仍能放行重试。
	tooSmall := 5000
	policy.RetryReserveMs = &tooSmall
	assert.Equal(t, at(85*time.Second), policy.AttemptFirstTokenDeadline(1, reqStart, reqStart, true))
	policy.RetryReserveMs = nil

	// 关闭安全死线：只剩渠道 TTFT。
	disabled := 0
	policy.ClientSafetyDeadlineMs = &disabled
	assert.Equal(t, at(140*time.Second), policy.AttemptFirstTokenDeadline(1, reqStart, at(40*time.Second), true))

	// 未启用智能负载（无 TTFT）：不设首字截止。
	assert.True(t, (&Policy{}).AttemptFirstTokenDeadline(1, reqStart, reqStart, true).IsZero())
}
