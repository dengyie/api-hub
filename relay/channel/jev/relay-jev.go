package jev

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/dengyie/apihub/common"
	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relay/helper"
	"github.com/dengyie/apihub/relaykit/dto"
	"github.com/dengyie/apihub/relaykit/types"
	"github.com/dengyie/apihub/service"

	"github.com/gin-gonic/gin"
)

// systemOneResponse is the System One decision envelope: one calibrated
// verdict per named question. Everything besides answers passes through to
// the client untouched. Bivia-flavored upstreams additionally report usage.
type systemOneResponse struct {
	Model   string          `json:"model"`
	Answers json.RawMessage `json:"answers"`
	Usage   *systemOneUsage `json:"usage"`
}

type systemOneUsage struct {
	Questions      int `json:"questions"`
	InputTokens    int `json:"input_tokens"`
	OutputTokens   int `json:"output_tokens"`
	BillableTokens int `json:"billable_tokens"`
}

// biviaStateString renders the jev state into the string Bivia requires:
// JSON string literals become their text value, objects/arrays become compact
// JSON text (lossless — Bivia reads the structure as text), anything else
// passes through trimmed.
func biviaStateString(state json.RawMessage) string {
	trimmed := strings.TrimSpace(string(state))
	if trimmed == "" {
		return ""
	}
	var asString string
	if err := common.Unmarshal([]byte(trimmed), &asString); err == nil {
		return asString
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(trimmed)); err == nil {
		return buf.String()
	}
	return trimmed
}

func readDecisionBody(resp *http.Response) ([]byte, *systemOneResponse, *types.NewAPIError) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		service.CloseResponseBodyGracefully(resp)
		return nil, nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	service.CloseResponseBodyGracefully(resp)

	var decision systemOneResponse
	if err := common.Unmarshal(responseBody, &decision); err != nil {
		return nil, nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	if len(decision.Answers) == 0 {
		return nil, nil, types.NewError(errors.New("jev returned an empty answers map"), types.ErrorCodeBadResponseBody)
	}
	return responseBody, &decision, nil
}

// buildUsage prefers the upstream's own usage report (Bivia reports
// billable_tokens per call) and falls back to estimating tokens over the wire
// payloads for native jev upstreams, which carry no usage.
func buildUsage(c *gin.Context, info *relaycommon.RelayInfo, responseBody []byte, decision *systemOneResponse) *dto.Usage {
	if decision.Usage != nil && decision.Usage.BillableTokens > 0 {
		prompt := max(decision.Usage.InputTokens, 0)
		total := max(decision.Usage.BillableTokens, 0)
		return &dto.Usage{
			PromptTokens:     prompt,
			CompletionTokens: max(total-prompt, 0),
			TotalTokens:      total,
		}
	}
	return synthesizeUsage(c, info, responseBody)
}

// synthesizeUsage bills on estimated tokens over the wire payloads: upstream
// System One responses carry no usage of their own.
func synthesizeUsage(c *gin.Context, info *relaycommon.RelayInfo, responseBody []byte) *dto.Usage {
	promptTokens := 0
	if requestJSON, err := common.Marshal(info.Request); err == nil {
		promptTokens = service.EstimateTokenByModel(info.UpstreamModelName, string(requestJSON))
	}
	return service.ResponseText2Usage(c, string(responseBody), info.UpstreamModelName, promptTokens)
}

// jevSystemOneHandler serves the native protocol: the client already speaks
// System One, so the upstream decision envelope passes through verbatim.
func jevSystemOneHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	responseBody, decision, apiErr := readDecisionBody(resp)
	if apiErr != nil {
		return nil, apiErr
	}

	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(responseBody)

	return buildUsage(c, info, responseBody, decision), nil
}

// jevChatProbeHandler serves the channel-test probe: it wraps the answers map
// into an OpenAI text response so the admin test button can judge the channel
// by its own wire format.
func jevChatProbeHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	responseBody, decision, apiErr := readDecisionBody(resp)
	if apiErr != nil {
		return nil, apiErr
	}

	response := dto.TextResponse{
		Id:      helper.GetResponseID(c),
		Object:  "chat.completion",
		Created: common.GetTimestamp(),
		Model:   info.UpstreamModelName,
		Choices: []dto.OpenAITextResponseChoice{
			{
				Index:        0,
				Message:      dto.Message{Role: "assistant", Content: string(decision.Answers)},
				FinishReason: "stop",
			},
		},
	}
	usage := synthesizeUsage(c, info, responseBody)
	response.Usage = *usage

	jsonResponse, err := common.Marshal(response)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(jsonResponse)

	return usage, nil
}
