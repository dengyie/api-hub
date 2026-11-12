package loadbalancer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// 2026-11-12 生产证据：豁免池 #278/#238 在同一个请求里先 502、再 85s 无首字，
// 客户端重试 11 次全部落回同一池子。熔断对 breaker_exempt 渠道是空操作（刻意
// 配置），若软降级也不记，「渠道慢」对选路完全不可见。
//
// 这里钉住两条：
//  1. 慢 + 失败（首字超时、上游死挂后客户端断开）必须让条目进入软降级；
//  2. 软降级不是熔断——渠道仍然可用（IsAvailable 通过），只是排序排最后。
func TestTTFTTimeoutDegradesChannelEvenWhenBreakerExempt(t *testing.T) {
	SetPolicy(&Policy{
		Enabled: true,
		Default: ChannelPolicy{
			MaxInflight: 3,
			Breaker: BreakerPolicy{
				FailureThreshold:         3,
				CooldownSeconds:          300,
				RateLimitCooldownSeconds: 30,
				HalfOpenProbes:           1,
			},
		},
		Channels: map[int]ChannelPolicy{
			7801: {BreakerExempt: true},
		},
	})
	t.Cleanup(func() {
		GlobalTracker().ResetChannelBreakers(7801)
		SetPolicy(&Policy{Enabled: false})
	})

	tracker := GlobalTracker()

	t.Run("exempt channel with TTFT timeout is downgraded but usable", func(t *testing.T) {
		h := tracker.Begin(7801, "gemini-3.8-flash")
		h.End(true, true)

		assert.True(t, tracker.IsDegraded(7801, "gemini-3.8-flash"),
			"首字超时必须让选路看见「这条渠道慢」，即使它豁免熔断")
		ok, reason := tracker.IsAvailable(7801, "gemini-3.8-flash")
		assert.True(t, ok, "软降级不得把渠道摘出可用集（breaker_exempt 的不可失去语义）: %s", reason)
	})

	t.Run("failure without slow signal does not degrade non-exempt channel", func(t *testing.T) {
		// 非豁免渠道（7803 不在 exempt 表里）：纯失败交给熔断计数器，
		// 不得顺带软降级。豁免渠道的纯失败会在 End 里记短窗口降级，
		// 见 exempt_degrade_test.go。
		GlobalTracker().ResetChannelBreakers(7803)
		h := tracker.Begin(7803, "gemini-3.8-flash")
		h.End(false, true)

		assert.False(t, tracker.IsDegraded(7803, "gemini-3.8-flash"),
			"纯失败应由熔断计数器处理，不应顺带软降级")
	})
}

// 慢降级只延长已有窗口：3 次连续慢触发的 10 分钟降级不能被一次 30s 的
// 首字超时记录缩短，否则慢渠道的隔离窗口会在下一次首字超时时被意外撤销。
func TestSlowDegradeNeverShortensExistingWindow(t *testing.T) {
	SetPolicy(&Policy{
		Enabled: true,
		Default: ChannelPolicy{
			MaxInflight: 3,
			Breaker: BreakerPolicy{
				FailureThreshold:         3,
				CooldownSeconds:          300,
				RateLimitCooldownSeconds: 30,
				HalfOpenProbes:           1,
			},
		},
	})
	t.Cleanup(func() {
		GlobalTracker().ResetChannelBreakers(7802)
		SetPolicy(&Policy{Enabled: false})
	})

	tracker := GlobalTracker()
	// 三次慢（非失败）触发 10 分钟降级
	for i := 0; i < 3; i++ {
		h := tracker.Begin(7802, "m")
		h.End(true, false)
	}
	assert.True(t, tracker.IsDegraded(7802, "m"))

	// 随后一次「慢 + 失败」不应把窗口缩短到 30s
	h := tracker.Begin(7802, "m")
	h.End(true, true)
	assert.True(t, tracker.IsDegraded(7802, "m"), "短窗口不得覆盖更长的既有降级窗口")
}
