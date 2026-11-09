package openai

import (
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	relaycommon "github.com/dengyie/apihub/relay/common"
)

// reasoning 微帧合并：「一个词一个思考块」故障家族的第四变体——部分公益站
// 上游以原生 reasoning_content 字段但一词一帧下发思考（2026-11-10 对渠道
// #118 cpa-yimingtalk 定点实测："The" / " user" / " is" 每词一个 SSE 帧）。
// 帧/块感知客户端（ZCode 按帧建 reasoning part）会把一次回答的思考炸成
// 数千个单词级块：UI 一词一换行、会话上下文膨胀。v29.30/v29.31 修的是
// reasoning 帧字段与答案错位，think_tag_split.go 修的是 <think> 标签包
// content，本文件修剩下这一种：帧粒度本身。
//
// 处理是站点下发前的无损重构：把连续的「纯 reasoning 增量帧」缓冲合并成
// ≥阈值的大帧再发。同样的字节、更少的帧；计费与 usage 解析走原始帧
// （OaiStreamHandler 的 processTokenData / observeStreamChoices 在过滤层
// 之前读原始帧），完全不受影响。
//
// 设计约束：
//   - 默认开启（无损），ChannelSettings.DisableReasoningCoalesce 按渠道豁免。
//   - 只缓冲「纯 reasoning 增量帧」：带 role / content / tool_calls /
//     finish_reason / usage / error / 多 choice 的帧先冲刷缓冲再原样下发，
//     全程保序。
//   - 阈值冲刷 + 终态冲刷（流正常结束、异常中断、HandleFinalResponse 前）。
//   - 与逐帧状态机改写器互斥：ThinkTagToReasoning / ReasoningAsContent /
//     ThinkingToContent 任一开启时整路旁路——那些状态机必须看到每一帧。
//   - 信封复用：冲刷帧 = 最近一个被缓冲帧的原始字节仅改写 reasoning 字段，
//     同流内 id/created/model 等字段逐字节存活。

const reasoningCoalesceThreshold = 256 // 字节；逐词帧 ~8-15B，约 20-40 词合并为一帧

// reasoningCoalesceEligible 判定本请求是否走合并层。
func reasoningCoalesceEligible(info *relaycommon.RelayInfo) bool {
	if info == nil || info.ChannelMeta == nil {
		return false
	}
	if info.ChannelSetting.DisableReasoningCoalesce {
		return false
	}
	// 逐帧状态机改写器依赖看到每一帧，与缓冲互斥
	if info.ChannelSetting.ThinkTagToReasoning ||
		info.ChannelSetting.ReasoningAsContent ||
		info.ChannelSetting.ThinkingToContent {
		return false
	}
	return true
}

// isEmptyJSONPlaceholder 判定一个 JSON 值是否为空占位（null / "" / [] / {}）。
// 逐词上游在每个 delta 帧里携带全量键的空占位（#118 实测帧形：delta 同时带
// function_call:null + refusal:"" + tool_calls:[] + extra_fields:null），
// 占位不是载荷，不得作为旁路合并的理由。
func isEmptyJSONPlaceholder(v gjson.Result) bool {
	switch v.Type {
	case gjson.Null:
		return true
	case gjson.String:
		return v.Str == ""
	case gjson.JSON:
		switch strings.TrimSpace(v.Raw) {
		case "[]", "{}", "null":
			return true
		}
		return false
	default:
		// False / Number 等实义标量按载荷处理（保守拒绝）
		return false
	}
}

// classifyReasoningFrame 判定一个 SSE 帧是否「纯 reasoning 增量帧」，并取出
// 增量文本（GetReasoningContent 同款胜者语义：reasoning_content 非空优先，
// 否则 reasoning）。pure=false 的帧一律冲刷后原样透传。
//
// 判定规则：除 reasoning 载荷键外，其余 delta 键必须是空占位或 role——
// role 恒为帧形标记（生产上游每帧重复 "role":"assistant"），空占位不带
// 载荷；任何实义载荷（content 文本、tool_calls 元素、数字/布尔标量）都
// 意味着这帧还承载了思考以外的东西。
func classifyReasoningFrame(data string) (reasoning string, pure bool) {
	choices := gjson.Get(data, "choices")
	if !choices.IsArray() {
		return "", false
	}
	arr := choices.Array()
	// 多 choice（n>1）不缓冲：帧与 choice 的对应关系会乱
	if len(arr) != 1 {
		return "", false
	}
	choice := arr[0]
	if fr := gjson.Get(choice.Raw, "finish_reason"); fr.Exists() && fr.Type != gjson.Null {
		return "", false
	}
	if u := gjson.Get(data, "usage"); u.Exists() && u.Type != gjson.Null {
		return "", false
	}
	if e := gjson.Get(data, "error"); e.Exists() && e.Type != gjson.Null {
		return "", false
	}
	delta := gjson.Get(choice.Raw, "delta")
	if !delta.Exists() || !delta.IsObject() {
		return "", false
	}
	for name, val := range delta.Map() {
		if name == "reasoning_content" || name == "reasoning" || name == "role" {
			continue
		}
		if !isEmptyJSONPlaceholder(val) {
			return "", false
		}
	}
	rc := gjson.Get(delta.Raw, "reasoning_content")
	if rc.Type == gjson.String && rc.Str != "" {
		return rc.Str, true
	}
	rs := gjson.Get(delta.Raw, "reasoning")
	if rs.Type == gjson.String && rs.Str != "" {
		return rs.Str, true
	}
	// 纯空壳（如 reasoning_content:null / ""）：无载荷可缓冲
	return "", false
}

// coalesceReasoningStreamFrame 是 HandleStreamFormat 的前置帧过滤器，返回
// 本轮应写出的帧序列（0..2 帧）。0 帧 = 帧已缓冲（吞掉，等后续冲刷）。
func coalesceReasoningStreamFrame(info *relaycommon.RelayInfo, data string) []string {
	if !reasoningCoalesceEligible(info) {
		return []string{data}
	}
	if !strings.Contains(data, "reasoning") {
		// 快速旁路：帧里没有 reasoning 字样，冲刷缓冲后原样下发
		return flushFirst(info, data)
	}
	reasoning, pure := classifyReasoningFrame(data)
	if !pure || reasoning == "" {
		return flushFirst(info, data)
	}
	st := &info.ReasoningCoalesce
	if !st.SawPure {
		// 本尝试首个纯 reasoning 帧立即透传：保住客户端 TTFT 观感
		st.SawPure = true
		return []string{data}
	}
	st.Pending += reasoning
	st.LastFrame = data
	st.Held = true
	if len(st.Pending) >= reasoningCoalesceThreshold {
		if frame := takeReasoningFlushFrame(info); frame != "" {
			return []string{frame}
		}
		return nil
	}
	return nil
}

// flushFirst 冲刷缓冲（如有）后返回 data 本身，保证 reasoning 先于其它
// 载荷到达客户端。
func flushFirst(info *relaycommon.RelayInfo, data string) []string {
	if frame := takeReasoningFlushFrame(info); frame != "" {
		return []string{frame, data}
	}
	return []string{data}
}

// takeReasoningFlushFrame 取出缓冲的合并帧文本并复位状态；无缓冲返回 ""。
// 复用 LastFrame 的原始信封，仅把 reasoning 字段改写为累计文本；信封改写
// 失败时退化为透传 LastFrame（丢失的只是更早缓冲的增量，不产生重复文本）。
func takeReasoningFlushFrame(info *relaycommon.RelayInfo) string {
	st := &info.ReasoningCoalesce
	if !st.Held {
		return ""
	}
	pending, lastFrame := st.Pending, st.LastFrame
	st.Reset()
	if pending == "" || lastFrame == "" {
		return ""
	}
	if frame := rewriteHeldFrameReasoning(lastFrame, pending); frame != "" {
		return frame
	}
	return lastFrame
}

// rewriteHeldFrameReasoning 把 held 帧的 reasoning 字段整体替换为 merged。
// reasoning_content 与 reasoning 双键并存时统一收敛到 reasoning_content
// （DTO 胜者语义的读取字段），删除 reasoning 键避免客户端双读重复。
func rewriteHeldFrameReasoning(data, merged string) string {
	var top orderedMembers
	if err := json.Unmarshal([]byte(data), &top); err != nil {
		return ""
	}
	for i := range top {
		if top[i].key != "choices" {
			continue
		}
		var choices orderedChoices
		if err := json.Unmarshal(top[i].raw, &choices); err != nil {
			return ""
		}
		if len(choices) != 1 {
			return ""
		}
		var choice orderedMembers
		if err := json.Unmarshal(choices[0], &choice); err != nil {
			return ""
		}
		changed := false
		for j := range choice {
			if choice[j].key != "delta" {
				continue
			}
			var delta orderedMembers
			if err := json.Unmarshal(choice[j].raw, &delta); err != nil {
				return ""
			}
			out := make(orderedMembers, 0, len(delta)+1)
			hasRC := false
			for _, m := range delta {
				switch m.key {
				case "reasoning_content":
					out = append(out, kvPair{key: "reasoning_content", raw: rawJSONString(merged)})
					hasRC = true
					changed = true
				case "reasoning":
					// 载荷并入 reasoning_content，丢掉镜像键
					changed = true
				default:
					out = append(out, m)
				}
			}
			if !hasRC {
				// held 帧只带了 reasoning 镜像键：合并文本补写进 reasoning_content
				out = append(out, kvPair{key: "reasoning_content", raw: rawJSONString(merged)})
				changed = true
			}
			if !changed {
				return ""
			}
			choice[j].raw = out.encode()
		}
		if !changed {
			return ""
		}
		choices[0] = choice.encode()
		b, err := choices.encode()
		if err != nil {
			return ""
		}
		top[i].raw = b
		return string(top.encode())
	}
	return ""
}

// writeCoalescedFlush 把缓冲的合并帧按当前 relay 格式直接写出（绕过过滤器，
// 否则冲刷帧会被再次缓冲）。流式收尾（终帧前 / HandleFinalResponse 前 /
// 断流兜底）各调用一次；无缓冲时为空操作。
func writeCoalescedFlush(c *gin.Context, info *relaycommon.RelayInfo, forceFormat, thinkToContent bool) error {
	frame := takeReasoningFlushFrame(info)
	if frame == "" {
		return nil
	}
	return writeStreamFrameDirect(c, info, frame, forceFormat, thinkToContent)
}
