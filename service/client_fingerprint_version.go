package service

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dengyie/apihub/logger"
	"github.com/dengyie/apihub/setting/operation_setting"

	"github.com/bytedance/gopkg/util/gopool"
)

// 用户定制：Codex CLI / Claude Code 伪装指纹版本自动对齐线上（借鉴 zcode-hub
// version_tracker 机制）。指纹 UA 里的版本号此前是 upstream_fingerprint.go 里
// 的写死常量，落后官方发版即向上游暴露代理身份（部分上游按客户端版本做门禁，
// 过低直接拒绝）。本模块周期拉取官方发布源，把最新版本号写入 atomic，请求路径
// 只读 atomic、零网络调用；同步失败沿用旧值，从未同步成功则用默认 UA（永远不
// 发空 UA）。X-Stainless-Package-Version 是 @anthropic-ai/sdk 的独立版本列车，
// 无法从 claude-code npm 版本推导，保持静态值（见知识库伪装规范）。
const (
	codexFingerprintDefaultVersion  = "0.77.0"
	claudeFingerprintDefaultVersion = "2.1.284"

	codexFingerprintUATemplate  = "codex_cli_rs/%s (Mac OS 26.1.0; arm64) iTerm.app/3.6.5"
	claudeFingerprintUATemplate = "claude-code/%s (Darwin 24.6.0; arm64)"

	clientFingerprintSyncInterval = time.Hour
	// 每个 fetch 源独立超时；两个源串行执行，互不挤占时间预算
	clientFingerprintFetchTimeout = 15 * time.Second
)

// 官方发版号只可能是数字分量；脏数据（README 名、错误页等）不得污染 UA。
var fingerprintVersionRegex = regexp.MustCompile(`^\d+(\.\d+)*$`)

type fingerprintUA struct {
	version string
	ua      string
}

var (
	activeCodexFingerprintUA  atomic.Pointer[fingerprintUA]
	activeClaudeFingerprintUA atomic.Pointer[fingerprintUA]
)

// ActiveCodexFingerprintVersion returns the synced official Codex release
// version, or "" before the first successful sync. Consumers must fall back to
// their own default (e.g. codexFingerprintDefaultVersion) on "".
func ActiveCodexFingerprintVersion() string {
	if fp := activeCodexFingerprintUA.Load(); fp != nil {
		return fp.version
	}
	return ""
}

// ActiveClaudeFingerprintVersion is the Claude Code counterpart of
// ActiveCodexFingerprintVersion.
func ActiveClaudeFingerprintVersion() string {
	if fp := activeClaudeFingerprintUA.Load(); fp != nil {
		return fp.version
	}
	return ""
}

// ActiveCodexFingerprintUA returns the UA the upstream must see for Codex
// fingerprint requests: the synced official release version when available,
// the compiled-in default otherwise.
func ActiveCodexFingerprintUA() string {
	if fp := activeCodexFingerprintUA.Load(); fp != nil {
		return fp.ua
	}
	return fmt.Sprintf(codexFingerprintUATemplate, codexFingerprintDefaultVersion)
}

// ActiveClaudeFingerprintUA is the Claude Code counterpart of
// ActiveCodexFingerprintUA.
func ActiveClaudeFingerprintUA() string {
	if fp := activeClaudeFingerprintUA.Load(); fp != nil {
		return fp.ua
	}
	return fmt.Sprintf(claudeFingerprintUATemplate, claudeFingerprintDefaultVersion)
}

func setActiveCodexFingerprintVersion(version string) {
	activeCodexFingerprintUA.Store(&fingerprintUA{
		version: version,
		ua:      fmt.Sprintf(codexFingerprintUATemplate, version),
	})
}

func setActiveClaudeFingerprintVersion(version string) {
	activeClaudeFingerprintUA.Store(&fingerprintUA{
		version: version,
		ua:      fmt.Sprintf(claudeFingerprintUATemplate, version),
	})
}

func isValidFingerprintVersion(version string) bool {
	return fingerprintVersionRegex.MatchString(version)
}

func validateFingerprintVersion(version string) string {
	version = strings.TrimSpace(version)
	if !isValidFingerprintVersion(version) {
		return ""
	}
	return version
}

// StartClientFingerprintSyncTask periodically syncs the Codex / Claude Code
// disguise UA versions against the official release feeds. Runs on every node:
// the task only performs read-only HTTP GETs and writes process-local atomic
// state (no DB writes, no cross-node coordination), so there is no
// master-exclusivity requirement — running it on slaves too keeps every
// node's UA at the synced version.
func StartClientFingerprintSyncTask() {
	clientFingerprintSyncOnce.Do(func() {
		gopool.Go(func() {
			logger.LogInfo(context.Background(), fmt.Sprintf("client fingerprint version sync task started: tick=%s", clientFingerprintSyncInterval))

			runClientFingerprintSyncOnce()
			ticker := time.NewTicker(clientFingerprintSyncInterval)
			defer ticker.Stop()
			for range ticker.C {
				runClientFingerprintSyncOnce()
			}
		})
	})
}

var (
	clientFingerprintSyncOnce    sync.Once
	clientFingerprintSyncRunning atomic.Bool
)

func runClientFingerprintSyncOnce() {
	if !operation_setting.GetChannelAffinitySetting().ClientVersionAutoSync {
		return
	}
	if !clientFingerprintSyncRunning.CompareAndSwap(false, true) {
		return
	}
	defer clientFingerprintSyncRunning.Store(false)

	client := GetHttpClient()
	codexCtx, codexCancel := context.WithTimeout(context.Background(), clientFingerprintFetchTimeout)
	defer codexCancel()
	syncCodexFingerprintVersion(codexCtx, client)

	claudeCtx, claudeCancel := context.WithTimeout(context.Background(), clientFingerprintFetchTimeout)
	defer claudeCancel()
	syncClaudeFingerprintVersion(claudeCtx, client)
}

func syncCodexFingerprintVersion(ctx context.Context, client *http.Client) {
	// 缓存层（codexClientVersionCache.get）返回的值在 fetch 时已经过了
	// validateFingerprintVersion 校验，这里直接采纳。
	version, err := GetLatestCodexClientVersion(ctx, client)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("codex fingerprint version sync failed: %v", err))
		return
	}
	if fp := activeCodexFingerprintUA.Load(); fp != nil && fp.version == version {
		return
	}
	setActiveCodexFingerprintVersion(version)
	logger.LogInfo(ctx, fmt.Sprintf("codex fingerprint UA synced to official release %s", version))
}

func syncClaudeFingerprintVersion(ctx context.Context, client *http.Client) {
	// claude 侧校验在 fetchLatestClaudeCodeVersion，语义与 codex 侧一致。
	version, err := latestClaudeCodeVersion(ctx, client)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("claude-code fingerprint version sync failed: %v", err))
		return
	}
	if fp := activeClaudeFingerprintUA.Load(); fp != nil && fp.version == version {
		return
	}
	setActiveClaudeFingerprintVersion(version)
	logger.LogInfo(ctx, fmt.Sprintf("claude-code fingerprint UA synced to official release %s", version))
}
