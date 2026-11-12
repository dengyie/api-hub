package loadbalancer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// 2026-11-12 生产证据：breaker_exempt 渠道累计几十次硬失败却始终排在最前
// （#152 59×500、#278 55×502、#154 51 次失败），因为豁免让 TripBreaker*、
// IsAvailable 熔断判断与 End 的阈值熔断全部变成空操作——「这条渠道已经确定
// 坏了」对选路完全不可见。这里钉住软降级这条唯一代价通道的三条不变式。
func exemptDegradeTestPolicy(channelID int) *Policy {
	p := DefaultPolicy()
	p.Enabled = true
	perModel := true
	p.Default.Breaker.PerModel = &perModel
	p.Default.Breaker.FailureThreshold = 3
	p.Default.Breaker.CooldownSeconds = 300
	p.Default.Breaker.RateLimitCooldownSeconds = 30
	p.Channels = map[int]ChannelPolicy{
		channelID: {BreakerExempt: true},
	}
	return p
}

// TestExemptChannelHardFailureDegradesButStaysUsable 纯硬失败（无慢信号）
// 也必须让条目软降级；软降级不是熔断，渠道仍在可用集里。
func TestExemptChannelHardFailureDegradesButStaysUsable(t *testing.T) {
	const channelID = 7811
	SetPolicy(exemptDegradeTestPolicy(channelID))
	t.Cleanup(func() {
		GlobalTracker().ResetChannelBreakers(channelID)
		SetPolicy(&Policy{Enabled: false})
	})

	tracker := GlobalTracker()
	for i := 0; i < 5; i++ {
		tracker.Begin(channelID, "exempt-model").End(false, true)
	}

	assert.True(t, tracker.IsDegraded(channelID, "exempt-model"),
		"豁免渠道没有阈值熔断，硬失败若不记软降级就完全没有选路代价")
	ok, reason := tracker.IsAvailable(channelID, "exempt-model")
	assert.True(t, ok, "软降级不得把豁免渠道摘出可用集: %s", reason)
}

// TestNonExemptChannelHardFailureDoesNotSoftDegrade 非豁免渠道的硬失败交给
// 熔断计数器，不得顺带软降级：把两条路径混在一起会让熔断的递增退避与软降级
// 窗口互相掩盖，排障也无法判断渠道是「熔了」还是「仅仅排最后」。
func TestNonExemptChannelHardFailureDoesNotSoftDegrade(t *testing.T) {
	const channelID = 7812
	p := exemptDegradeTestPolicy(channelID)
	p.Channels = nil
	SetPolicy(p)
	t.Cleanup(func() {
		GlobalTracker().ResetChannelBreakers(channelID)
		SetPolicy(&Policy{Enabled: false})
	})

	tracker := GlobalTracker()
	tracker.Begin(channelID, "normal-model").End(false, true)

	assert.False(t, tracker.IsDegraded(channelID, "normal-model"))
}

// TestDegradeChannelForFaultUsesCooldownWindow 确定性故障窗口按常规冷却，
// 限流按短避让；重复记录只延长不缩短。
func TestDegradeChannelForFaultUsesCooldownWindow(t *testing.T) {
	const channelID = 7813
	SetPolicy(exemptDegradeTestPolicy(channelID))
	t.Cleanup(func() {
		GlobalTracker().ResetChannelBreakers(channelID)
		SetPolicy(&Policy{Enabled: false})
	})

	tracker := GlobalTracker()
	assert.Equal(t, 300*time.Second, tracker.DegradeChannelForFault(channelID, "m"),
		"确定性故障按 breaker.cooldown_seconds 降级")
	assert.True(t, tracker.IsDegraded(channelID, "m"))
	assert.Equal(t, time.Duration(0), tracker.DegradeChannelForFault(channelID, "m"),
		"同一时刻重复记录不得缩短/重写已有窗口")

	assert.Equal(t, time.Duration(0), tracker.DegradeChannelForRateLimit(channelID, "m"),
		"限流按 30s 短避让，但只延长不缩短：已生效的 300s 窗口不得被重写成更短的")
	assert.True(t, tracker.IsDegraded(channelID, "m"))
}
