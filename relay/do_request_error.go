package relay

import (
	"errors"
	"net/http"

	"github.com/dengyie/apihub/loadbalancer"
	"github.com/dengyie/apihub/relaykit/types"
)

// doRequestError 统一 adaptor.DoRequest 传输层失败的归因。
//
// 响应头等待超时（`relay/channel/api_request.go` 里的 header timeout 定时器）
// 产生的是 *loadbalancer.TTFTTimeoutError，它与流式路径的首字超时是同一件事：
// 「这条渠道在等待窗口内没有吐出首个字节」。两者必须终结成同一个客户端错误
// ——504 / channel:response_time_exceeded——这样才会走 ttft_timeout 重试分支、
// 计入渠道健康度，并且客户端看到的是「网关等待上游超时」而不是 500。
//
// 2026-11-12 生产实测：/v1/responses 的三个 500 全部来自这条路径。日志里网关
// 自己的上游侧明明是「渠道 #355 响应头等待超时（18.48s），触发换渠道重试」，
// 客户端拿到的却是 500 + "loadbalancer: time to first token timeout"；同一批
// 请求里走流式 TTFT 路径的失败则是正确的 504。原因只是这个 handler 少了
// compatible_handler 里那段映射——同一语义在不同 handler 上口径相反。
//
// 其余传输层失败（连接失败、请求体构造失败等）仍是 ErrorCodeDoRequestFailed
// + 500：它们不代表渠道慢，只是本次请求没能发出去。
//
// 用 errors.As 而非裸类型断言：适配器可能把错误包一层（fmt.Errorf %w），
// 裸断言会静默退化成 500 —— 这正是这条路径此前无法被发现的原因之一。
func doRequestError(err error) *types.NewAPIError {
	var ttftErr *loadbalancer.TTFTTimeoutError
	if errors.As(err, &ttftErr) {
		return types.NewErrorWithStatusCode(
			ttftErr,
			types.ErrorCodeChannelResponseTimeExceeded,
			http.StatusGatewayTimeout,
		)
	}
	return types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
}
