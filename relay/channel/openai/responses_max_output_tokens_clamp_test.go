package openai

import (
	"net/http/httptest"
	"testing"

	"github.com/dengyie/apihub/constant"
	"github.com/dengyie/apihub/loadbalancer"
	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relaykit/dto"
	"github.com/dengyie/apihub/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Responses 请求不经过 chat 侧的 SanitizeModelCapabilitiesAndLimits，
// 所以 max_output_tokens 的钳制必须单独接进来。没接的时候，学习链路是通的
// （ProcessChannelError 已经把上限记下来了），但只有 chat 请求受益：
// 每个 Responses 请求都要先吃一次同样的 400，再靠重试成功。
//
// 这里既覆盖钳制语义本身，也覆盖它确实挂在 ConvertOpenAIResponsesRequest
// 的返回路径上 —— 只测 helper 而漏掉挂载，等于测了一个没人调用的函数。

func responsesUpstreamInfo(channelID int, upstreamModel, originModel string) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			ChannelId:         channelID,
			UpstreamModelName: upstreamModel,
		},
		OriginModelName: originModel,
	}
}

func TestClampResponsesMaxOutputTokensUsesLearnedLimit(t *testing.T) {
	const channelID = 9401
	const upstream = "vendor/slash-model"
	loadbalancer.RecordMaxCompletionTokensLimit(channelID, upstream, 65536)

	requested := uint(128000)
	original := requested
	req := dto.OpenAIResponsesRequest{MaxOutputTokens: &requested}

	got := clampResponsesMaxOutputTokens(req, responsesUpstreamInfo(channelID, upstream, "client-alias"))

	require.NotNil(t, got.MaxOutputTokens)
	assert.Equal(t, uint(65536), *got.MaxOutputTokens, "必须钳到已学到的上限")
	assert.Equal(t, original, requested, "不得原地改写调用方指针指向的值")
	assert.NotSame(t, req.MaxOutputTokens, got.MaxOutputTokens, "必须新建值再挂回去，避免污染上层请求")
}

func TestClampResponsesMaxOutputTokensFallsBackToOriginModelName(t *testing.T) {
	const channelID = 9402
	const origin = "client-model-with-limit"
	loadbalancer.RecordMaxCompletionTokensLimit(channelID, origin, 4096)

	requested := uint(32000)
	req := dto.OpenAIResponsesRequest{MaxOutputTokens: &requested}

	got := clampResponsesMaxOutputTokens(req, responsesUpstreamInfo(channelID, "", origin))
	require.NotNil(t, got.MaxOutputTokens)
	assert.Equal(t, uint(4096), *got.MaxOutputTokens)

	// 未学过的模型不得被凭空钳制 —— 0 表示「不知道上限」，不是「上限为 0」。
	other := uint(32000)
	req2 := dto.OpenAIResponsesRequest{MaxOutputTokens: &other}
	got2 := clampResponsesMaxOutputTokens(req2, responsesUpstreamInfo(channelID, "never-learned", "never-learned"))
	require.NotNil(t, got2.MaxOutputTokens)
	assert.Equal(t, uint(32000), *got2.MaxOutputTokens)
}

func TestClampResponsesMaxOutputTokensNoOpCases(t *testing.T) {
	requested := uint(999999)
	req := dto.OpenAIResponsesRequest{MaxOutputTokens: &requested}

	// nil info
	got := clampResponsesMaxOutputTokens(req, nil)
	require.NotNil(t, got.MaxOutputTokens)
	assert.Equal(t, requested, *got.MaxOutputTokens)

	// 没有 max_output_tokens
	empty := dto.OpenAIResponsesRequest{}
	assert.Nil(t, clampResponsesMaxOutputTokens(empty, responsesUpstreamInfo(9403, "m", "m")).MaxOutputTokens)

	// 0 视为「未指定」
	zero := uint(0)
	zeroReq := dto.OpenAIResponsesRequest{MaxOutputTokens: &zero}
	gotZero := clampResponsesMaxOutputTokens(zeroReq, responsesUpstreamInfo(9403, "m", "m"))
	require.NotNil(t, gotZero.MaxOutputTokens)
	assert.Equal(t, uint(0), *gotZero.MaxOutputTokens)
}

// TestConvertOpenAIResponsesRequestClampsOnFastPath 验证挂载点：
// 无推理后缀、无推理转换时走的是快速返回路径，也必须钳制。
func TestConvertOpenAIResponsesRequestClampsOnFastPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const channelID = 9404
	const upstream = "fast-path-model"
	loadbalancer.RecordMaxCompletionTokensLimit(channelID, upstream, 8192)

	requested := uint(64000)
	request := dto.OpenAIResponsesRequest{Model: upstream, MaxOutputTokens: &requested}
	info := responsesUpstreamInfo(channelID, upstream, upstream)
	info.RelayFormat = types.RelayFormatOpenAIResponses

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)

	converted, err := (&Adaptor{}).ConvertOpenAIResponsesRequest(c, info, request)
	require.NoError(t, err)

	convertedRequest, ok := converted.(dto.OpenAIResponsesRequest)
	require.True(t, ok, "快速路径应原样返回 Responses 请求，实际类型 %T", converted)
	require.NotNil(t, convertedRequest.MaxOutputTokens)
	assert.Equal(t, uint(8192), *convertedRequest.MaxOutputTokens,
		"Responses 快速路径必须钳制 max_output_tokens，否则每个请求都要先吃一次 400")
	assert.Equal(t, uint(64000), requested, "调用方请求对象不得被就地改写")
}
