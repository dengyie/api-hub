package openai

import (
	"encoding/json"
	"strings"
	"testing"

	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relaykit/dto"
)

// reasoning 微帧合并的回归测试。生产帧形取自 2026-11-10 对渠道 #118
// (cpa-yimingtalk) 的定点实测：glm-5.3-flash 的思考以原生 reasoning_content
// 字段一词一帧下发（"The" / " user" / " is"…），没有 <think> 标签。
// ZCode 按帧建 reasoning part，一词一帧 = 一词一思考块。

func coalescePolicy(disable bool) *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ChannelSetting: dto.ChannelSettings{DisableReasoningCoalesce: disable}},
	}
}

// 生产帧形：一词一帧的纯 reasoning 增量。
func wordFrame(word string) string {
	return `{"id":"34ccefde","object":"chat.completion.chunk","created":1794245067,"model":"glm-5.3-flash","choices":[{"index":0,"delta":{"reasoning_content":"` + word + `"},"finish_reason":null}]}`
}

func frameReasoning(t *testing.T, frame string) string {
	t.Helper()
	var obj struct {
		Choices []struct {
			Delta struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(frame), &obj); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, frame)
	}
	if len(obj.Choices) != 1 {
		t.Fatalf("合并帧必须单 choice: %s", frame)
	}
	return obj.Choices[0].Delta.ReasoningContent
}

// 生产回归（2026-11-10 渠道 #118 实测帧形）：上游在每个 delta 帧里都重复
// 携带 "role":"assistant" 且一词一帧。首帧立即透传（TTFT），其后逐词帧
// 必须被缓冲合并——role 不得成为旁路理由。
func TestReasoningCoalesceMergesRoleRepeatedWordFrames(t *testing.T) {
	info := coalescePolicy(false)
	roleWordFrame := func(word string) string {
		return `{"id":"34ccefde","model":"glm-5.3-flash","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"` + word + `"},"finish_reason":null}]}`
	}
	// 首帧：立即透传
	if frames := coalesceReasoningStreamFrame(info, roleWordFrame("The")); len(frames) != 1 || frames[0] != roleWordFrame("The") {
		t.Fatalf("首帧必须透传, got %v", frames)
	}
	// 后续帧：缓冲直到阈值
	words := []string{" user", " is", " asking"}
	for i, w := range words {
		if frames := coalesceReasoningStreamFrame(info, roleWordFrame(w)); len(frames) != 0 {
			t.Fatalf("第 %d 个逐词帧应被缓冲, got %v", i+2, frames)
		}
	}
	// 等效阈值前的 content 过渡帧：先冲刷缓冲（含重复 role 的信封），再透传
	contentFrame := `{"choices":[{"index":0,"delta":{"role":"assistant","content":"正文"},"finish_reason":null}]}`
	frames := coalesceReasoningStreamFrame(info, contentFrame)
	if len(frames) != 2 {
		t.Fatalf("冲刷+透传应产出 2 帧, got %d", len(frames))
	}
	if got := frameReasoning(t, frames[0]); got != " user is asking" {
		t.Fatalf("合并文本必须保序全量: got %q", got)
	}
	if !strings.Contains(frames[0], `"id":"34ccefde"`) || !strings.Contains(frames[0], `"model":"glm-5.3-flash"`) {
		t.Fatalf("合并帧必须复用原信封字段: %s", frames[0])
	}
	if !strings.Contains(frames[1], `"content":"正文"`) {
		t.Fatalf("content 帧必须原样透传: %s", frames[1])
	}
}

// 逐词帧被缓冲合并：字节数到阈值才出一帧，拼接文本与原词序一致。
func TestReasoningCoalesceMergesWordFrames(t *testing.T) {
	info := coalescePolicy(false)
	var words []string
	var frames []string
	// 首帧透传（SawPure 语义），其后缓冲至阈值
	coalesceReasoningStreamFrame(info, wordFrame(" first"))
	words = append(words, " first")
	for i := 0; i < 64 && len(frames) == 0; i++ {
		w := " word" + strings.Repeat("x", i%7)
		words = append(words, w)
		frames = coalesceReasoningStreamFrame(info, wordFrame(w))
	}
	if len(frames) != 1 {
		t.Fatalf("阈值到达时应冲刷出一帧, got %d 帧", len(frames))
	}
	if got := frameReasoning(t, frames[0]); got != strings.Join(words[1:], "") {
		t.Fatalf("合并文本必须保序全量: got %q", got)
	}
	if !strings.Contains(frames[0], `"id":"34ccefde"`) || !strings.Contains(frames[0], `"model":"glm-5.3-flash"`) {
		t.Fatalf("合并帧必须复用原信封字段: %s", frames[0])
	}
}

// 阈值未到时帧被吞掉（返回 0 帧）；首帧透传不计入缓冲。
func TestReasoningCoalesceSwallowsBelowThreshold(t *testing.T) {
	info := coalescePolicy(false)
	if frames := coalesceReasoningStreamFrame(info, wordFrame("The")); len(frames) != 1 {
		t.Fatalf("首帧必须透传, got %v", frames)
	}
	if frames := coalesceReasoningStreamFrame(info, wordFrame(" user")); len(frames) != 0 {
		t.Fatalf("小增量帧应被缓冲, got %v", frames)
	}
	if !info.ReasoningCoalesce.Held || info.ReasoningCoalesce.Pending != " user" {
		t.Fatalf("缓冲状态不对: %+v", info.ReasoningCoalesce)
	}
}

// 首帧带 role（生产帧形第一帧是 role+reasoning）：必须立即透传，不缓冲，
// 保住客户端 TTFT 观感。
func TestReasoningCoalesceRoleFramePassesThrough(t *testing.T) {
	info := coalescePolicy(false)
	roleFrame := `{"id":"34ccefde","model":"glm-5.3-flash","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"The"},"finish_reason":null}]}`
	frames := coalesceReasoningStreamFrame(info, roleFrame)
	if len(frames) != 1 || frames[0] != roleFrame {
		t.Fatalf("role 帧必须原样透传, got %v", frames)
	}
	if info.ReasoningCoalesce.Held {
		t.Fatalf("role 帧不得进入缓冲")
	}
}

// 思考转正文的过渡帧（content）必须先冲刷缓冲再透传，保序。
func TestReasoningCoalesceFlushBeforeContent(t *testing.T) {
	info := coalescePolicy(false)
	coalesceReasoningStreamFrame(info, wordFrame("The"))
	coalesceReasoningStreamFrame(info, wordFrame(" user"))
	coalesceReasoningStreamFrame(info, wordFrame(" is"))
	contentFrame := `{"choices":[{"index":0,"delta":{"content":"正文"},"finish_reason":null}]}`
	frames := coalesceReasoningStreamFrame(info, contentFrame)
	if len(frames) != 2 {
		t.Fatalf("冲刷+透传应产出 2 帧, got %d", len(frames))
	}
	if got := frameReasoning(t, frames[0]); got != " user is" {
		t.Fatalf("冲刷帧文本不对: %q", got)
	}
	if !strings.Contains(frames[1], `"content":"正文"`) {
		t.Fatalf("content 帧必须原样透传: %s", frames[1])
	}
}

// finish / usage / 多 choice 帧同样先冲刷再透传。
func TestReasoningCoalesceFlushBeforeTerminalFrames(t *testing.T) {
	cases := []struct {
		name  string
		frame string
	}{
		{"finish", `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`},
		{"usage", `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`},
		{"multi-choice", `{"choices":[{"index":0,"delta":{"reasoning_content":"a"}},{"index":1,"delta":{"reasoning_content":"b"}}]}`},
		{"tool-call", `{"choices":[{"index":0,"delta":{"tool_calls":[{"id":"c1","function":{"name":"Bash","arguments":"{}"}}]}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := coalescePolicy(false)
			coalesceReasoningStreamFrame(info, wordFrame("The")) // 首帧透传
			coalesceReasoningStreamFrame(info, wordFrame(" more"))
			frames := coalesceReasoningStreamFrame(info, tc.frame)
			if len(frames) != 2 {
				t.Fatalf("应冲刷+透传 2 帧, got %d", len(frames))
			}
			if got := frameReasoning(t, frames[0]); got != " more" {
				t.Fatalf("冲刷文本不对: %q", got)
			}
			if frames[1] != tc.frame {
				t.Fatalf("终态帧必须原样透传: %s", frames[1])
			}
		})
	}
}

// 单个大帧（≥阈值）立即冲刷：大帧上游零行为变化。
func TestReasoningCoalesceBigFramePassesImmediately(t *testing.T) {
	info := coalescePolicy(false)
	big := strings.Repeat("词", 200) // 600 字节 > 阈值
	frames := coalesceReasoningStreamFrame(info, wordFrame(big))
	if len(frames) != 1 {
		t.Fatalf("大帧应立即冲刷为一帧, got %d", len(frames))
	}
	if got := frameReasoning(t, frames[0]); got != big {
		t.Fatalf("大帧文本必须原样: got %d bytes want %d bytes", len(got), len(big))
	}
}

// takeReasoningFlushFrame 幂等复位：取走后再次取应为空。
func TestTakeReasoningFlushFrameResets(t *testing.T) {
	info := coalescePolicy(false)
	coalesceReasoningStreamFrame(info, wordFrame("The")) // 首帧透传
	coalesceReasoningStreamFrame(info, wordFrame(" more"))
	first := takeReasoningFlushFrame(info)
	if first == "" {
		t.Fatalf("有缓冲时必须返回合并帧")
	}
	if got := frameReasoning(t, first); got != " more" {
		t.Fatalf("合并文本不对: %q", got)
	}
	if second := takeReasoningFlushFrame(info); second != "" {
		t.Fatalf("取走后必须复位, got %q", second)
	}
	if info.ReasoningCoalesce.Held || info.ReasoningCoalesce.Pending != "" {
		t.Fatalf("状态未复位: %+v", info.ReasoningCoalesce)
	}
}

// 上游用 reasoning 镜像键（无 reasoning_content）时，合并文本必须补写进
// reasoning_content，且不得残留镜像键造成客户端双读重复。
func TestReasoningCoalesceMirrorReasoningKey(t *testing.T) {
	info := coalescePolicy(false)
	mirror := `{"id":"m1","model":"m","choices":[{"index":0,"delta":{"reasoning":"The"},"finish_reason":null}]}`
	if frames := coalesceReasoningStreamFrame(info, mirror); len(frames) != 1 {
		t.Fatalf("首帧必须透传, got %v", frames)
	}
	if frames := coalesceReasoningStreamFrame(info, mirror); len(frames) != 0 {
		t.Fatalf("镜像键小帧应被缓冲, got %v", frames)
	}
	coalesceReasoningStreamFrame(info, wordFrame(" more"))
	frame := takeReasoningFlushFrame(info)
	if frame == "" {
		t.Fatalf("必须产出合并帧")
	}
	if got := frameReasoning(t, frame); got != "The more" {
		t.Fatalf("镜像键载荷必须并入合并帧: %q", got)
	}
	if strings.Contains(frame, `"reasoning":`) {
		t.Fatalf("镜像键必须被移除避免双读: %s", frame)
	}
}

// 渠道豁免开关：DisableReasoningCoalesce 时整路旁路、零缓冲。
func TestReasoningCoalesceDisabledBypass(t *testing.T) {
	info := coalescePolicy(true)
	for i := 0; i < 100; i++ {
		frames := coalesceReasoningStreamFrame(info, wordFrame(" word"))
		if len(frames) != 1 || frames[0] != wordFrame(" word") {
			t.Fatalf("豁免渠道必须逐帧透传, got %v", frames)
		}
	}
	if info.ReasoningCoalesce.Held {
		t.Fatalf("豁免渠道不得有缓冲")
	}
}

// 与逐帧状态机改写器互斥：ThinkTagToReasoning 开启时整路旁路。
func TestReasoningCoalesceBypassesStatefulTransformers(t *testing.T) {
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelSetting: dto.ChannelSettings{ThinkTagToReasoning: true}}}
	if reasoningCoalesceEligible(info) {
		t.Fatalf("ThinkTagToReasoning 开启时必须旁路合并层")
	}
	info2 := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelSetting: dto.ChannelSettings{ReasoningAsContent: true}}}
	if reasoningCoalesceEligible(info2) {
		t.Fatalf("ReasoningAsContent 开启时必须旁路合并层")
	}
	info3 := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelSetting: dto.ChannelSettings{ThinkingToContent: true}}}
	if reasoningCoalesceEligible(info3) {
		t.Fatalf("ThinkingToContent 开启时必须旁路合并层")
	}
	if reasoningCoalesceEligible(nil) {
		t.Fatalf("nil info 必须旁路")
	}
}

// 无 reasoning 字样的帧走快速旁路：先冲刷再透传。
func TestReasoningCoalesceFastPathFlushesFirst(t *testing.T) {
	info := coalescePolicy(false)
	coalesceReasoningStreamFrame(info, wordFrame("The")) // 首帧透传
	coalesceReasoningStreamFrame(info, wordFrame(" word"))
	plain := `{"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]}`
	frames := coalesceReasoningStreamFrame(info, plain)
	if len(frames) != 2 || frameReasoning(t, frames[0]) != " word" || frames[1] != plain {
		t.Fatalf("快速旁路必须先冲刷: %v", frames)
	}
}
