package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRecorder(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return httptest.NewRecorder()
}

// 上游反滥用哨兵实测形态（2026-11-18 kunyou 免费池）：
// 非流式请求有时收到 SSE body（data: chunk，id=chatcmpl-freeipblk...），
// 有时收到 JSON body（id=resp_freeipblk...，completion_tokens=1），
// 旧逻辑前者报 invalid character 'd' (bad_response_body)、后者假绿。
func newNonStreamResponse(t *testing.T, body string) (*gin.Context, *http.Response, *relaycommon.RelayInfo) {
	t.Helper()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, IsStream: false}
	c, _ := gin.CreateTestContext(newRecorder(t))
	return c, resp, info
}

func TestOpenaiHandlerAggregatesSSESentinelBody(t *testing.T) {
	// 修复前：data: 开头 → invalid character 'd' looking for beginning of value
	sseBody := "data: {\"id\":\"chatcmpl-freeipblk000000000000000\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash-1017\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-freeipblk000000000000000\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash-1017\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"当前网络的请求存在异常，已被临时限制访问，请稍后再试。\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-freeipblk000000000000000\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash-1017\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	c, resp, info := newNonStreamResponse(t, sseBody)
	usage, apiErr := OpenaiHandler(c, info, resp)
	require.Nil(t, usage, "哨兵响应不应被当成功")
	require.NotNil(t, apiErr)
	assert.Contains(t, apiErr.Error(), "上游反滥用哨兵拦截")
	assert.Contains(t, apiErr.Error(), "已被临时限制访问")
}

func TestOpenaiHandlerJSONSentinelReturnsError(t *testing.T) {
	jsonBody := "{\"id\":\"resp_freeipblk00000000000000000\",\"object\":\"chat.completion\",\"model\":\"deepseek-v4.1-flash-1017\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"当前网络的请求存在异常，已被临时限制访问，请稍后再试。\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":13,\"completion_tokens\":1,\"total_tokens\":14}}"

	c, resp, info := newNonStreamResponse(t, jsonBody)
	usage, apiErr := OpenaiHandler(c, info, resp)
	require.Nil(t, usage, "哨兵响应不应被当成功")
	require.NotNil(t, apiErr)
	assert.Contains(t, apiErr.Error(), "上游反滥用哨兵拦截")
}

func TestOpenaiHandlerNormalSSEBodyAggregated(t *testing.T) {
	// 上游强转流式（非哨兵）：聚合后应还原为正常非流式响应并继续走成功路径
	sseBody := "data: {\"id\":\"resp_09301851\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-6-luna\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"pong\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"resp_09301851\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-6-luna\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n" +
		"data: [DONE]\n\n"

	c, resp, info := newNonStreamResponse(t, sseBody)
	usage, apiErr := OpenaiHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 6, usage.TotalTokens)
}

func TestOpenaiHandlerNormalResponseUnaffected(t *testing.T) {
	jsonBody := "{\"id\":\"resp_abc\",\"object\":\"chat.completion\",\"model\":\"gpt-6-luna\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"pong\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}"

	c, resp, info := newNonStreamResponse(t, jsonBody)
	usage, apiErr := OpenaiHandler(c, info, resp)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 6, usage.TotalTokens)
}

func TestUpstreamAntiProbeSentinelTextDiscriminators(t *testing.T) {
	// id 命中即拦截（无论 usage 大小）
	resp := &dto.OpenAITextResponse{Id: "resp_freeblock000", Choices: []dto.OpenAITextResponseChoice{{Message: dto.Message{Role: "assistant", Content: "任意内容"}}}}
	assert.Equal(t, "任意内容", upstreamAntiProbeSentinelText(resp))
	// 非 id 命中：正文含标记但 usage 大 → 不拦截（防误伤）
	resp = &dto.OpenAITextResponse{Id: "resp_normal", Usage: dto.Usage{TotalTokens: 100}, Choices: []dto.OpenAITextResponseChoice{{Message: dto.Message{Role: "assistant", Content: "今天我们聊聊「已被临时限制访问」这个词"}}}}
	assert.Equal(t, "", upstreamAntiProbeSentinelText(resp))
	// 正文标记 + 小 usage → 拦截兜底
	resp = &dto.OpenAITextResponse{Id: "resp_x", Usage: dto.Usage{TotalTokens: 1}, Choices: []dto.OpenAITextResponseChoice{{Message: dto.Message{Role: "assistant", Content: "当前网络的请求存在异常，已被临时限制访问，请稍后再试。"}}}}
	assert.Contains(t, upstreamAntiProbeSentinelText(resp), "已被临时限制访问")
}

func TestIsSSEFormattedBody(t *testing.T) {
	assert.True(t, isSSEFormattedBody([]byte("data: {\"id\":1}\n")))
	assert.True(t, isSSEFormattedBody([]byte("data:{\"id\":1}\n")))
	assert.True(t, isSSEFormattedBody([]byte("\n\nevent: ping\ndata: {}\n")))
	assert.True(t, isSSEFormattedBody([]byte("\xef\xbb\xbfdata: {}\n")))
	assert.False(t, isSSEFormattedBody([]byte("{\"id\":\"resp_1\"}")))
	assert.False(t, isSSEFormattedBody([]byte("")))
	assert.False(t, isSSEFormattedBody([]byte("data {")))
}
