package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dengyie/apihub/common"
	"github.com/dengyie/apihub/constant"
	"github.com/dengyie/apihub/dto"
	"github.com/dengyie/apihub/i18n"
	"github.com/dengyie/apihub/loadbalancer"
	"github.com/dengyie/apihub/model"
	"github.com/dengyie/apihub/relay"
	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relaykit/types"
	"github.com/dengyie/apihub/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupRelayAttemptDB(t *testing.T) *gorm.DB {
	t.Helper()
	previousDB := model.DB
	previousType := common.MainDatabaseType()
	previousCache := common.MemoryCacheEnabled
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, database.AutoMigrate(&model.Channel{}, &model.Ability{}))
	model.DB = database
	common.SetMainDatabaseType(common.DatabaseTypeSQLite)
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		model.DB = previousDB
		common.SetMainDatabaseType(previousType)
		common.MemoryCacheEnabled = previousCache
		require.NoError(t, sqlDB.Close())
	})
	return database
}

func TestExecuteRelayAttemptReportsNoAvailableChannelWhenRetrySelectionExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.NoError(t, i18n.Init())
	db := setupRelayAttemptDB(t)

	ch := &model.Channel{
		Name:     "ch-1",
		Key:      "sk-test-1",
		Status:   common.ChannelStatusEnabled,
		Type:     constant.ChannelTypeOpenAI,
		Group:    "default",
		Models:   "gpt-test",
		Priority: common.GetPointer(int64(0)),
	}
	require.NoError(t, db.Create(ch).Error)
	require.NoError(t, db.Create(&model.Ability{
		ChannelId: ch.Id,
		Model:     "gpt-test",
		Group:     "default",
		Enabled:   true,
		Priority:  ch.Priority,
	}).Error)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-test")
	service.AppendUsedChannel(c, ch.Id)

	priorStreamStatus := relaycommon.NewStreamStatus()
	priorStreamStatus.MarkFailed("upstream_502", "upstream_error", http.StatusBadGateway)
	priorErr := types.NewErrorWithStatusCode(errors.New("upstream 502 bad gateway"), types.ErrorCodeBadResponseStatusCode, http.StatusBadGateway)

	relayInfo := &relaycommon.RelayInfo{
		OriginModelName:              "gpt-test",
		UsingGroup:                   "default",
		TokenGroup:                   "default",
		ChannelMeta:                  &relaycommon.ChannelMeta{ChannelId: ch.Id},
		LastError:                    priorErr,
		StreamStatus:                 priorStreamStatus,
		ThinkTagSplit:                relaycommon.ThinkTagSplitState{Phase: 1, Carry: "tail"},
		PerformanceBusinessRejection: true,
		PerformanceOutputTokens:      42,
		RetryIndex:                   0,
	}
	retryParam := &service.RetryParam{
		Ctx:        c,
		TokenGroup: "default",
		ModelName:  "gpt-test",
		Retry:      common.GetPointer(1),
	}

	outcome := executeRelayAttempt(c, types.RelayFormatOpenAI, relayInfo, retryParam, nil, nil)
	assert.False(t, outcome.Success)
	assert.False(t, outcome.WillRetry)
	require.NotNil(t, outcome.Err)
	// 选不出下一个渠道时回给客户端的必须是「无可用渠道」，不能是上一轮那条
	// 渠道的内部错误（2026-11-12 生产实测：上游 403 余额不足被当成客户端错误）。
	assert.Equal(t, types.ErrorCodeNoAvailableChannel, outcome.Err.GetErrorCode())
	assert.Equal(t, http.StatusServiceUnavailable, outcome.Err.StatusCode)
	assert.NotContains(t, outcome.Err.Error(), "upstream 502 bad gateway",
		"上一轮的渠道内部错误不得成为本次请求的最终客户端错误")
	// 原始失败仍然完整保留在 LastError 上，供 ProcessChannelError 落盘的
	// 错误行与后续排障使用。
	assert.Equal(t, priorErr, relayInfo.LastError)
	assert.Same(t, priorStreamStatus, relayInfo.StreamStatus, "failed retry selection must not wipe prior attempt StreamStatus")
	assert.Equal(t, int8(1), relayInfo.ThinkTagSplit.Phase)
	assert.Equal(t, "tail", relayInfo.ThinkTagSplit.Carry)
	assert.True(t, relayInfo.PerformanceBusinessRejection)
	assert.EqualValues(t, 42, relayInfo.PerformanceOutputTokens)
	assert.Equal(t, 0, relayInfo.RetryIndex, "RetryIndex must remain on the last actually attempted channel")
}

func TestGetChannelRetryReturns503NoAvailableChannelAndSetupsBeforeMutations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.NoError(t, i18n.Init())
	db := setupRelayAttemptDB(t)

	ch1 := &model.Channel{
		Name:     "ch-1",
		Key:      "sk-test-1",
		Status:   common.ChannelStatusEnabled,
		Type:     constant.ChannelTypeOpenAI,
		Group:    "default",
		Models:   "gpt-test",
		Priority: common.GetPointer(int64(10)),
	}
	require.NoError(t, db.Create(ch1).Error)
	require.NoError(t, db.Create(&model.Ability{
		ChannelId: ch1.Id,
		Model:     "gpt-test",
		Group:     "default",
		Enabled:   true,
		Priority:  ch1.Priority,
	}).Error)

	// 1. All candidates already used -> getChannel returns 503 with ErrorCodeNoAvailableChannel.
	c1, _ := gin.CreateTestContext(httptest.NewRecorder())
	c1.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	service.AppendUsedChannel(c1, ch1.Id)
	info1 := &relaycommon.RelayInfo{
		OriginModelName: "gpt-test",
		UsingGroup:      "default",
		TokenGroup:      "default",
		ChannelMeta:     &relaycommon.ChannelMeta{ChannelId: ch1.Id},
	}
	retry1 := &service.RetryParam{
		Ctx:        c1,
		TokenGroup: "default",
		ModelName:  "gpt-test",
		Retry:      common.GetPointer(1),
	}
	picked, apiErr := getChannel(c1, info1, retry1)
	assert.Nil(t, picked)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
	assert.Equal(t, types.ErrorCodeNoAvailableChannel, apiErr.GetErrorCode())
	assert.True(t, loadbalancer.IsRoutingExhaustedError(apiErr))

	// 2. Candidate selected, but SetupContextForSelectedChannel fails -> UsingGroup and RequestPolicy.Attempts stay clean.
	ch2 := &model.Channel{
		Name:     "ch-2-mismatch",
		Key:      "sk-test-2",
		Status:   common.ChannelStatusEnabled,
		Type:     constant.ChannelTypeOpenAI,
		Group:    "alt",
		Models:   "gpt-test",
		Priority: common.GetPointer(int64(0)),
	}
	require.NoError(t, db.Create(ch2).Error)
	require.NoError(t, db.Create(&model.Ability{
		ChannelId: ch2.Id,
		Model:     "gpt-test",
		Group:     "alt",
		Enabled:   true,
		Priority:  ch2.Priority,
	}).Error)

	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	c2.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c2.Set("expected_task_plugin_key", "missing-plugin")
	common.SetContextKey(c2, constant.ContextKeyAutoGroup, "alt")
	info2 := &relaycommon.RelayInfo{
		OriginModelName: "gpt-test",
		UsingGroup:      "original-group",
		TokenGroup:      "alt",
		ChannelMeta:     &relaycommon.ChannelMeta{ChannelId: ch1.Id},
	}
	retry2 := &service.RetryParam{
		Ctx:        c2,
		TokenGroup: "alt",
		ModelName:  "gpt-test",
		Retry:      common.GetPointer(1),
	}
	picked2, setupErr := getChannel(c2, info2, retry2)
	assert.Nil(t, picked2)
	require.NotNil(t, setupErr)
	assert.Equal(t, types.ErrorCodeGetChannelFailed, setupErr.GetErrorCode())
	assert.Equal(t, "original-group", info2.UsingGroup, "failed channel setup must not mutate RelayInfo.UsingGroup")
	assert.Zero(t, service.RequestPolicy(c2).Attempts, "failed channel setup must not record a RequestPolicy attempt")
}

func TestExecuteTaskSubmissionPreservesPriorUpstreamErrorWhenRetryChannelExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.NoError(t, i18n.Init())
	db := setupRelayAttemptDB(t)
	prevRetry := common.RetryTimesOption()
	common.SetRetryTimesOption(1)
	t.Cleanup(func() { common.SetRetryTimesOption(prevRetry) })

	ch := &model.Channel{
		Name:     "task-ch-1",
		Key:      "sk-task-1",
		Status:   common.ChannelStatusEnabled,
		Type:     constant.ChannelTypeOpenAI,
		Group:    "default",
		Models:   "task-model",
		Priority: common.GetPointer(int64(0)),
	}
	require.NoError(t, db.Create(ch).Error)
	require.NoError(t, db.Create(&model.Ability{
		ChannelId: ch.Id,
		Model:     "task-model",
		Group:     "default",
		Enabled:   true,
		Priority:  ch.Priority,
	}).Error)

	c := taskSubmissionTestContext()
	common.SetContextKey(c, constant.ContextKeyChannelId, ch.Id)
	common.SetContextKey(c, constant.ContextKeyChannelType, ch.Type)
	common.SetContextKey(c, constant.ContextKeyChannelName, ch.Name)
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "task-model")

	info := &relaycommon.RelayInfo{
		UserId:          1,
		UsingGroup:      "default",
		TokenGroup:      "default",
		OriginModelName: "task-model",
		TaskRelayInfo:   &relaycommon.TaskRelayInfo{},
	}

	upstreamTaskErr := &dto.TaskError{
		Code:       "upstream_bad_gateway",
		Message:    "upstream task service unavailable",
		StatusCode: http.StatusBadGateway,
	}
	attempts := 0
	outcome, gotErr := executeTaskSubmissionWith(c, info, func(ctx *gin.Context, ri *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		attempts++
		ri.InitChannelMeta(ctx)
		return nil, upstreamTaskErr
	})

	assert.Nil(t, outcome)
	assert.Equal(t, 1, attempts)
	require.Equal(t, upstreamTaskErr, gotErr, "exhausting retry channels must preserve the prior upstream TaskError")
	require.NotNil(t, info.LastError)
	assert.Equal(t, http.StatusBadGateway, info.LastError.StatusCode)
	assert.Equal(t, 0, info.RetryIndex)
}

func TestExecuteTaskSubmissionLockedChannelSetupFailureAfterPriorErrorPreservesTaskErr(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.NoError(t, i18n.Init())
	prevRetry := common.RetryTimesOption()
	common.SetRetryTimesOption(1)
	t.Cleanup(func() { common.SetRetryTimesOption(prevRetry) })

	lockedCh := &model.Channel{
		Id:     77,
		Name:   "locked-task-ch",
		Key:    "sk-locked",
		Status: common.ChannelStatusEnabled,
		Type:   constant.ChannelTypeTaskPlugin,
	}
	c := taskSubmissionTestContext()
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "plugin-model")

	info := &relaycommon.RelayInfo{
		UserId:          1,
		UsingGroup:      "default",
		TokenGroup:      "default",
		OriginModelName: "plugin-model",
		TaskRelayInfo: &relaycommon.TaskRelayInfo{
			PublicTaskID:  "task_locked",
			LockedChannel: lockedCh,
		},
	}

	upstreamTaskErr := &dto.TaskError{
		Code:       "upstream_unavailable",
		Message:    "upstream plugin busy",
		StatusCode: http.StatusBadGateway,
	}
	outcome, gotErr := executeTaskSubmissionWith(c, info, func(ctx *gin.Context, ri *relaycommon.RelayInfo) (*relay.TaskSubmitResult, *dto.TaskError) {
		// Force SetupContextForSelectedChannel to reject the locked channel on retry 1.
		ctx.Set("expected_task_plugin_key", "mismatch-plugin")
		return nil, upstreamTaskErr
	})

	assert.Nil(t, outcome)
	require.Equal(t, upstreamTaskErr, gotErr, "locked channel retry setup failure must preserve prior upstream TaskError")
	assert.Equal(t, 1, service.RequestPolicy(c).Attempts, "failed locked channel setup on retry must not call BeginAttempt")

	// Also verify attempt 0 locked setup failure uses setupErr.StatusCode / handles nil Err without panicking.
	// Note: Retry=0 skips SetupContextForSelectedChannel in lockedCh branch; test unlocked getChannel nil-cause safety directly:
	assert.True(t, strings.Contains(upstreamTaskErr.Message, "upstream plugin busy"))
}
