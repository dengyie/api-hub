package service

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/dengyie/apihub/common"
)

// claudeCodeLatestReleaseURL 保持 var 便于测试注入本地桩源。
var claudeCodeLatestReleaseURL = "https://registry.npmjs.org/@anthropic-ai/claude-code/latest"

var latestClaudeClientVersionCache = clientVersionCache{fetch: fetchLatestClaudeCodeVersion}

// latestClaudeCodeVersion mirrors GetLatestCodexClientVersion's cache
// semantics: 1h TTL, stale value on fetch failure, hard error only when there
// is nothing to fall back to. Unexported: the only consumer is the fingerprint
// sync task in client_fingerprint_version.go.
func latestClaudeCodeVersion(ctx context.Context, client *http.Client) (string, error) {
	return latestClaudeClientVersionCache.get(ctx, client, claudeCodeLatestReleaseURL, time.Now())
}

func fetchLatestClaudeCodeVersion(ctx context.Context, client *http.Client, releaseURL string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("nil http client")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "new-api")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("claude-code release lookup failed: status=%d", resp.StatusCode)
	}

	var manifest struct {
		Version string `json:"version"`
	}
	if err := common.DecodeJson(resp.Body, &manifest); err != nil {
		return "", err
	}
	// 与 codex 侧（fetchLatestCodexClientVersion）同约定：脏版本在此拒绝，
	// 不进缓存。
	version := validateFingerprintVersion(manifest.Version)
	if version == "" {
		return "", fmt.Errorf("claude-code release has no valid version: %q", manifest.Version)
	}
	return version, nil
}
