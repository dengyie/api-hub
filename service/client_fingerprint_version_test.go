package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dengyie/apihub/setting/operation_setting"
)

func resetFingerprintUAState(t *testing.T) {
	t.Helper()
	activeCodexFingerprintUA.Store(nil)
	activeClaudeFingerprintUA.Store(nil)
	latestClaudeClientVersionCache.Lock()
	latestClaudeClientVersionCache.version = ""
	latestClaudeClientVersionCache.expiresAt = time.Time{}
	latestClaudeClientVersionCache.Unlock()
}

func TestActiveFingerprintUAFallsBackToDefault(t *testing.T) {
	resetFingerprintUAState(t)

	if got := ActiveCodexFingerprintUA(); got != "codex_cli_rs/0.77.0 (Mac OS 26.1.0; arm64) iTerm.app/3.6.5" {
		t.Fatalf("default codex UA mismatch: %q", got)
	}
	if got := ActiveClaudeFingerprintUA(); got != "claude-code/2.1.284 (Darwin 24.6.0; arm64)" {
		t.Fatalf("default claude UA mismatch: %q", got)
	}
	if got := ActiveCodexFingerprintVersion(); got != "" {
		t.Fatalf("expected empty active codex version before sync, got %q", got)
	}
}

func TestSetActiveFingerprintVersionBuildsUA(t *testing.T) {
	resetFingerprintUAState(t)

	setActiveCodexFingerprintVersion("0.167.2")
	setActiveClaudeFingerprintVersion("2.1.322")

	if got := ActiveCodexFingerprintUA(); got != "codex_cli_rs/0.167.2 (Mac OS 26.1.0; arm64) iTerm.app/3.6.5" {
		t.Fatalf("synced codex UA mismatch: %q", got)
	}
	if got := ActiveClaudeFingerprintUA(); got != "claude-code/2.1.322 (Darwin 24.6.0; arm64)" {
		t.Fatalf("synced claude UA mismatch: %q", got)
	}
	if got := ActiveCodexFingerprintVersion(); got != "0.167.2" {
		t.Fatalf("active codex version mismatch: %q", got)
	}
}

func TestValidateFingerprintVersionRejectsDirtyData(t *testing.T) {
	for _, bad := range []string{"", "  ", "v0.167.2", "0.167.2-beta", "latest", "Release 0.1", "0x1F"} {
		if got := validateFingerprintVersion(bad); got != "" {
			t.Fatalf("expected %q to be rejected, got %q", bad, got)
		}
	}
	for _, good := range []string{"0.167.2", "2.1.322", "3", "1.2.3.4"} {
		if got := validateFingerprintVersion(good); got != good {
			t.Fatalf("expected %q to be accepted, got %q", good, got)
		}
	}
}

// stubClaudeReleaseURL 把全局 claude 发布源指向本地桩，测试结束恢复并清空缓存，
// 防止用例间污染（缓存与 URL 都是包级全局）。
func stubClaudeReleaseURL(t *testing.T, url string) {
	t.Helper()
	orig := claudeCodeLatestReleaseURL
	claudeCodeLatestReleaseURL = url
	t.Cleanup(func() {
		claudeCodeLatestReleaseURL = orig
		latestClaudeClientVersionCache.Lock()
		latestClaudeClientVersionCache.version = ""
		latestClaudeClientVersionCache.expiresAt = time.Time{}
		latestClaudeClientVersionCache.Unlock()
	})
}

func TestLatestClaudeClientVersionFromNpm(t *testing.T) {
	resetFingerprintUAState(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/@anthropic-ai/claude-code/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"@anthropic-ai/claude-code","version":"2.1.322"}`)
	}))
	defer server.Close()
	stubClaudeReleaseURL(t, server.URL+"/@anthropic-ai/claude-code/latest")

	got, err := latestClaudeCodeVersion(context.Background(), server.Client())
	if err != nil {
		t.Fatalf("fetch failed: %v", err)
	}
	if got != "2.1.322" {
		t.Fatalf("expected 2.1.322, got %q", got)
	}
}

func TestLatestClaudeClientVersionStaleFallback(t *testing.T) {
	resetFingerprintUAState(t)

	now := time.Now()
	latestClaudeClientVersionCache.Lock()
	latestClaudeClientVersionCache.version = "2.1.300"
	latestClaudeClientVersionCache.expiresAt = now.Add(time.Hour)
	latestClaudeClientVersionCache.Unlock()

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	stubClaudeReleaseURL(t, broken.URL)

	got, err := latestClaudeCodeVersion(context.Background(), broken.Client())
	if err != nil {
		t.Fatalf("stale fallback should not error: %v", err)
	}
	if got != "2.1.300" {
		t.Fatalf("expected stale version 2.1.300, got %q", got)
	}

	// 过期后源仍然挂：只要缓存里有旧值就继续回退旧值（与 codex 缓存语义一致，
	// 不报错）。真正报错只发生在"从未成功同步过"时。
	latestClaudeClientVersionCache.Lock()
	latestClaudeClientVersionCache.version = "2.1.300"
	latestClaudeClientVersionCache.expiresAt = now
	latestClaudeClientVersionCache.Unlock()

	got, err = latestClaudeCodeVersion(context.Background(), broken.Client())
	if err != nil {
		t.Fatalf("stale value must still be returned after expiry: %v", err)
	}
	if got != "2.1.300" {
		t.Fatalf("expected stale version 2.1.300 after expiry, got %q", got)
	}

	// 从未成功同步过 + 源挂 → 报错
	latestClaudeClientVersionCache.Lock()
	latestClaudeClientVersionCache.version = ""
	latestClaudeClientVersionCache.expiresAt = time.Time{}
	latestClaudeClientVersionCache.Unlock()
	got, err = latestClaudeCodeVersion(context.Background(), broken.Client())
	if err == nil {
		t.Fatalf("expected error when no cached value and source broken")
	}
	if got != "" {
		t.Fatalf("expected empty version, got %q", got)
	}
}

func TestLatestClaudeClientVersionRejectsDirtyVersion(t *testing.T) {
	resetFingerprintUAState(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"not-a-version"}`)
	}))
	defer server.Close()
	stubClaudeReleaseURL(t, server.URL)

	if _, err := latestClaudeCodeVersion(context.Background(), server.Client()); err == nil {
		t.Fatalf("expected error for dirty version payload")
	}
}

func TestSyncFingerprintVersionUpdatesAtomic(t *testing.T) {
	resetFingerprintUAState(t)

	codexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"0.167.2","draft":false,"prerelease":false}`)
	}))
	defer codexServer.Close()

	claudeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"2.1.322"}`)
	}))
	defer claudeServer.Close()

	// 用真实 fetch 链路：把全局缓存指向本地桩
	origCodexURL := codexLatestReleaseURL
	codexLatestReleaseURL = codexServer.URL
	origClaudeURL := claudeCodeLatestReleaseURL
	claudeCodeLatestReleaseURL = claudeServer.URL
	t.Cleanup(func() {
		codexLatestReleaseURL = origCodexURL
		claudeCodeLatestReleaseURL = origClaudeURL
	})

	ctx := context.Background()
	syncCodexFingerprintVersion(ctx, codexServer.Client())
	syncClaudeFingerprintVersion(ctx, claudeServer.Client())

	if got := ActiveCodexFingerprintVersion(); got != "0.167.2" {
		t.Fatalf("codex sync failed, active version %q", got)
	}
	if got := ActiveClaudeFingerprintVersion(); got != "2.1.322" {
		t.Fatalf("claude sync failed, active version %q", got)
	}
	if !strings.Contains(ActiveCodexFingerprintUA(), "codex_cli_rs/0.167.2") {
		t.Fatalf("codex UA not rebuilt from synced version: %q", ActiveCodexFingerprintUA())
	}
	if !strings.Contains(ActiveClaudeFingerprintUA(), "claude-code/2.1.322") {
		t.Fatalf("claude UA not rebuilt from synced version: %q", ActiveClaudeFingerprintUA())
	}

	// 发布源挂掉：已有同步值时静默沿用，不清空
	syncCodexFingerprintVersion(ctx, brokenClient())
	if got := ActiveCodexFingerprintVersion(); got != "0.167.2" {
		t.Fatalf("sync failure must keep previous version, got %q", got)
	}
}

func brokenClient() *http.Client {
	return &http.Client{Transport: failingRoundTripper{}}
}

type failingRoundTripper struct{}

func (failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("network down")
}

func stubCodexReleaseURL(t *testing.T, url string) {
	t.Helper()
	orig := codexLatestReleaseURL
	codexLatestReleaseURL = url
	t.Cleanup(func() {
		codexLatestReleaseURL = orig
		latestCodexClientVersion.Lock()
		latestCodexClientVersion.version = ""
		latestCodexClientVersion.expiresAt = time.Time{}
		latestCodexClientVersion.Unlock()
	})
}

// 开关关闭：runClientFingerprintSyncOnce 直接返回，atomic 状态不变。
func TestClientFingerprintSyncRespectsOffSwitch(t *testing.T) {
	resetFingerprintUAState(t)

	operation_setting.GetChannelAffinitySetting().ClientVersionAutoSync = false
	t.Cleanup(func() {
		operation_setting.GetChannelAffinitySetting().ClientVersionAutoSync = true
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"9.9.9","draft":false,"prerelease":false}`)
	}))
	defer server.Close()
	stubCodexReleaseURL(t, server.URL)

	runClientFingerprintSyncOnce()

	if got := ActiveCodexFingerprintVersion(); got != "" {
		t.Fatalf("switch off must not sync, got active version %q", got)
	}
	if got := ActiveCodexFingerprintUA(); got != "codex_cli_rs/0.77.0 (Mac OS 26.1.0; arm64) iTerm.app/3.6.5" {
		t.Fatalf("switch off must keep default UA, got %q", got)
	}
}

// 已同步到同版本时幂等：不重复 Store、不发同步日志。
func TestSyncFingerprintVersionIsIdempotent(t *testing.T) {
	resetFingerprintUAState(t)

	setActiveCodexFingerprintVersion("0.167.2")
	setActiveClaudeFingerprintVersion("2.1.322")

	// 缓存里也放上同版本，fetch 命中缓存后走幂等短路分支
	latestCodexClientVersion.Lock()
	latestCodexClientVersion.version = "0.167.2"
	latestCodexClientVersion.expiresAt = time.Now().Add(time.Hour)
	latestCodexClientVersion.Unlock()
	latestClaudeClientVersionCache.Lock()
	latestClaudeClientVersionCache.version = "2.1.322"
	latestClaudeClientVersionCache.expiresAt = time.Now().Add(time.Hour)
	latestClaudeClientVersionCache.Unlock()

	ctx := context.Background()
	syncCodexFingerprintVersion(ctx, http.DefaultClient)
	syncClaudeFingerprintVersion(ctx, http.DefaultClient)

	if got := ActiveCodexFingerprintVersion(); got != "0.167.2" {
		t.Fatalf("codex version must stay %q, got %q", "0.167.2", got)
	}
	if got := ActiveClaudeFingerprintVersion(); got != "2.1.322" {
		t.Fatalf("claude version must stay %q, got %q", "2.1.322", got)
	}
}

// 并发 sync 不与缓存互斥死锁、不撕裂状态。
func TestSyncFingerprintVersionConcurrent(t *testing.T) {
	resetFingerprintUAState(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"0.167.2","draft":false,"prerelease":false}`)
	}))
	defer server.Close()
	stubCodexReleaseURL(t, server.URL)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			syncCodexFingerprintVersion(context.Background(), server.Client())
		}()
	}
	wg.Wait()

	if got := ActiveCodexFingerprintVersion(); got != "0.167.2" {
		t.Fatalf("concurrent sync failed, active version %q", got)
	}
}

// runClientFingerprintSyncOnce 防重入：第二次并发调用直接跳过。
func TestRunClientFingerprintSyncOnceReentrant(t *testing.T) {
	resetFingerprintUAState(t)
	if !clientFingerprintSyncRunning.CompareAndSwap(false, true) {
		t.Fatalf("failed to acquire reentrancy guard for test")
	}
	t.Cleanup(func() { clientFingerprintSyncRunning.Store(false) })

	runClientFingerprintSyncOnce()
	// 走不到 fetch：atomic 仍为空
	if got := ActiveCodexFingerprintVersion(); got != "" {
		t.Fatalf("reentrant call must be skipped, got %q", got)
	}
}
