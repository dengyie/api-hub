package helper

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// FrameCarriesVisibleContent 决定断流后能否换渠道：结构帧（role-only delta、
// typed 生命周期事件、usage 帧）对用户不可见，不该被当成已交付内容。
func TestFrameCarriesVisibleContent(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		data string
		want bool
	}{
		{
			name: "openai role-only delta is structural",
			data: `{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			want: false,
		},
		{
			name: "openai empty delta with null finish_reason is structural",
			data: `{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":null}]}`,
			want: false,
		},
		{
			name: "openai content delta is visible",
			data: `{"id":"c1","choices":[{"index":0,"delta":{"content":"Hi"}}]}`,
			want: true,
		},
		{
			name: "openai reasoning delta is visible",
			data: `{"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"thinking"}}]}`,
			want: true,
		},
		{
			name: "openai finish marker is visible",
			data: `{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			want: true,
		},
		{
			name: "openai tool_calls delta is visible",
			data: `{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1"}]}}]}`,
			want: true,
		},
		{
			name: "claude message_start is structural",
			data: `{"type":"message_start","message":{"id":"m1","role":"assistant","content":[]}}`,
			want: false,
		},
		{
			name: "opencode2api typed status is structural",
			data: `{"type":"messageStatus","status":"running"}`,
			want: false,
		},
		{
			name: "claude thinking delta is visible",
			data: `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
			want: true,
		},
		{
			name: "claude text delta is visible",
			data: `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
			want: true,
		},
		{
			name: "claude empty text block start is structural",
			data: `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			want: false,
		},
		{
			name: "claude tool_use block start is visible",
			data: `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"read"}}`,
			want: true,
		},
		{
			name: "responses output text delta is visible",
			data: `{"type":"response.output_text.delta","delta":"Hi"}`,
			want: true,
		},
		{
			name: "responses created is structural",
			data: `{"type":"response.created"}`,
			want: false,
		},
		{
			name: "gemini text parts is visible",
			data: `{"candidates":[{"content":{"parts":[{"text":"Hi"}],"role":"model"},"index":0}]}`,
			want: true,
		},
		{
			name: "gemini empty text part is structural",
			data: `{"candidates":[{"content":{"parts":[{"text":""}],"role":"model"},"index":0}]}`,
			want: false,
		},
		{
			name: "usage only frame is structural",
			data: `{"id":"c1","usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`,
			want: false,
		},
		{
			name: "error frame carries no answer",
			data: `{"error":{"message":"Concurrency limit exceeded for user, please retry later"}}`,
			want: false,
		},
		{
			name: "done is not content",
			data: `[DONE]`,
			want: false,
		},
		{
			name: "unknown shape stays conservative",
			data: `{"vendor_payload":{"opaque":true}}`,
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, FrameCarriesVisibleContent(tc.data))
		})
	}
}
