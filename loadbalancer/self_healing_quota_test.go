package loadbalancer

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/dengyie/apihub/relaykit/types"
)

// 生产实锤文案（2026-11-20 排查取证）：
// #394 joystar 429 / #374 1tim 403 / #121 zcode-hub 503（必须不命中）。
func TestParseSelfHealingQuotaReset(t *testing.T) {
	now := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC) // 上海 02:00 次日凌晨已过

	t.Run("joystar Moscow midnight", func(t *testing.T) {
		err := types.NewErrorWithStatusCode(errors.New("The shared daily limit for the free models is exhausted (10,000,000 tokens across all free models combined). The limit resets at 00:00 Moscow time."), types.ErrorCodeBadResponseBody, http.StatusTooManyRequests)
		got, ok := ParseSelfHealingQuotaReset(err, now)
		if !ok {
			t.Fatal("expected parse ok")
		}
		moscow, _ := time.LoadLocation("Europe/Moscow")
		want := time.Date(2026, 11, 21, 0, 0, 0, 0, moscow)
		if !got.Equal(want) {
			t.Fatalf("got %v want %v", got, want)
		}
		// 上海视角 = 05:00 次日：不能解析成上海 0 点（已过期→顺延一天会错 19h）
		if got.Sub(now) > 24*time.Hour || got.Sub(now) < time.Hour {
			t.Fatalf("unreasonable distance from now: %v", got.Sub(now))
		}
	})

	t.Run("1tim Chinese midnight reset", func(t *testing.T) {
		err := types.NewErrorWithStatusCode(errors.New("DeepSeek共享池额度没啦，去用奇怪版分组，当前额度剩余0，可以去网站查看共享池详细，明天凌晨0点自动重置，佬们蹬太狠了，去 QQ 群求求站长加一点吧"), types.ErrorCodeBadResponseBody, http.StatusForbidden)
		got, ok := ParseSelfHealingQuotaReset(err, now)
		if !ok {
			t.Fatal("expected parse ok")
		}
		sh := shanghaiLoc()
		want := time.Date(2026, 11, 22, 0, 0, 0, 0, sh) // now=上海 11-21 02:00（已过当天 0 点），文案「明天凌晨0点」= 11-22 00:00
		if !got.Equal(want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("121 transient mixed wording must NOT parse", func(t *testing.T) {
		err := types.NewErrorWithStatusCode(errors.New("所有账号均不可用、额度已用完或并发已满，请在后台检查账号状态"), types.ErrorCodeBadResponseBody, http.StatusServiceUnavailable)
		if _, ok := ParseSelfHealingQuotaReset(err, now); ok {
			t.Fatal("transient mixed wording must not be treated as self-healing daily reset")
		}
		if IsSelfHealingQuotaError(err) {
			t.Fatal("IsSelfHealingQuotaError must not match transient mixed wording (#121)")
		}
	})

	t.Run("unknown city falls back", func(t *testing.T) {
		err := types.NewErrorWithStatusCode(errors.New("The limit resets at 00:00 Atlantis time."), types.ErrorCodeBadResponseBody, http.StatusTooManyRequests)
		if _, ok := ParseSelfHealingQuotaReset(err, now); ok {
			t.Fatal("unknown city must return false so caller uses fixed fallback")
		}
	})
}

