package relay

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/dengyie/apihub/loadbalancer"
	"github.com/dengyie/apihub/relaykit/types"
	"github.com/stretchr/testify/assert"
)

// TestDoRequestErrorMapsHeaderTimeoutToGatewayTimeout 钉住 2026-11-12 生产缺陷：
// 响应头等待超时（由 relay/channel/api_request.go 的定时器返回
// *loadbalancer.TTFTTimeoutError）在 /v1/responses 等 handler 上被归成 500 +
// ErrorCodeDoRequestFailed，同一请求里走流式 TTFT 路径的失败却是 504。两者是
// 同一件事，客户端必须看到同样的结论：网关等上游超时，可重试、可换渠道。
func TestDoRequestErrorMapsHeaderTimeoutToGatewayTimeout(t *testing.T) {
	newAPIError := doRequestError(&loadbalancer.TTFTTimeoutError{ChannelID: 355, TimeoutMs: 18482})

	assert.Equal(t, http.StatusGatewayTimeout, newAPIError.StatusCode)
	assert.Equal(t, types.ErrorCodeChannelResponseTimeExceeded, newAPIError.GetErrorCode())
	assert.True(t, loadbalancer.IsTTFTTimeout(newAPIError), "504 仍要能被 ttft_timeout 重试分支识别")
	assert.True(t, types.IsChannelError(newAPIError))
}

// TestDoRequestErrorMapsWrappedHeaderTimeout：适配器可能把超时错误包一层，
// 裸类型断言（旧实现）会静默退化成 500。映射必须走解包。
func TestDoRequestErrorMapsWrappedHeaderTimeout(t *testing.T) {
	wrapped := fmt.Errorf("do request: %w", &loadbalancer.TTFTTimeoutError{ChannelID: 355, TimeoutMs: 20000})

	newAPIError := doRequestError(wrapped)

	assert.Equal(t, http.StatusGatewayTimeout, newAPIError.StatusCode)
	assert.Equal(t, types.ErrorCodeChannelResponseTimeExceeded, newAPIError.GetErrorCode())
}

// TestDoRequestErrorKeepsOtherTransportFailures 连接失败等非延迟类传输错误
// 不代表渠道慢，仍是 500 + do_request_failed。
func TestDoRequestErrorKeepsOtherTransportFailures(t *testing.T) {
	newAPIError := doRequestError(errors.New("dial tcp 10.0.0.1:443: connect: connection refused"))

	assert.Equal(t, http.StatusInternalServerError, newAPIError.StatusCode)
	assert.Equal(t, types.ErrorCodeDoRequestFailed, newAPIError.GetErrorCode())
	assert.False(t, loadbalancer.IsTTFTTimeout(newAPIError))
}
