package controller

import (
	"fmt"
	"time"

	"github.com/dengyie/apihub/loadbalancer"
	"github.com/dengyie/apihub/logger"
	"github.com/dengyie/apihub/model"
	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relaykit/types"
	"github.com/dengyie/apihub/service"
	"github.com/gin-gonic/gin"
)

func processChannelError(c *gin.Context, channelError types.ChannelError, err *types.NewAPIError, relayInfo *relaycommon.RelayInfo) {
	service.ProcessChannelError(c, channelError, err, relayInfo)
}

// logBreakerAlert 记录一次「判定需要熔断」的上游失败。detail 只描述成因，
// 结论由熔断状态机是否真正动作决定：生效时打 WARN 并追加 effective；渠道
// 豁免（breaker_exempt）或策略未加载时 TripBreaker* 是空操作，降级为 INFO
// 并写明未实际冷却。
//
// 不能无条件打「已立即熔断」：豁免是刻意配置（本机 CPA、Codex 代理池这类
// 不可失去的通道），生产日志里它们每次失败都打「已立即熔断」，实际却从未
// 冷却过——排障会照着不存在的隔离窗口解释流量（2026-11 实测：#152/#154
// 在 20 分钟内出现 30 次「已立即熔断」，而两者始终照常接流量）。
func logBreakerAlert(c *gin.Context, channelID int, detail, effective string) {
	switch {
	case !loadbalancer.Enabled():
		logger.LogInfo(c, detail+"，熔断未启用（策略未加载），本次失败仅记录")
	case loadbalancer.IsBreakerExempt(channelID):
		logger.LogInfo(c, detail+"，渠道豁免熔断（breaker_exempt），不摘出可用集")
	default:
		logger.LogWarn(c, detail+"，"+effective)
	}
}

// applyExemptChannelDegrade 让 breaker_exempt 渠道的确定性故障在选路上有代价。
//
// 豁免渠道没有别的代价通道：TripBreaker* 对它们是空操作，IsAvailable 跳过熔断，
// End 的阈值熔断也被跳过，于是「这条渠道已经确定坏了」对选路完全不可见，流量
// 每次都回到同一条坏渠道（2026-11-12 生产实测：#152 累计 59 次 500、#278 累计
// 55 次 502、#154 累计 51 次失败，全部因豁免而始终排最前）。这里按熔断分类
// 记一个软降级窗口：确定性故障用常规冷却（默认 300s），限流用短避让（默认
// 30s）。软降级不摘出可用集——只剩它时照常使用，与「不可失去」的豁免语义一致。
//
// BreakerNone（参数不支持、无特征 5xx 等）刻意不动：这些要么是请求形状问题、
// 要么已由 End 的短窗口记录，不应升级成分钟级惩罚。
func applyExemptChannelDegrade(c *gin.Context, channel *model.Channel, modelName string, effect service.BreakerEffectType, newAPIError *types.NewAPIError) {
	if channel == nil || newAPIError == nil {
		return
	}
	// 与 tripModelScope 同源：per_model 关闭或错误本身是渠道级时，标签与实际
	// 写入的键都落回渠道级（空串），不能一个说模型级、一个写渠道级。
	effective := loadbalancer.EffectiveModelName(channel.Id, modelName, newAPIError)
	scopes := []string{effective}
	scopes = append(scopes, loadbalancer.SiblingModelsFromMapping(channel.GetModelMapping(), modelName)...)

	tracker := loadbalancer.GlobalTracker()
	for _, scope := range scopes {
		var window time.Duration
		switch effect {
		case service.BreakerRateLimit:
			window = tracker.DegradeChannelForRateLimit(channel.Id, scope)
		case service.BreakerQuota, service.BreakerPermission, service.BreakerRelay,
			service.BreakerRouting, service.BreakerModelUnavailable,
			service.BreakerEOL, service.BreakerStreamBroken, service.BreakerThinkingHistory,
			service.BreakerCurfew, service.BreakerResponsesAPINotSupported:
			window = tracker.DegradeChannelForFault(channel.Id, scope)
		default:
			return
		}
		if window <= 0 {
			continue
		}
		label := "全部模型"
		if scope != "" {
			label = scope
		}
		logger.LogWarn(c, fmt.Sprintf("渠道 #%d [%s] 豁免熔断（breaker_exempt），改为软降级 %s：仍可用但选路排最后（%s）",
			channel.Id, label, window, newAPIError.Error()))
	}
}

// applyUpstreamFailureEffects 在一轮尝试失败后，把这次失效记到正确的熔断
// 作用域上。熔断（本函数）与自动禁用（processChannelError）在此分流：熔断
// 可逆、按 (渠道, 模型) 粒度快速绕开；账号级整体下线由 AutomaticDisable
// 兜底，两者互不替代。lbAttempt 是本次尝试的句柄（必传）：常规冷却路径
// 依据它的 failureTripped 标记与 End 的阈值/半开熔断去重，一次失败至多
// 一次 tripCount 递增。
//
// 分支必须是严格的 if/else-if 链（或收敛的 switch），一次失败至多一次 tripBreaker。
// 原因有二：
//
//  1. 错误分类器之间存在有意的重叠：IsUpstreamRelayError 会把
//     permission / thinking 类也认下（中转站常把它们包成自己的报错格式），
//     宵禁的 403 同时命中 IsUpstreamPermissionError，上游 new-api 家族包装
//     的额度错误（type=new_api_error）也会命中 relay 分类。若用并列 if，
//     同一次失败会触发多次 tripBreaker，tripCount 一次涨 2-3——递增退避在
//     首次失败就跳到 2-3 倍冷却、cap 提前触顶，故障渠道退出轮转的速度
//     比设计快一倍以上（2026-10 review 实证：一次权限错误 tripCount=3）。
//  2. 并列结构无法对「一次失败恰好一次 trip」写回归测试，重叠面一旦变化
//     只能在生产上发现。
//
// 链序 = 特异性序：处置最特殊的在前（宵禁要绝对到期时间、429 要短避让、
// EOL/quota 是确定性失效），兜底的 relay 分类在最后。
//
// 每条熔断日志都必须打出作用域：按模型熔断之后，只打渠道 id 的日志无法
// 归因「到底是哪个模型把它熔了」。scopeOf 与 tracker.scopeKey 同源
// （loadbalancer.EffectiveModelName），per_model 关闭时标签自动落回渠道级。
func applyUpstreamFailureEffects(c *gin.Context, channel *model.Channel, relayInfo *relaycommon.RelayInfo, lbAttempt *loadbalancer.RequestHandle, newAPIError *types.NewAPIError) {
	if newAPIError == nil {
		return
	}
	modelName := relayInfo.OriginModelName
	scopeOf := func(err *types.NewAPIError) string {
		return loadbalancer.EffectiveModelName(channel.Id, modelName, err)
	}
	scopeLabel := func(model string) string {
		if model == "" {
			return "全部模型"
		}
		return model
	}
	// tripModelScope 熔断「实际作用域为模型级」的那一把键，并连带熔掉
	// 映射到同一上游模型的兄弟客户端模型。
	//
	// until 传零值表示走常规冷却（相对 openedAt 递增退避），非零走定时
	// 熔断（绝对 blockedUntil，如宵禁）——两者语义不同，不能互相替代。
	//
	// 返回实际生效的模型名（空串=渠道级）。开关关闭时 effective 为空，
	// 键已折叠成渠道级，兄弟一并被渠道级熔断覆盖，这里直接返回空即可。
	tripModelScope := func(err *types.NewAPIError, until time.Time) string {
		effective := scopeOf(err)
		trip := func(m string) {
			if until.IsZero() {
				loadbalancer.GlobalTracker().TripBreaker(channel.Id, m)
			} else {
				loadbalancer.GlobalTracker().TripBreakerUntil(channel.Id, m, until)
			}
		}
		// 常规冷却路径尊重句柄标记：End 的阈值/半开路径已为本次失败熔断过时，
		// 这里只留日志不重复递增 tripCount（一次失败至多一次熔断，见
		// RequestHandle.failureTripped）。blockedUntil 路径（宵禁）不查标记，
		// 它改写的是绝对到期时间，必须无条件执行。
		if until.IsZero() && lbAttempt.FailureTripped() {
			logger.LogInfo(c, fmt.Sprintf("渠道 #%d [%s] 本次失败已由连续失败阈值触发熔断，显式分类不重复递增退避", channel.Id, scopeLabel(effective)))
		} else {
			trip(effective)
			lbAttempt.MarkFailureTripped()
		}
		if effective == "" {
			return ""
		}
		for _, sibling := range loadbalancer.SiblingModelsFromMapping(channel.GetModelMapping(), modelName) {
			trip(sibling)
			logBreakerAlert(c, channel.Id,
				fmt.Sprintf("渠道 #%d [%s] 与 %s 映射到同一上游模型", channel.Id, scopeLabel(sibling), modelName),
				"一并熔断")
		}
		return effective
	}

	// 智能负载：基于收敛的错误分类执行严格单次熔断
	effect, curfewUntil := service.ClassifyBreakerEffect(newAPIError)
	// 不透明 400 水合失败（中转站抹掉 code、只留 "bad response status code 400"）：
	// 请求本身带跨账号密文历史，根因不在渠道，降为不计故障，避免健康渠道被软降级。
	if effect == service.BreakerRelay && service.IsContextualReasoningHydration(c, newAPIError) {
		effect = service.BreakerNone
		curfewUntil = time.Time{}
	}
	// 豁免渠道没有熔断出口，确定性故障改记软降级（仍可用、选路排最后）。
	// 与下面的 switch 并存而非替代：TripBreaker* 对豁免渠道是空操作，
	// 这里补上它们在选路层唯一可见的代价。
	if loadbalancer.IsBreakerExempt(channel.Id) {
		applyExemptChannelDegrade(c, channel, modelName, effect, newAPIError)
	}
	switch effect {
	case service.BreakerRateLimit:
		effective := scopeOf(newAPIError)
		loadbalancer.GlobalTracker().TripBreakerForRateLimit(channel.Id, effective)
		logBreakerAlert(c, channel.Id,
			fmt.Sprintf("渠道 #%d [%s] 上游限流/并发超限 (429/RPM/Concurrency)", channel.Id, scopeLabel(effective)),
			"已设置短期避让冷却: "+newAPIError.Error())
		for _, sibling := range loadbalancer.SiblingModelsFromMapping(channel.GetModelMapping(), modelName) {
			loadbalancer.GlobalTracker().TripBreakerForRateLimit(channel.Id, sibling)
			logBreakerAlert(c, channel.Id,
				fmt.Sprintf("渠道 #%d [%s] 与 %s 映射到同一上游模型", channel.Id, scopeLabel(sibling), modelName),
				"一并避让")
		}
	case service.BreakerCurfew:
		tripped := tripModelScope(newAPIError, curfewUntil)
		logBreakerAlert(c, channel.Id,
			fmt.Sprintf("渠道 #%d [%s] 宵禁中", channel.Id, scopeLabel(tripped)),
			"已熔断到 "+curfewUntil.Format("15:04"))
	case service.BreakerEOL:
		tripped := tripModelScope(newAPIError, time.Time{})
		logBreakerAlert(c, channel.Id,
			fmt.Sprintf("渠道 #%d [%s] 上游模型已 EOL/下线 (410)", channel.Id, scopeLabel(tripped)),
			"已立即熔断")
	case service.BreakerStreamBroken:
		tripped := tripModelScope(newAPIError, time.Time{})
		logBreakerAlert(c, channel.Id,
			fmt.Sprintf("渠道 #%d [%s] 上游流传输中断 (%s)", channel.Id, scopeLabel(tripped), newAPIError.Error()),
			"已立即熔断并触发重试")
	case service.BreakerQuota:
		tripped := tripModelScope(newAPIError, time.Time{})
		logBreakerAlert(c, channel.Id,
			fmt.Sprintf("渠道 #%d [%s] 上游额度已耗尽", channel.Id, scopeLabel(tripped)),
			"已立即熔断: "+newAPIError.Error())
	case service.BreakerRouting:
		tripped := tripModelScope(newAPIError, time.Time{})
		logBreakerAlert(c, channel.Id,
			fmt.Sprintf("渠道 #%d [%s] 上游会话路由失败 (缺失 x-opencode-session)", channel.Id, scopeLabel(tripped)),
			"已立即熔断该渠道: "+newAPIError.Error())
	case service.BreakerThinkingHistory:
		tripped := tripModelScope(newAPIError, time.Time{})
		logBreakerAlert(c, channel.Id,
			fmt.Sprintf("渠道 #%d [%s] 上游思考模式历史消息不兼容 (reasoning_content must be passed back)", channel.Id, scopeLabel(tripped)),
			"已立即熔断: "+newAPIError.Error())
	case service.BreakerModelUnavailable:
		tripped := tripModelScope(newAPIError, time.Time{})
		logBreakerAlert(c, channel.Id,
			fmt.Sprintf("渠道 #%d [%s] 上游模型不可用/已禁用", channel.Id, scopeLabel(tripped)),
			"已立即熔断: "+newAPIError.Error())
	case service.BreakerResponsesAPINotSupported:
		// Responses API 不被该上游支持：模型在、协议不在。按 (渠道, 模型) 冷却，
		// 其余模型照常；既非「模型不存在」（不得进自动禁用），也非渠道故障。
		tripped := tripModelScope(newAPIError, time.Time{})
		logBreakerAlert(c, channel.Id,
			fmt.Sprintf("渠道 #%d [%s] 上游不支持 Responses API (api_not_supported)", channel.Id, scopeLabel(tripped)),
			"已按模型冷却: "+newAPIError.Error())
	case service.BreakerPermission:
		tripped := tripModelScope(newAPIError, time.Time{})
		logBreakerAlert(c, channel.Id,
			fmt.Sprintf("渠道 #%d [%s] 上游权限受限/分组无权访问/模型不支持", channel.Id, scopeLabel(tripped)),
			"已立即熔断: "+newAPIError.Error())
	case service.BreakerRelay:
		tripped := tripModelScope(newAPIError, time.Time{})
		logBreakerAlert(c, channel.Id,
			fmt.Sprintf("渠道 #%d [%s] 上游中继代理异常 (bad response status code / 渠道出错)", channel.Id, scopeLabel(tripped)),
			"已立即熔断: "+newAPIError.Error())
	case service.BreakerNone:
	}
}
