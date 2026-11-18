package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/dengyie/apihub/common"
	"github.com/dengyie/apihub/constant"
	"github.com/dengyie/apihub/logger"
	"github.com/dengyie/apihub/relay/channel/openrouter"
	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relay/helper"
	"github.com/dengyie/apihub/relaykit/dto"
	"github.com/dengyie/apihub/relaykit/types"
	"github.com/dengyie/apihub/service"

	"github.com/gin-gonic/gin"
)

func OaiStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, "invalid response or response body")
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	defer service.CloseResponseBodyGracefully(resp)

	model := info.UpstreamModelName
	var responseId string
	var createAt int64 = 0
	var systemFingerprint string
	var containStreamUsage bool
	var responseTextBuilder strings.Builder
	var toolCount int
	var usage = &dto.Usage{}
	var lastStreamData string
	var secondLastStreamData string // 保留倒数第二个stream data；部分兼容网关把完整usage放在倒数第二个事件
	seenStreamToolCalls := make(map[string]struct{})
	var streamFunctionCallNames []string

	if info != nil {
		if info.StreamStatus == nil {
			info.StreamStatus = relaycommon.NewStreamStatus()
		}
		info.StreamStatus.RequireTerminal()
	}

	// 智能负载：首字节前 TTFT 超时返回可重试错误，触发透明换渠道（客户端无感知）
	if streamErr := helper.ToNewAPIError(helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		data = NormalizeStreamFrame(data)
		// 流式哨兵检测：哨兵 chunk 同样以数据帧送达（id 含 freeipblk/freeblock）。
		// 不拦截会把固定拦截文案当助手回复透传给客户端并照常计费。仅在尚未向客户
		// 端下发任何数据帧时判定（此时中断无计费副作用），命中即终止走 failover。
		if !c.Writer.Written() && info.ReceivedVisibleContentBytes == 0 {
			if sentinelText := streamSentinelText(data); sentinelText != "" {
				logger.LogWarn(c, "upstream anti-probe sentinel detected in stream, aborting: %s", sentinelText)
				sr.Stop(types.NewOpenAIError(fmt.Errorf("上游反滥用哨兵拦截: %s", sentinelText), types.ErrorCodeUpstreamSentinelBlocked, http.StatusBadGateway))
				return
			}
		}
		if lastStreamData != "" {
			if err := HandleStreamFormat(c, info, lastStreamData, info.ChannelSetting.ForceFormat, info.ChannelSetting.ThinkingToContent); err != nil {
				common.SysLog("error handling stream format: " + err.Error())
				if c != nil && c.Writer != nil && !c.Writer.Written() {
					sr.Stop(err)
					return
				}
				sr.Error(err)
			}
		}
		if len(data) > 0 {
			if lastStreamData != "" {
				secondLastStreamData = lastStreamData
			}

			lastStreamData = data
			observeStreamChoices(info, data, seenStreamToolCalls, &streamFunctionCallNames)
			if err := processTokenData(info, data, &responseTextBuilder, &toolCount); err != nil {
				logger.LogError(c, "error processing stream token data: "+err.Error())
				if c != nil && c.Writer != nil && !c.Writer.Written() {
					sr.Stop(err)
					return
				}
				sr.Error(err)
			}
		}
	})); streamErr != nil {
		// 断流：客户端已经收到了中断前的正文，这一段用量必须结算，否则白送。
		// usage 连同错误一起返回，由调用方统一转交 controller；早退丢掉它，
		// compatible_handler 看到 err != nil 就直接 return，走不到
		// PostTextConsumeQuota。末帧的 usage 帧在断流时通常也没到，所以跟成功
		// 路径的兜底一样，从已累积的正文估算。
		//
		// 判据是真正累积到的产出而非「收到了帧」：上游错误同样以数据帧送达，
		// 而空产出会被估成整段 prompt —— 那等于给一次失败的请求收一遍全款。
		// responseTextBuilder 同时收了正文、reasoning 与工具名/参数，非空即代表
		// 客户端确实拿到了可计费内容。
		//
		// 断流前尽力冲刷 reasoning 微帧缓冲：缓冲里是已从上游收到、但还没下发
		// 的思考增量，扣掉等于客户端看到的思考缺尾巴。写失败静默忽略（写通道
		// 可能已死）。断流同样输出合并摘要/帧风暴告警——上游中途断流不影响
		// 「它是不是逐词上游」这个事实。
		_ = writeCoalescedFlush(c, info, info != nil && info.ChannelSetting.ForceFormat, info != nil && info.ChannelSetting.ThinkingToContent)
		logReasoningCoalesceSummary(c, info)
		interrupted := service.InterruptedTextUsage(c, info, responseTextBuilder.String())
		if interrupted != nil {
			interrupted.CompletionTokens += toolCount * 7
		}
		return interrupted, streamErr
	}

	info.StreamStatus.RequireTerminal()

	// 处理最后的响应
	shouldSendLastResp := true
	if err := handleLastResponse(lastStreamData, &responseId, &createAt, &systemFingerprint, &model, &usage,
		&containStreamUsage, info, &shouldSendLastResp); err != nil {
		logger.LogError(c, fmt.Sprintf("error handling last response: %s, lastStreamData: [%s]", err.Error(), lastStreamData))
	}

	// 部分兼容网关把完整的累计usage附在倒数第二个事件上，随后发送一个空的最后事件。
	// 仅当最后一个事件没有有效usage时，回退到倒数第二个事件的完整快照。
	usageFrame := lastStreamData
	if !containStreamUsage && secondLastStreamData != "" {
		var streamResp struct {
			Usage *dto.Usage `json:"usage"`
		}
		err := common.Unmarshal([]byte(secondLastStreamData), &streamResp)
		if err == nil && streamResp.Usage != nil &&
			streamResp.Usage.PromptTokens > 0 &&
			(streamResp.Usage.CompletionTokens > 0 || streamResp.Usage.TotalTokens > 0) {
			usage = dto.MergeUsageNonZero(usage, streamResp.Usage)
			containStreamUsage = true
			usageFrame = secondLastStreamData

			if common.DebugEnabled {
				logger.LogDebug(c, "usage extracted from second last SSE: PromptTokens=%d, CompletionTokens=%d, TotalTokens=%d, InputTokens=%d, OutputTokens=%d",
					usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens,
					usage.InputTokens, usage.OutputTokens)
			}
		}
	}

	// reasoning 微帧合并的终态冲刷：缓冲的思考增量必须先于终帧送达。
	// OpenAI 格式的终帧在下方 sendStreamData 直写（不经 HandleStreamFormat
	// 过滤层），Claude/Gemini 格式的终帧在 HandleFinalResponse 内转换 ——
	// 两者的前置冲刷都在这里；HandleFinalResponse 入口还有幂等兜底。
	if err := writeCoalescedFlush(c, info, info.ChannelSetting.ForceFormat, info.ChannelSetting.ThinkingToContent); err != nil {
		logger.LogError(c, "reasoning coalesce flush failed: "+err.Error())
	}

	if info.RelayFormat == types.RelayFormatOpenAI {
		if shouldSendLastResp {
			_ = sendStreamData(c, info, lastStreamData, info.ChannelSetting.ForceFormat, info.ChannelSetting.ThinkingToContent)
		}
	}

	if !containStreamUsage {
		usage = service.DeliveredTextUsage(c, info, responseTextBuilder.String())
		usage.CompletionTokens += toolCount * 7
	}

	applyUsagePostProcessing(info, usage, common.StringToByteSlice(usageFrame))

	for _, name := range streamFunctionCallNames {
		info.CountBillableToolCall(dto.BuildInCallFunctionCall, name)
	}

	HandleFinalResponse(c, info, lastStreamData, responseId, createAt, model, systemFingerprint, usage, containStreamUsage)

	return usage, nil
}

// observeStreamChoices collects billable function call names and records the
// finish reason facts used by health sampling from one parsed chunk.
func observeStreamChoices(info *relaycommon.RelayInfo, data string, seen map[string]struct{}, names *[]string) {
	var streamResponse dto.ChatCompletionsStreamResponse
	if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
		return
	}
	for _, choice := range streamResponse.Choices {
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			if *choice.FinishReason == constant.FinishReasonContentFilter {
				info.PerformanceBusinessRejection = true
			}
			info.StreamStatus.MarkCompleted()
		}
		for i, tc := range choice.Delta.ToolCalls {
			name := strings.TrimSpace(tc.Function.Name)
			if name == "" {
				continue
			}
			toolIdx := i
			if tc.Index != nil {
				toolIdx = *tc.Index
			}
			fallbackKey := fmt.Sprintf("index\x00%d\x00%d\x00%s", choice.Index, toolIdx, name)
			activeKey := fmt.Sprintf("active\x00%d\x00%d\x00%s", choice.Index, toolIdx, name)
			callID := strings.TrimSpace(tc.ID)
			if callID != "" {
				idKey := fmt.Sprintf("id\x00%d\x00%s", choice.Index, callID)
				if _, ok := seen[idKey]; ok {
					continue
				}
				seen[idKey] = struct{}{}
				seen[activeKey] = struct{}{}
				if _, delayedID := seen[fallbackKey]; delayedID {
					delete(seen, fallbackKey)
					continue
				}
			} else {
				if _, ok := seen[fallbackKey]; ok {
					continue
				}
				if _, ok := seen[activeKey]; ok {
					continue
				}
				seen[fallbackKey] = struct{}{}
				seen[activeKey] = struct{}{}
			}
			*names = append(*names, name)
		}
	}
}

func OpenaiHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	var simpleResponse dto.OpenAITextResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	logger.LogDebug(c, "upstream response body: %s", responseBody)
	// 部分上游网关对非流式请求返回 SSE body（反滥用哨兵或强制流式），body 以 "data:" 开头
	// 会被下方 JSON 解析报 invalid character 'd' (bad_response_body)。嗅探到 SSE 时聚合
	// 流式 chunk 还原为非流式 JSON 再走统一解析。
	restoredBody, apiErr := guardNonStreamUpstreamBody(c, responseBody, "chat")
	if apiErr != nil {
		return nil, apiErr
	}
	responseBody = restoredBody
	// Unmarshal to simpleResponse
	if info.ChannelType == constant.ChannelTypeOpenRouter && info.ChannelOtherSettings.IsOpenRouterEnterprise() {
		// 尝试解析为 openrouter enterprise
		var enterpriseResponse openrouter.OpenRouterEnterpriseResponse
		err = common.Unmarshal(responseBody, &enterpriseResponse)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		if enterpriseResponse.Success {
			responseBody = enterpriseResponse.Data
		} else {
			logger.LogError(c, fmt.Sprintf("openrouter enterprise response success=false, data: %s", enterpriseResponse.Data))
			return nil, types.NewOpenAIError(fmt.Errorf("openrouter response success=false"), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
	}

	err = common.Unmarshal(responseBody, &simpleResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	if oaiError := simpleResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	// 上游反滥用哨兵以 200 + 正常 choice 形态返回（id 含 freeipblk/freeblock，内容为固定拦截
	// 文案），既不是真实回复也不该让测活假绿。识别后按上游错误抛出，走既有熔断/failover 链。
	// 专属错误码 upstream_sentinel_blocked：DecideRelayRetry 在 alwaysSkipCodes/2xx 闸门前
	// 截获换渠道重试，ClassifyBreakerEffect 归 BreakerSentinel（no-op，不熔断健康渠道）。
	if sentinelText := upstreamAntiProbeSentinelText(&simpleResponse); sentinelText != "" {
		return nil, types.NewOpenAIError(fmt.Errorf("上游反滥用哨兵拦截: %s", sentinelText), types.ErrorCodeUpstreamSentinelBlocked, http.StatusBadGateway)
	}

	info.ObserveResponseModel(simpleResponse.Model)
	for _, choice := range simpleResponse.Choices {
		if choice.FinishReason == constant.FinishReasonContentFilter {
			info.PerformanceBusinessRejection = true
			common.SetContextKey(c, constant.ContextKeyAdminRejectReason, "openai_finish_reason=content_filter")
			break
		}
	}

	for _, choice := range simpleResponse.Choices {
		for _, tc := range choice.Message.ParseToolCalls() {
			info.CountBillableToolCall(dto.BuildInCallFunctionCall, tc.Function.Name)
		}
	}

	forceFormat := false
	if info.ChannelSetting.ForceFormat {
		forceFormat = true
	}

	usageModified := false
	if simpleResponse.Usage.PromptTokens == 0 {
		completionTokens := simpleResponse.Usage.CompletionTokens
		if completionTokens == 0 {
			for _, choice := range simpleResponse.Choices {
				ctkm := service.CountTextToken(choice.Message.StringContent()+choice.Message.GetReasoningContent(), info.UpstreamModelName)
				completionTokens += ctkm
			}
		}
		fallbackUsage := &dto.Usage{
			PromptTokens:     info.GetEstimatePromptTokens(),
			CompletionTokens: completionTokens,
			TotalTokens:      info.GetEstimatePromptTokens() + completionTokens,
		}
		simpleResponse.Usage = *fallbackUsage
		usageModified = true
	}

	if ShouldApplyReasoningAsContent(info) {
		for i := range simpleResponse.Choices {
			msg := &simpleResponse.Choices[i].Message
			reasoning := msg.GetReasoningContent()
			if reasoning != "" {
				msg.ReasoningContent = nil
				msg.Reasoning = nil
				if msg.StringContent() == "" {
					msg.SetStringContent(reasoning)
				}
				forceFormat = true
			}
		}
	}

	applyUsagePostProcessing(info, &simpleResponse.Usage, responseBody)

	switch info.RelayFormat {
	case types.RelayFormatOpenAI:
		if usageModified {
			var bodyMap map[string]any
			err = common.Unmarshal(responseBody, &bodyMap)
			if err != nil {
				return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			}
			bodyMap["usage"] = simpleResponse.Usage
			responseBody, _ = common.Marshal(bodyMap)
		}
		if forceFormat {
			responseBody, err = common.Marshal(simpleResponse)
			if err != nil {
				return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
			}
		} else {
			break
		}
	case types.RelayFormatClaude:
		convertResult, err := service.ConvertResponse(c, info, types.RelayFormatClaude, &simpleResponse)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		claudeRespStr, err := common.Marshal(convertResult.Value)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		responseBody = claudeRespStr
	case types.RelayFormatGemini:
		convertResult, err := service.ConvertResponse(c, info, types.RelayFormatGemini, &simpleResponse)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		geminiRespStr, err := common.Marshal(convertResult.Value)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		responseBody = geminiRespStr
	}

	if info != nil && info.OriginModelName != "" {
		responseBody = helper.RewriteResponseModelBytes(info, responseBody)
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)

	return &simpleResponse.Usage, nil
}

// isSSEFormattedBody 嗅探 body 是否为 SSE 流式格式（"data:" 行）。上游网关偶发对
// 非流式请求回 SSE（如反滥用哨兵），content-type 也不可信（实测 application/json 下
// 仍出现 SSE body），因此以 body 前缀为准、content-type 仅作辅助信号。
func isSSEFormattedBody(body []byte) bool {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	trimmed = bytes.TrimPrefix(trimmed, []byte("\xef\xbb\xbf")) // BOM
	if bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte("data: ")) {
		return true
	}
	if bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("event: ")) {
		return true
	}
	return false
}

type aggregatedChatCompletion struct {
	body       []byte
	chunkCount int
}

// aggregateSSEAsChatCompletion 把 SSE chat.completion.chunk 流聚合为等价的非流式
// chat.completion JSON：choice 按 index 分桶，content 按 delta 拼接、tool_calls 按
// index 增量合并（流式函数调用分片到完整调用）、reasoning_content 增量累加、usage
// 取末个携带 chunk。error chunk 首个透传进聚合体 Error 字段（下游 GetOpenAIError
// 统一拦截，避免全错误流变成 200 空响应）；error chunk 不计入 chunkCount。
// object 硬编码 "chat.completion"（chunk 的 object 值不透传，严格客户端如
// Zod/Vercel AI SDK 会校验该字段）。无法解析任何 chunk 时返回错误（而非静默吞掉
// 原始 body）。
func aggregateSSEAsChatCompletion(body []byte) (*aggregatedChatCompletion, error) {
	type choiceAgg struct {
		content      strings.Builder
		reasoning    strings.Builder
		finishReason string
		toolCalls    map[int]*dto.ToolCallResponse
		toolOrder    []int
	}
	choices := make(map[int]*choiceAgg)
	var choiceOrder []int
	var (
		id, model  string
		usage      *dto.Usage
		chunkCount int
		hasChunk   bool
		// 首个 error chunk 透传进聚合体：上游错误若整条流仅以 error chunk 送达，
		// 不传播会让 OpenaiHandler 把无 choices 的聚合体当成功计费（假绿）。
		streamError any
	)
	choiceBucket := func(index int) *choiceAgg {
		bucket, ok := choices[index]
		if !ok {
			bucket = &choiceAgg{toolCalls: make(map[int]*dto.ToolCallResponse)}
			choices[index] = bucket
			choiceOrder = append(choiceOrder, index)
		}
		return bucket
	}
	for _, rawLine := range bytes.Split(body, []byte("\n")) {
		line := strings.TrimSpace(string(rawLine))
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk dto.ChatCompletionsStreamResponse
		if err := common.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		hasChunk = true
		if streamError == nil {
			chunkCount++
		}
		if chunk.Id != "" {
			id = chunk.Id
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if chunk.Error != nil {
			if streamError == nil {
				streamError = chunk.Error
			}
			continue
		}
		for _, choice := range chunk.Choices {
			bucket := choiceBucket(choice.Index)
			if choice.Delta.Content != nil {
				bucket.content.WriteString(*choice.Delta.Content)
			}
			// reasoning_content 与 content 同为逐 chunk 增量分片，必须累加而非覆盖
			// （覆盖只会留下最后一个微片段）。reasoning/reasoning_content 双来源
			// 共用同一 Builder：正常流两者不会交替出现，若出现也属同义字段。
			if choice.Delta.ReasoningContent != nil {
				bucket.reasoning.WriteString(*choice.Delta.ReasoningContent)
			} else if choice.Delta.Reasoning != nil {
				bucket.reasoning.WriteString(*choice.Delta.Reasoning)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				bucket.finishReason = *choice.FinishReason
			}
			for _, tc := range choice.Delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				existing, ok := bucket.toolCalls[idx]
				if !ok {
					clone := tc
					clone.Index = nil // 非流式 tool_calls 不携带 index 字段
					bucket.toolCalls[idx] = &clone
					bucket.toolOrder = append(bucket.toolOrder, idx)
					continue
				}
				// 增量分片：id/name 首块已带，arguments 逐块追加
				if tc.ID != "" {
					existing.ID = tc.ID
				}
				if tc.Function.Name != "" {
					existing.Function.Name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					existing.Function.Arguments += tc.Function.Arguments
				}
			}
		}
	}
	if !hasChunk {
		return nil, fmt.Errorf("上游返回 SSE body 但无可解析的 data chunk（前 120 字节: %.120s）", string(body))
	}

	completion := dto.OpenAITextResponse{
		Id:     id,
		Object: "chat.completion",
		Model:  model,
	}
	if streamError != nil {
		completion.Error = streamError
	}
	for _, index := range choiceOrder {
		bucket := choices[index]
		msg := dto.Message{Role: "assistant", Content: bucket.content.String()}
		if bucket.reasoning.Len() > 0 {
			reasoning := bucket.reasoning.String()
			msg.ReasoningContent = &reasoning
		}
		if len(bucket.toolOrder) > 0 {
			toolCalls := make([]dto.ToolCallResponse, 0, len(bucket.toolOrder))
			for _, idx := range bucket.toolOrder {
				toolCalls = append(toolCalls, *bucket.toolCalls[idx])
			}
			msg.SetToolCalls(toolCalls)
		}
		completion.Choices = append(completion.Choices, dto.OpenAITextResponseChoice{
			Index:        index,
			FinishReason: bucket.finishReason,
			Message:      msg,
		})
	}
	if usage != nil {
		completion.Usage = *usage
	}
	encoded, err := common.Marshal(completion)
	if err != nil {
		return nil, err
	}
	return &aggregatedChatCompletion{body: encoded, chunkCount: chunkCount}, nil
}

// extractResponsesCompletedFromSSE 从 responses 协议的 SSE 流中抽取终态事件的
// response 对象序列化为 JSON body。非流式 Responses 请求收到 SSE body 时（上游
// 强转流式/哨兵），response.completed / response.done / response.incomplete 携带
// 完整 response 对象，抽取它即可还原等价非流式 body；找不到终态事件返回错误。
func extractResponsesCompletedFromSSE(body []byte) ([]byte, error) {
	for _, rawLine := range bytes.Split(body, []byte("\n")) {
		line := strings.TrimSpace(string(rawLine))
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if err := common.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		switch event.Type {
		case "response.completed", "response.done", "response.incomplete", "response.failed":
			if len(event.Response) > 0 && string(event.Response) != "null" {
				return event.Response, nil
			}
		}
	}
	return nil, fmt.Errorf("上游返回 SSE body 但无可抽取的 responses 终态事件（前 120 字节: %.120s）", string(body))
}

// guardNonStreamUpstreamBody 统一处理「非流式请求收到 SSE body」的嗅探与还原。
// format 为 chat（chat.completion.chunk 聚合）或 responses（终态事件抽取）。
// 返回还原后的 JSON body；body 不是 SSE 时原样返回。
func guardNonStreamUpstreamBody(c *gin.Context, body []byte, format string) ([]byte, *types.NewAPIError) {
	if !isSSEFormattedBody(body) {
		return body, nil
	}
	var restored []byte
	var err error
	if format == "responses" {
		restored, err = extractResponsesCompletedFromSSE(body)
	} else {
		aggregated, aggErr := aggregateSSEAsChatCompletion(body)
		if aggErr == nil {
			restored = aggregated.body
			logger.LogWarn(c, "upstream returned SSE body for non-stream request, aggregated %d chunk(s)", aggregated.chunkCount)
		}
		err = aggErr
	}
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusBadGateway)
	}
	return restored, nil
}

// upstreamAntiProbeSentinelIDMarkers 是已实证的上游反滥用哨兵 id 标记（免费池对探针
// 返回 200 + 固定文案的伪正常回复）：freeipblk = IP 级临时限制，freeblock = 客户端
// 指纹级拦截。id 是精确判别式，正文标记仅在小额 usage 时兜底防漏。
const (
	upstreamSentinelIDMarkerIP  = "freeipblk"
	upstreamSentinelIDMarkerKey = "freeblock"
	upstreamSentinelContentMark = "已被临时限制访问"
)

func upstreamAntiProbeSentinelText(resp *dto.OpenAITextResponse) string {
	if resp == nil {
		return ""
	}
	if strings.Contains(resp.Id, upstreamSentinelIDMarkerIP) || strings.Contains(resp.Id, upstreamSentinelIDMarkerKey) {
		for _, choice := range resp.Choices {
			if content := choice.Message.StringContent(); content != "" {
				return content
			}
		}
		return "(哨兵未携带正文)"
	}
	// 无哨兵 id 时要求正文匹配拦截文案且上游上报的 usage 极小非零，避免误伤正常模型
	// 输出（上游省略 usage 时 TotalTokens=0，门槛失效，此时只信 id 判别式）。
	if resp.Usage.TotalTokens == 0 || resp.Usage.TotalTokens > 2 {
		return ""
	}
	for _, choice := range resp.Choices {
		if strings.Contains(choice.Message.StringContent(), upstreamSentinelContentMark) {
			return choice.Message.StringContent()
		}
	}
	return ""
}

// streamSentinelText 对单个流式数据帧做哨兵判别：只信 id 精确标记（流式形态
// 没有「小 usage」可做兜底门槛，正文匹配误伤面太大——模型输出讨论网络限制
// 的正常内容会被误杀）。
func streamSentinelText(data string) string {
	var chunk dto.ChatCompletionsStreamResponse
	if err := common.UnmarshalJsonStr(data, &chunk); err != nil {
		return ""
	}
	if strings.Contains(chunk.Id, upstreamSentinelIDMarkerIP) || strings.Contains(chunk.Id, upstreamSentinelIDMarkerKey) {
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				return *choice.Delta.Content
			}
		}
		return "(哨兵未携带正文)"
	}
	return ""
}

// responsesAntiProbeSentinelText 是哨兵判别的 Responses 协议变体：正文从
// output[].content[].text 抽取（Responses 协议没有 choices/message 结构），
// id 判别式与 usage 兜底门槛与 chat 形态完全一致。
func responsesAntiProbeSentinelText(resp *dto.OpenAIResponsesResponse) string {
	if resp == nil {
		return ""
	}
	contentOf := func() string {
		for _, output := range resp.Output {
			for _, part := range output.Content {
				if part.Text != "" {
					return part.Text
				}
			}
		}
		return ""
	}
	if strings.Contains(resp.ID, upstreamSentinelIDMarkerIP) || strings.Contains(resp.ID, upstreamSentinelIDMarkerKey) {
		if content := contentOf(); content != "" {
			return content
		}
		return "(哨兵未携带正文)"
	}
	if resp.Usage == nil || resp.Usage.TotalTokens == 0 || resp.Usage.TotalTokens > 2 {
		return ""
	}
	if content := contentOf(); strings.Contains(content, upstreamSentinelContentMark) {
		return content
	}
	return ""
}
