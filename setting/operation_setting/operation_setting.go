package operation_setting

import (
	"strings"
	"sync/atomic"
)

var DemoSiteEnabled = false
var SelfUseModeEnabled = false

var defaultAutomaticDisableKeywords = []string{
	"Your credit balance is too low",
	"This organization has been disabled.",
	"You exceeded your current quota",
	"Permission denied",
	"The security token included in the request is invalid",
	"Operation not allowed",
	"Your account is not authorized",
	"credit insufficient balance",
	"insufficient_user_quota",
	"insufficient_quota",
	"insufficient balance",
	"user quota not enough",
	"quota exhausted",
	"balance is not enough",
	"quota_exceeded",
	"user_quota_exhausted",
	"无权访问",
	// 刻意不收裸词「当前分组」（2026-11-28 删除）：上游 new-api 的分组报文几乎都是临时
	// 状态——「上游负载已饱和」「正在维护」「本时段不可调用」「下对于模型 X 无可用渠道」，
	// 渠道 #436 就因「当前分组正在维护」被不可逆禁用。分组无权访问已由「无权访问」与
	// user_group_no_permission 覆盖。
	"not supported by tokenplan",
	"is not supported by tokenplan",
	"user_group_no_permission",
	// 以下 5 条是 2026-10-01 从生产文件日志里挖出来的：这些措辞在真实上游
	// 返回里反复出现，却与上面任何一条都不构成子串匹配，所以渠道一直被熔断
	// 重试、却永远不会被自动下线 —— 白白消耗重试预算和上游配额。
	// 选取标准是「**必须人工介入才能恢复**」，而不是「看起来像失败」：
	// 限流、并发超限、临时不可用一律不收，那是熔断该干的活。
	"预扣费额度失败",                   // 上游账号余额不足以预扣（实测 200+ 次）
	"token quota is not enough", // 同上，另一家上游的英文措辞
	"用户额度不足",                    // 同上，第三家上游的中文措辞
	"令牌因分组倍率上调已停用",              // 令牌被上游停用，需人工去 API 密钥页重新启用
	"Invalid token",             // 上游密钥失效。注意它与已有的 "The security token
	// included in the request is invalid" 是两回事：后者是完整句、后者是裸
	// 短语，子串匹配互相覆盖不到，两条都得留着。
	// ↓↓ v29.13 补的第二批 ↓↓
	//
	// 上一批是用正则从日志里捞的，只捞到了「上游 XX 失败」那类带前缀的报文，
	// 漏掉了**裸报文**形式。改成把每条真实报文逐条喂给词表做子串判定后，
	// 找出下面 4 条同样长期漏网、同样必须人工才能恢复的措辞。
	"API key 额度已用完",                    // 上游 API key 的额度打光（实测 47 次）
	"Insufficient account balance",     // 账号余额不足。注意与已有的 "insufficient balance" 不是同一条：中间隔了 "account"，子串匹配覆盖不到
	"token plan entitlement exhausted", // 上游订阅额度耗尽（实测 24 次；这条正是把 space-bunny 打到 503 的元凶）
	"user quota is not enough",         // 与已有的 "user quota not enough" 差一个 "is"，子串同样覆盖不到
	// ⚠️ 刻意**不收** "quota exceeded"：历史上有 90 次，但它既可能是账号超额、
	// 也可能是单请求 max_tokens 超限，两种含义的后果完全相反，而它只出现在
	// 已轮转的最老日志里、当前所有会话均未复现。宁可漏判也不误杀健康渠道。
	//
	// 同样刻意不收（会自愈，是熔断的活）："rpm exhausted"、"您已达到并发请求数限制"、
	// "您已达到请求数限制：N分钟内最多请求N次"、"免费模型…每天限 N 次…明天重置"、
	// "Upstream service temporarily unavailable"、"No available channel for model …"
	//（后者是模型级，渠道本身还服务其它模型）、"bad response status code"（语义不唯一）。
}

// automaticDisableKeywords 是自动禁用词表的不可变快照。
//
// 管理员保存选项 / 选项定时重载会调用 AutomaticDisableKeywordsFromString，而每次
// 上游失败都会在请求热路径读词表；原先直接重置再逐条 append 同一个全局切片，
// 并发读会看到空表或半截表（-race 报 DATA RACE，重载期间可漏判死 key）。改为
// 写侧构造新切片后整体原子替换，读侧拿到的永远是完整的一版，与
// status_code_ranges.go 的 statusCodeRulesPtr 同一模式。快照切片构造后不得再修改。
var automaticDisableKeywords atomic.Pointer[[]string]

func init() {
	keywords := defaultAutomaticDisableKeywords
	automaticDisableKeywords.Store(&keywords)
}

// AutomaticDisableKeywords 返回当前词表快照，调用方只读不改。
func AutomaticDisableKeywords() []string {
	return *automaticDisableKeywords.Load()
}

func AutomaticDisableKeywordsToString() string {
	return strings.Join(AutomaticDisableKeywords(), "\n")
}

func AutomaticDisableKeywordsFromString(s string) {
	keywords := []string{}
	for k := range strings.SplitSeq(s, "\n") {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "" {
			keywords = append(keywords, k)
		}
	}
	automaticDisableKeywords.Store(&keywords)
}
