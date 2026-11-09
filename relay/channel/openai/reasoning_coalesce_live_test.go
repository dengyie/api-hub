package openai

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dengyie/apihub/constant"
	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relaykit/types"

	"github.com/gin-gonic/gin"
)

// 端到端管线回归：真 OaiStreamHandler + 生产帧形（#118 实测），断言客户端
// 看到的 reasoning 帧被合并。若单测全绿而本测试红，差异在管线接线。

func TestOaiStreamHandlerCoalescesLiveFrames(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, RelayFormat: types.RelayFormatOpenAI}

	frame := func(rc string) string {
		return "data: " + `{"id":"8ae31877","model":"glm-5.3-flash","object":"chat.completion.chunk","created":1794247339,"usage":null,"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"` + rc + `","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"finish_reason":null,"logprobs":null}]}` + "\n\n"
	}
	upstream := strings.Join([]string{
		frame("The"), frame(" user"), frame(" is"), frame(" asking"),
		"data: " + `{"id":"8ae31877","model":"glm-5.3-flash","object":"chat.completion.chunk","created":1794247339,"usage":null,"choices":[{"index":0,"delta":{"content":"answer text","function_call":null,"refusal":"","tool_calls":[]},"finish_reason":null}]}` + "\n\n",
		"data: " + `{"id":"8ae31877","model":"glm-5.3-flash","object":"chat.completion.chunk","created":1794247339,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n",
		"data: [DONE]\n\n",
	}, "")

	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       http.NoBody,
	}
	// 用真实 body reader
	resp.Body = io.NopCloser(bytes.NewReader([]byte(upstream)))

	usage, apiErr := OaiStreamHandler(c, info, resp)
	_ = usage
	if apiErr != nil {
		t.Fatalf("OaiStreamHandler 报错: %v", apiErr)
	}

	t.Logf("SendResponseCount=%d written=%v bodyLen=%d", info.SendResponseCount, c.Writer.Written(), w.Body.Len())
	out := w.Body.String()
	reasoningFrames := strings.Count(out, `"reasoning_content":"`)
	// 首帧透传 + content 帧前冲刷 1 帧 = 2 帧（3 个词帧合并进 1 帧）
	if reasoningFrames != 2 {
		t.Fatalf("客户端应看到 2 个 reasoning 帧, got %d\n输出:\n%s", reasoningFrames, out)
	}
	if !strings.Contains(out, `"reasoning_content":" user is asking"`) {
		t.Fatalf("合并帧文本必须完整保序:\n%s", out)
	}
	if strings.Count(out, "answer text") != 1 {
		t.Fatalf("content 必须原样透传")
	}
}

// 生产帧逐字节回归（2026-11-10 渠道 #118 第三次实测抓帧）：finish_reason 是
// 空串而非 null（该上游用 "" 表示进行中），choice 级还带 logprobs:null、
// 顶层 usage:null。曾因把空串 finish_reason 误判为终态导致合并整路失效。
func TestOaiStreamHandlerCoalescesProductionBytes(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, RelayFormat: types.RelayFormatOpenAI}

	// 2026-11-10 02:47 探针抓帧原文（仅换 reasoning 词）
	prodFrame := func(word string) string {
		return "data: " + `{"id":"a5ceec1f2b4e41d3a0a65f29b48422bc","model":"glm-5.3-flash","object":"chat.completion.chunk","created":1794249637,"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"` + word + `","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}],"usage":null}` + "\n\n"
	}
	upstream := strings.Join([]string{
		prodFrame("9"), prodFrame("."), prodFrame("8"), prodFrame(" >"), prodFrame(" 9"), prodFrame("."), prodFrame("1"), prodFrame("1"),
		"data: " + `{"id":"a5ceec1f2b4e41d3a0a65f29b48422bc","model":"glm-5.3-flash","object":"chat.completion.chunk","created":1794249637,"choices":[{"index":0,"delta":{"role":"assistant","content":"答案","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}],"usage":null}` + "\n\n",
		"data: " + `{"id":"a5ceec1f2b4e41d3a0a65f29b48422bc","model":"glm-5.3-flash","object":"chat.completion.chunk","created":1794249637,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n",
		"data: [DONE]\n\n",
	}, "")

	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
	resp.Body = io.NopCloser(bytes.NewReader([]byte(upstream)))

	if _, apiErr := OaiStreamHandler(c, info, resp); apiErr != nil {
		t.Fatalf("OaiStreamHandler 报错: %v", apiErr)
	}
	out := w.Body.String()
	n := strings.Count(out, `"reasoning_content":"`)
	// 8 个逐词帧：首帧透传 + 7 帧在 content 前合并冲刷 = 2 帧
	if n != 2 {
		t.Fatalf("生产帧形应合并为 2 个 reasoning 帧, got %d\n输出:\n%s", n, out)
	}
	// json.Marshal 会把 > 转义为 \u003e（标准行为，客户端解码无损）
	if !strings.Contains(out, `"reasoning_content":".8 \u003e 9.11"`) {
		t.Fatalf("合并文本必须保序全量:\n%s", out)
	}
	if strings.Count(out, "答案") != 1 {
		t.Fatalf("content 必须原样透传:\n%s", out)
	}
}
