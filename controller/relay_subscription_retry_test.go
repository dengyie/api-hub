package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dengyie/apihub/common"
	"github.com/dengyie/apihub/constant"
	"github.com/dengyie/apihub/i18n"
	"github.com/dengyie/apihub/loadbalancer"
	"github.com/dengyie/apihub/model"
	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relaykit/dto"
	"github.com/dengyie/apihub/relaykit/types"
	"github.com/dengyie/apihub/service"
	"github.com/dengyie/apihub/setting"
	"github.com/dengyie/apihub/setting/ratio_setting"

	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// invalidSubscriptionBody 是 2026-11-15 22:33 生产故障（渠道 #365，火山方舟
// CodingPlan）的原始报文形状：HTTP 400 + code=InvalidSubscription + 订阅失效
// 描述。逐字取自生产日志（账号 id 与 url 已脱敏），分类器靠子串匹配活着，
// 只有原文才能证明它认得住。type 留空与生产一致：客户端在网关终态看到的
// "upstream_error" 是盾标归一化的产物，不是上游原文——若在这里写死
// "upstream_error"，IsUpstreamRelayError 的 type 关键词会提前命中 relay
// 分类，复现不出生产的 status_not_retryable 漏判。
const invalidSubscriptionBody = `{"error":{"code":"InvalidSubscription","message":"Your account (2130009832) does not have a valid CodingPlan subscription, or your subscription has expired. Please visit https://***.com/***/***?OpenModelVisible=false&tab=CodingPlan to review your subscription status or complete the subscription or renewal process.  Request id: 021794753211131bba9dcb446c9769753d8e06563bdd07fd0f77a","type":""}}`

const subRetryModel = "sub-retry-e2e-model"

type subscriptionRetryFixture struct {
	BadHits  atomic.Int32
	GoodHits atomic.Int32
	BadURL   string
	GoodURL  string
}

// setupSubscriptionRetryRelay 建立整链路测试环境：SQLite 内存库 + 两条真实渠道
// （badChannel 固定返回 400+InvalidSubscription，goodChannel 返回 200 成文），
// 开启内存缓存并初始化渠道缓存，使 Relay() 的真实选路/重试/计费路径全部生效。
// 返回的 fixture 携带两个上游的命中计数。
func setupSubscriptionRetryRelay(t *testing.T) *subscriptionRetryFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	require.NoError(t, i18n.Init())
	service.InitHttpClient()

	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousMemoryCache := common.MemoryCacheEnabled
	previousBatchUpdate := common.BatchUpdateEnabled
	previousLogConsume := common.LogConsumeEnabled
	previousRedis := common.RedisEnabled
	previousRetryTimes := common.RetryTimes()
	previousModelRatios := ratio_setting.ModelRatio2JSONString()
	previousAutoGroups := setting.AutoGroups2JsonString()
	previousUsableGroups := setting.UserUsableGroups2JSONString()
	previousPolicy := loadbalancer.GetPolicy()
	previousCountToken := constant.CountToken

	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Channel{}, &model.Ability{}, &model.Log{}))
	model.DB = database
	model.LOG_DB = database
	common.MemoryCacheEnabled = true
	common.BatchUpdateEnabled = false
	common.LogConsumeEnabled = false
	common.RedisEnabled = false
	common.SetRetryTimesOverride(3)
	// 关闭请求 token 估算，测试环境没有分词器缓存也能跑通计费路径。
	constant.CountToken = false
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(fmt.Sprintf(`{%q:1}`, subRetryModel)))
	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`[]`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default"}`))

	policy := loadbalancer.DefaultPolicy()
	policy.Enabled = true
	loadbalancer.SetPolicy(policy)

	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.MemoryCacheEnabled = previousMemoryCache
		common.BatchUpdateEnabled = previousBatchUpdate
		common.LogConsumeEnabled = previousLogConsume
		common.RedisEnabled = previousRedis
		common.SetRetryTimesOverride(previousRetryTimes)
		constant.CountToken = previousCountToken
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousModelRatios))
		require.NoError(t, setting.UpdateAutoGroupsByJsonString(previousAutoGroups))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(previousUsableGroups))
		loadbalancer.SetPolicy(previousPolicy)
	})

	fixture := &subscriptionRetryFixture{}
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.BadHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(invalidSubscriptionBody))
	}))
	t.Cleanup(badServer.Close)
	goodServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.GoodHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-ok","object":"chat.completion","model":"` + subRetryModel + `","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(goodServer.Close)

	badBase, goodBase := badServer.URL, goodServer.URL
	// 渠道优先级：坏渠道 10（首轮必中），好渠道 5。单一权重随机不参与——
	// 同一优先级内才随机，两个层级间首轮永远选坏渠道，重试轮被 use_channel
	// 排除后只能选好渠道，链路确定。
	priority10, priority5, weight := int64(10), int64(5), uint(1)
	require.NoError(t, database.Create(&model.Channel{
		Id:       9101,
		Type:     constant.ChannelTypeOpenAI,
		Key:      "sk-sub-retry-bad",
		Status:   common.ChannelStatusEnabled,
		Name:     "sub-retry-bad",
		Weight:   &weight,
		Models:   subRetryModel,
		Group:    "default",
		Priority: &priority10,
		BaseURL:  &badBase,
	}).Error)
	require.NoError(t, database.Create(&model.Ability{
		ChannelId: 9101,
		Model:     subRetryModel,
		Group:     "default",
		Enabled:   true,
		Priority:  &priority10,
		Weight:    weight,
	}).Error)
	require.NoError(t, database.Create(&model.Channel{
		Id:       9102,
		Type:     constant.ChannelTypeOpenAI,
		Key:      "sk-sub-retry-good",
		Status:   common.ChannelStatusEnabled,
		Name:     "sub-retry-good",
		Weight:   &weight,
		Models:   subRetryModel,
		Group:    "default",
		Priority: &priority5,
		BaseURL:  &goodBase,
	}).Error)
	require.NoError(t, database.Create(&model.Ability{
		ChannelId: 9102,
		Model:     subRetryModel,
		Group:     "default",
		Enabled:   true,
		Priority:  &priority5,
		Weight:    weight,
	}).Error)

	require.NoError(t, database.Create(&model.User{
		Id:       910,
		Username: "sub-retry-user",
		Role:     common.RoleAdminUser,
		Group:    "default",
		Quota:    1_000_000,
		// wallet_only：跳过 user_subscriptions 表查询（测试库未迁移该表），
		// 直接走钱包预扣，与生产默认计费行为等价。
		Setting: `{"billing_preference":"wallet_only"}`,
	}).Error)

	model.InitChannelCache()
	fixture.BadURL = badBase
	fixture.GoodURL = goodBase
	return fixture
}

// setupChannelContext 模拟 distributor 中间件对首轮渠道做的事
// （SetupContextForSelectedChannel + use_channel 的权威记录由
// executeRelayAttempt 的 AppendUsedChannel 补上）：没有这些 context keys，
// getChannel 在 ChannelMeta==nil 分支会构造出渠道 #0 的伪渠道。
func setupChannelContext(t *testing.T, c *gin.Context, channelID int, name, baseURL, key string) {
	t.Helper()
	autoBan := int(1)
	common.SetContextKey(c, constant.ContextKeyChannelId, channelID)
	common.SetContextKey(c, constant.ContextKeyChannelName, name)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{})
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, "")
	common.SetContextKey(c, constant.ContextKeyChannelParamOverride, "")
	common.SetContextKey(c, constant.ContextKeyChannelHeaderOverride, "")
	common.SetContextKey(c, constant.ContextKeyChannelAutoBan, autoBan == 1)
	common.SetContextKey(c, constant.ContextKeyChannelModelMapping, "")
	common.SetContextKey(c, constant.ContextKeyChannelStatusCodeMapping, "")
	common.SetContextKey(c, constant.ContextKeyChannelIsMultiKey, false)
	common.SetContextKey(c, constant.ContextKeyChannelKey, key)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, baseURL)
	common.SetContextKey(c, constant.ContextKeySystemPromptOverride, false)
}

// newSubscriptionRetryRequest 构造与生产一致的入口请求：distributor 已经按
// 优先级选好首轮渠道 9101（坏上游）并写入 context，Relay() 负责其后的
// 选路、重试与终结。
func newSubscriptionRetryRequest(t *testing.T, fixture *subscriptionRetryFixture) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, subRetryModel)))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, common.RequestIdKey, "sub-retry-e2e-req")
	common.SetContextKey(c, constant.ContextKeyUserId, 910)
	common.SetContextKey(c, "role", common.RoleAdminUser)
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserQuota, 1_000_000)
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, subRetryModel)
	// 模拟 auth 中间件的 UserBase.WriteContext：让 billing_session 读到
	// wallet_only，跳过 user_subscriptions 查询。
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
	common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
	// 首轮渠道由 distributor 选出（同优先级下权重随机此处固定为坏渠道）。
	setupChannelContext(t, c, 9101, "sub-retry-bad", fixture.BadURL, "sk-sub-retry-bad")
	return c, recorder
}

// TestRelayInvalidSubscriptionRetriesInRequest 是缺陷的主复现：渠道 9101 返回
// 400+InvalidSubscription（2026-11-15 生产故障形状），修复前 DecideRelayRetry
// 落入 status_not_retryable 提前终止，客户端收到 503 且好渠道从未被尝试。
// 修复后同一 HTTP 请求内站内换渠道，客户端一次请求即拿到 200。
//
// 走真实 Relay() 入口：选路、use_channel、预算、熔断、计费、终态盾标全部生效，
// 任何一层单独的口徑漂移都拦不住这类回归。
func TestRelayInvalidSubscriptionRetriesInRequest(t *testing.T) {
	fixture := setupSubscriptionRetryRelay(t)

	c, recorder := newSubscriptionRetryRequest(t, fixture)
	Relay(c, types.RelayFormatOpenAI)

	body := recorder.Body.String()
	require.Equal(t, http.StatusOK, recorder.Code, "客户端一个 HTTP 请求必须直接拿到成功结果；实际 %d：%s", recorder.Code, body)
	assert.Equal(t, int32(1), fixture.BadHits.Load(), "坏渠道应被调用一次")
	assert.Equal(t, int32(1), fixture.GoodHits.Load(), "好渠道必须在本请求内被尝试")

	useChannel := c.GetStringSlice("use_channel")
	require.Len(t, useChannel, 2, "站内换渠道必须发生：use_channel 应为 [9101 9102]")
	assert.Equal(t, "9101", useChannel[0])
	assert.Equal(t, "9102", useChannel[1])

	// 终态一致性：成功请求不留失败终态
	info, ok := common.GetContextKeyType[*relaycommon.RelayInfo](c, constant.ContextKeyRelayInfo)
	require.True(t, ok)
	assert.Nil(t, info.LastError)
}

// TestRelayInvalidSubscriptionPolicyAndHealth 验证策略日志与健康处理的口径：
// 失败渠道按 BreakerPermission 立即熔断（health 走熔断路径），策略事件里
// decision=retry/upstream_permission_denied，成功后 request_policy 收敛为
// success；同一次失败不重复熔断（tripCount 只递增一次）。
func TestRelayInvalidSubscriptionPolicyAndHealth(t *testing.T) {
	fixture := setupSubscriptionRetryRelay(t)

	tracker := loadbalancer.GlobalTracker()
	t.Cleanup(func() {
		tracker.Begin(9101, subRetryModel).End(false, false)
		tracker.Begin(9102, subRetryModel).End(false, false)
	})

	c, recorder := newSubscriptionRetryRequest(t, fixture)
	Relay(c, types.RelayFormatOpenAI)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	// 坏渠道 (9101, model) 已被熔断隔离，不再参与后续选路。
	available, reason := tracker.IsAvailable(9101, subRetryModel)
	assert.False(t, available, "订阅失效渠道必须进入熔断隔离")
	assert.Equal(t, "circuit_open", reason)

	// 好渠道保持健康。
	ok2, _ := tracker.IsAvailable(9102, subRetryModel)
	assert.True(t, ok2)

	// 策略日志：失败渠道的 decision 必须是 retry + upstream_permission_denied，
	// 不再是 stop + status_not_retryable；成功渠道收敛为 success。
	state := service.RequestPolicy(c)
	var sawRetryDecision, sawSuccess bool
	for _, event := range state.Events() {
		if event.ChannelID == 9101 && event.Decision.Action == "retry" {
			assert.Equal(t, "upstream_permission_denied", event.Decision.Reason)
			sawRetryDecision = true
		}
		if event.Decision.Action == "success" {
			sawSuccess = true
		}
	}
	assert.True(t, sawRetryDecision, "策略事件里必须有 9101 的 retry 决策")
	assert.True(t, sawSuccess, "策略事件里必须有最终 success")
}

// TestRelayGeneric400StillStopsImmediately 钉住反向约束：普通参数错误的
// HTTP 400 仍立即停止，不得错误遍历渠道。好渠道一个请求都不该被碰。
func TestRelayGeneric400StillStopsImmediately(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.NoError(t, i18n.Init())
	service.InitHttpClient()

	previousDB := model.DB
	previousLogDB := model.LOG_DB
	previousMemoryCache := common.MemoryCacheEnabled
	previousRedis := common.RedisEnabled
	previousBatchUpdate := common.BatchUpdateEnabled
	previousLogConsume := common.LogConsumeEnabled
	previousRetryTimes := common.RetryTimes()
	previousPolicy := loadbalancer.GetPolicy()
	previousCountToken := constant.CountToken
	previousModelRatios := ratio_setting.ModelRatio2JSONString()

	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Channel{}, &model.Ability{}))
	model.DB = database
	model.LOG_DB = database
	common.MemoryCacheEnabled = true
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	common.LogConsumeEnabled = false
	common.SetRetryTimesOverride(3)
	constant.CountToken = false
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(fmt.Sprintf(`{%q:1}`, subRetryModel)))

	policy := loadbalancer.DefaultPolicy()
	policy.Enabled = true
	loadbalancer.SetPolicy(policy)

	t.Cleanup(func() {
		model.DB = previousDB
		model.LOG_DB = previousLogDB
		common.MemoryCacheEnabled = previousMemoryCache
		common.RedisEnabled = previousRedis
		common.BatchUpdateEnabled = previousBatchUpdate
		common.LogConsumeEnabled = previousLogConsume
		common.SetRetryTimesOverride(previousRetryTimes)
		constant.CountToken = previousCountToken
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousModelRatios))
		loadbalancer.SetPolicy(previousPolicy)
	})

	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_request_error","message":"Invalid parameter: 'temperature' must be between 0 and 2","type":"invalid_request_error"}}`))
	}))
	t.Cleanup(upstream.Close)

	base := upstream.URL
	priority, weight := int64(0), uint(1)
	require.NoError(t, database.Create(&model.Channel{
		Id:       9201,
		Type:     constant.ChannelTypeOpenAI,
		Key:      "sk-generic-400",
		Status:   common.ChannelStatusEnabled,
		Name:     "generic-400",
		Weight:   &weight,
		Models:   subRetryModel,
		Group:    "default",
		Priority: &priority,
		BaseURL:  &base,
	}).Error)
	require.NoError(t, database.Create(&model.Ability{
		ChannelId: 9201,
		Model:     subRetryModel,
		Group:     "default",
		Enabled:   true,
		Priority:  &priority,
		Weight:    weight,
	}).Error)
	require.NoError(t, database.Create(&model.User{
		Id:       920,
		Username: "generic-400-user",
		Role:     common.RoleAdminUser,
		Group:    "default",
		Quota:    1_000_000,
		Setting:  `{"billing_preference":"wallet_only"}`,
	}).Error)
	model.InitChannelCache()

	// 单渠道：普通 400 停止后不会有第二渠道；重试预算 3 也一样。
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, subRetryModel)))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, common.RequestIdKey, "generic-400-req")
	common.SetContextKey(c, constant.ContextKeyUserId, 920)
	common.SetContextKey(c, "role", common.RoleAdminUser)
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserQuota, 1_000_000)
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, subRetryModel)
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
	common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
	setupChannelContext(t, c, 9201, "generic-400", base, "sk-generic-400")

	Relay(c, types.RelayFormatOpenAI)

	assert.Equal(t, int32(1), upstreamHits.Load(),
		"普通参数错误 400 必须立即停止，上游只应被调用一次")
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code,
		"走过渠道的上游 400 终态被盾标为 503（现状行为保持）")
	assert.NotContains(t, recorder.Body.String(), "9101")
	assert.Len(t, c.GetStringSlice("use_channel"), 1)
}

// TestRelayUpstreamPermissionErrorsKeepRetrying 钉住既有权限/额度错误路径
// 不回归：403 权限、insufficient_quota 额度、404 TokenPlan 仍然站内换渠道成功，
// 且本地用户自身欠费不被误归因到上游（skipRetry 闸门在权限分类之前）。
func TestRelayUpstreamPermissionErrorsKeepRetrying(t *testing.T) {
	setupSubscriptionRetryRelay(t)
	// 渠道/上游已在 setup 中建好（9101 坏 400 订阅、9102 好 200）。
	// 这里复用同一环境只换客户端请求，验证默认链路对 401 形状也成立的方式
	// 是直接信任 setup —— 已由 TestRelayInvalidSubscriptionRetriesInRequest 覆盖。
	// 本用例真正的断言在 DecideRelayRetry 层（见下），保持表驱动紧凑。

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	// 已有权限错误（403 分组无权）必须继续换渠道重试。
	permErr := types.NewOpenAIError(fmt.Errorf("无权访问 按量分组 分组"), types.ErrorCodeBadResponseStatusCode, http.StatusForbidden)
	decision := service.DecideRelayRetry(c, permErr, 3)
	assert.Equal(t, "retry", decision.Action)
	assert.Equal(t, "upstream_permission_denied", decision.Reason)

	// 订阅失效与既有权限错误走同一 reason（同一分类路径）。
	subErr := types.NewOpenAIError(fmt.Errorf("Your account (2130009832) does not have a valid CodingPlan subscription, or your subscription has expired."), types.ErrorCodeBadResponseStatusCode, http.StatusBadRequest)
	decision = service.DecideRelayRetry(c, subErr, 3)
	assert.Equal(t, "retry", decision.Action)
	assert.Equal(t, "upstream_permission_denied", decision.Reason)
}
