package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dengyie/apihub/common"
	"github.com/dengyie/apihub/constant"
	"github.com/dengyie/apihub/model"
	"github.com/dengyie/apihub/relaykit/dto"
	"github.com/dengyie/apihub/relaykit/types"
	"github.com/dengyie/apihub/setting"
	"github.com/dengyie/apihub/setting/config"
	"github.com/dengyie/apihub/setting/operation_setting"
	"github.com/dengyie/apihub/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type listModelsResponse struct {
	Success bool               `json:"success"`
	Data    []dto.OpenAIModels `json:"data"`
	Object  string             `json:"object"`
}

type userModelsResponse struct {
	Success bool     `json:"success"`
	Data    []string `json:"data"`
}

func setupModelListControllerTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	initModelListColumnNames(t)

	gin.SetMode(gin.TestMode)
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	model.LOG_DB = db

	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Channel{}, &model.Ability{}, &model.Model{}, &model.Vendor{}))

	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	return db
}

func initModelListColumnNames(t *testing.T) {
	t.Helper()

	originalIsMasterNode := common.IsMasterNode
	originalSQLitePath := common.SQLitePath
	originalMainDatabaseType := common.MainDatabaseType()
	originalLogDatabaseType := common.LogDatabaseType()
	originalSQLDSN, hadSQLDSN := os.LookupEnv("SQL_DSN")
	defer func() {
		common.IsMasterNode = originalIsMasterNode
		common.SQLitePath = originalSQLitePath
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		if hadSQLDSN {
			require.NoError(t, os.Setenv("SQL_DSN", originalSQLDSN))
		} else {
			require.NoError(t, os.Unsetenv("SQL_DSN"))
		}
	}()

	common.IsMasterNode = false
	common.SQLitePath = fmt.Sprintf("file:%s_init?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	require.NoError(t, os.Setenv("SQL_DSN", "local"))

	require.NoError(t, model.InitDB())
	if model.DB != nil {
		sqlDB, err := model.DB.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	}
}

func withTieredBillingConfig(t *testing.T, modes map[string]string, exprs map[string]string) {
	t.Helper()

	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error {
		if strings.HasPrefix(key, "billing_setting.") {
			saved[key] = value
		}
		return nil
	}))
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
		model.InvalidatePricingCache()
	})

	modeBytes, err := common.Marshal(modes)
	require.NoError(t, err)
	exprBytes, err := common.Marshal(exprs)
	require.NoError(t, err)

	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode": string(modeBytes),
		"billing_setting.billing_expr": string(exprBytes),
	}))
	model.InvalidatePricingCache()
}

func withSelfUseModeDisabled(t *testing.T) {
	t.Helper()

	original := operation_setting.SelfUseModeEnabled
	operation_setting.SelfUseModeEnabled = false
	t.Cleanup(func() {
		operation_setting.SelfUseModeEnabled = original
	})
}

func withSelfUseModeEnabled(t *testing.T) {
	t.Helper()

	original := operation_setting.SelfUseModeEnabled
	operation_setting.SelfUseModeEnabled = true
	t.Cleanup(func() {
		operation_setting.SelfUseModeEnabled = original
	})
}

func decodeListModelsPayload(t *testing.T, recorder *httptest.ResponseRecorder) listModelsResponse {
	t.Helper()

	require.Equal(t, http.StatusOK, recorder.Code)
	var payload listModelsResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Success)
	require.Equal(t, "list", payload.Object)
	return payload
}

func decodeListModelsResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]struct{} {
	t.Helper()

	payload := decodeListModelsPayload(t, recorder)
	ids := make(map[string]struct{}, len(payload.Data))
	for _, item := range payload.Data {
		ids[item.Id] = struct{}{}
	}
	return ids
}

func pricingByModelName(pricings []model.Pricing) map[string]model.Pricing {
	byName := make(map[string]model.Pricing, len(pricings))
	for _, pricing := range pricings {
		byName[pricing.ModelName] = pricing
	}
	return byName
}

func decodeUserModelsResponse(t *testing.T, recorder *httptest.ResponseRecorder) []string {
	t.Helper()

	require.Equal(t, http.StatusOK, recorder.Code)
	var payload userModelsResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Success)
	return payload.Data
}

func TestGetUserModelsFiltersByRequestedGroup(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.Create(&model.User{
		Id:       1002,
		Username: "playground-model-user",
		Password: "password",
		Group:    "default",
		Status:   common.UserStatusEnabled,
	}).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "default", Model: "zz-default-only-model", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "zz-disabled-model", ChannelId: 1, Enabled: false},
	}).Error)

	defaultRecorder := httptest.NewRecorder()
	defaultContext, _ := gin.CreateTestContext(defaultRecorder)
	defaultContext.Request = httptest.NewRequest(http.MethodGet, "/api/user/models?group=default", nil)
	defaultContext.Set("id", 1002)

	GetUserModels(defaultContext)

	defaultModels := decodeUserModelsResponse(t, defaultRecorder)
	require.ElementsMatch(t, []string{"zz-default-only-model"}, defaultModels)

	vipRecorder := httptest.NewRecorder()
	vipContext, _ := gin.CreateTestContext(vipRecorder)
	vipContext.Request = httptest.NewRequest(http.MethodGet, "/api/user/models?group=vip", nil)
	vipContext.Set("id", 1002)

	GetUserModels(vipContext)

	require.Empty(t, decodeUserModelsResponse(t, vipRecorder))
}

func TestGetUserModelsExpandsAutoGroupsInConfiguredOrder(t *testing.T) {
	originalAutoGroups := setting.AutoGroups2JsonString()
	originalUsableGroups := setting.UserUsableGroups2JSONString()
	originalSpecialGroups := ratio_setting.GetGroupRatioSetting().GroupSpecialUsableGroup.ReadAll()
	t.Cleanup(func() {
		require.NoError(t, setting.UpdateAutoGroupsByJsonString(originalAutoGroups))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(originalUsableGroups))
		specialGroups := ratio_setting.GetGroupRatioSetting().GroupSpecialUsableGroup
		specialGroups.Clear()
		specialGroups.AddAll(originalSpecialGroups)
	})

	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`["vip","default","unavailable"]`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"auto":"自动分组","default":"默认分组","unavailable":"不可用分组"}`))
	specialGroups := ratio_setting.GetGroupRatioSetting().GroupSpecialUsableGroup
	specialGroups.Clear()
	specialGroups.Set("default", map[string]string{
		"+:vip":         "VIP 分组",
		"-:unavailable": "",
	})

	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.Create(&model.User{
		Id:       1003,
		Username: "playground-auto-model-user",
		Password: "password",
		Group:    "default",
		Status:   common.UserStatusEnabled,
	}).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "vip", Model: "zz-vip-model", ChannelId: 1, Enabled: true},
		{Group: "vip", Model: "zz-shared-model", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "zz-default-model", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "zz-shared-model", ChannelId: 2, Enabled: true},
		{Group: "unavailable", Model: "zz-unavailable-model", ChannelId: 1, Enabled: true},
	}).Error)

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/user/models?group=auto", nil)
	context.Set("id", 1003)

	GetUserModels(context)

	models := decodeUserModelsResponse(t, recorder)
	require.Len(t, models, 3)
	assert.ElementsMatch(t, []string{"zz-vip-model", "zz-shared-model"}, models[:2])
	assert.Equal(t, "zz-default-model", models[2])
}

func TestListModelsIncludesTieredBillingModel(t *testing.T) {
	withSelfUseModeDisabled(t)
	withTieredBillingConfig(t, map[string]string{
		"zz-tiered-visible-model":      "tiered_expr",
		"zz-tiered-empty-expr-model":   "tiered_expr",
		"zz-tiered-missing-expr-model": "tiered_expr",
	}, map[string]string{
		"zz-tiered-visible-model":    `tier("base", p * 1 + c * 2)`,
		"zz-tiered-empty-expr-model": "   ",
	})

	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.Create(&model.User{
		Id:       1001,
		Username: "model-list-user",
		Password: "password",
		Group:    "default",
		Status:   common.UserStatusEnabled,
	}).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "default", Model: "zz-tiered-visible-model", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "zz-tiered-empty-expr-model", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "zz-tiered-missing-expr-model", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "zz-unpriced-model", ChannelId: 1, Enabled: true},
	}).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	ctx.Set("id", 1001)

	ListModels(ctx, constant.ChannelTypeOpenAI)

	ids := decodeListModelsResponse(t, recorder)
	require.Contains(t, ids, "zz-tiered-visible-model")
	require.NotContains(t, ids, "zz-tiered-empty-expr-model")
	require.NotContains(t, ids, "zz-tiered-missing-expr-model")
	require.NotContains(t, ids, "zz-unpriced-model")

	pricingByName := pricingByModelName(model.GetPricing())
	visiblePricing, ok := pricingByName["zz-tiered-visible-model"]
	require.True(t, ok)
	require.Equal(t, "tiered_expr", visiblePricing.BillingMode)
	require.NotEmpty(t, visiblePricing.BillingExpr)

	emptyExprPricing, ok := pricingByName["zz-tiered-empty-expr-model"]
	require.True(t, ok)
	require.Empty(t, emptyExprPricing.BillingMode)
	require.Empty(t, emptyExprPricing.BillingExpr)

	missingExprPricing, ok := pricingByName["zz-tiered-missing-expr-model"]
	require.True(t, ok)
	require.Empty(t, missingExprPricing.BillingMode)
	require.Empty(t, missingExprPricing.BillingExpr)
}

func TestListModelsUsesAdvancedCustomEndpointTypesFromPricingCache(t *testing.T) {
	withSelfUseModeEnabled(t)
	db := setupModelListControllerTestDB(t)

	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		model.InvalidatePricingCache()
	})

	require.NoError(t, db.Create(&model.User{
		Id:       1003,
		Username: "advanced-custom-model-list-user",
		Password: "password",
		Group:    "default",
		Status:   common.UserStatusEnabled,
	}).Error)

	channel := &model.Channel{
		Id:     701,
		Type:   constant.ChannelTypeAdvancedCustom,
		Key:    "advanced-custom-key",
		Status: common.ChannelStatusEnabled,
		Name:   "advanced-custom-channel",
		Group:  "default",
		Models: "gemini-3.5-flash",
	}
	channel.SetOtherSettings(dto.ChannelOtherSettings{
		AdvancedCustom: &dto.AdvancedCustomConfig{
			Routes: []dto.AdvancedCustomRoute{
				{
					IncomingPath: "/v1/chat/completions",
					UpstreamPath: "/v1/chat/completions",
				},
				{
					IncomingPath: "/v1/responses",
					UpstreamPath: "/v1beta/models/{model}:generateContent",
					Converter:    "openai_responses_to_gemini_generate_content",
					Models:       []string{"re:^gemini-"},
				},
			},
		},
	})
	require.NoError(t, db.Create(channel).Error)
	require.NoError(t, db.Create(&model.Ability{
		Group:     "default",
		Model:     "gemini-3.5-flash",
		ChannelId: 701,
		Enabled:   true,
	}).Error)

	model.InitChannelCache()
	model.GetPricing()

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	ctx.Set("id", 1003)

	ListModels(ctx, constant.ChannelTypeOpenAI)

	payload := decodeListModelsPayload(t, recorder)
	require.Len(t, payload.Data, 1)
	require.Equal(t, "gemini-3.5-flash", payload.Data[0].Id)
	require.Equal(t, []constant.EndpointType{
		constant.EndpointTypeOpenAI,
		constant.EndpointTypeOpenAIResponse,
	}, payload.Data[0].SupportedEndpointTypes)
}

func TestListModelsTokenLimitIncludesTieredBillingModel(t *testing.T) {
	withSelfUseModeDisabled(t)
	withTieredBillingConfig(t, map[string]string{
		"zz-token-tiered-visible-model":      "tiered_expr",
		"zz-token-tiered-empty-expr-model":   "tiered_expr",
		"zz-token-tiered-missing-expr-model": "tiered_expr",
	}, map[string]string{
		"zz-token-tiered-visible-model":    `tier("base", p * 1 + c * 2)`,
		"zz-token-tiered-empty-expr-model": "",
	})
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "default", Model: "zz-token-tiered-visible-model", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "zz-token-tiered-empty-expr-model", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "zz-token-tiered-missing-expr-model", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "zz-token-unpriced-model", ChannelId: 1, Enabled: true},
	}).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(ctx, constant.ContextKeyTokenModelLimitEnabled, true)
	common.SetContextKey(ctx, constant.ContextKeyTokenModelLimit, map[string]bool{
		"zz-token-tiered-visible-model":      true,
		"zz-token-tiered-empty-expr-model":   true,
		"zz-token-tiered-missing-expr-model": true,
		"zz-token-unpriced-model":            true,
	})

	ListModels(ctx, constant.ChannelTypeOpenAI)

	ids := decodeListModelsResponse(t, recorder)
	require.Contains(t, ids, "zz-token-tiered-visible-model")
	require.NotContains(t, ids, "zz-token-tiered-empty-expr-model")
	require.NotContains(t, ids, "zz-token-tiered-missing-expr-model")
	require.NotContains(t, ids, "zz-token-unpriced-model")
}

func TestListModelsTokenLimitUsesResolvedCustomAutoGroups(t *testing.T) {
	withSelfUseModeEnabled(t)
	originalMax := setting.GetMaxTokenAutoGroups()
	originalUsableGroups := setting.UserUsableGroups2JSONString()
	originalRatios := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, setting.UpdateMaxTokenAutoGroups("5"))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","vip":"VIP"}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":1}`))
	t.Cleanup(func() {
		require.NoError(t, setting.UpdateMaxTokenAutoGroups(fmt.Sprintf("%d", originalMax)))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(originalUsableGroups))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalRatios))
	})

	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "vip", Model: "zz-vip-allowed", ChannelId: 1, Enabled: true},
		{Group: "vip", Model: "zz-vip-denied", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "zz-default-outside-snapshot", ChannelId: 1, Enabled: true},
	}).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(ctx, constant.ContextKeyTokenGroup, "auto")
	common.SetContextKey(ctx, constant.ContextKeyTokenAutoGroups, []string{"vip"})
	common.SetContextKey(ctx, constant.ContextKeyTokenModelLimitEnabled, true)
	common.SetContextKey(ctx, constant.ContextKeyTokenModelLimit, map[string]bool{
		"zz-vip-allowed":              true,
		"zz-default-outside-snapshot": true,
		"zz-not-enabled":              true,
	})

	ListModels(ctx, constant.ChannelTypeOpenAI)
	ids := decodeListModelsResponse(t, recorder)
	require.Equal(t, map[string]struct{}{"zz-vip-allowed": {}}, ids)

	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default"}`))
	emptyRecorder := httptest.NewRecorder()
	emptyCtx, _ := gin.CreateTestContext(emptyRecorder)
	emptyCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	common.SetContextKey(emptyCtx, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(emptyCtx, constant.ContextKeyTokenGroup, "auto")
	common.SetContextKey(emptyCtx, constant.ContextKeyTokenAutoGroups, []string{"vip"})
	common.SetContextKey(emptyCtx, constant.ContextKeyTokenModelLimitEnabled, true)
	common.SetContextKey(emptyCtx, constant.ContextKeyTokenModelLimit, map[string]bool{"zz-vip-allowed": true})

	require.NotPanics(t, func() {
		ListModels(emptyCtx, constant.ChannelTypeAnthropic)
	})
	var anthropicResponse struct {
		Data    []dto.AnthropicModel `json:"data"`
		FirstID string               `json:"first_id"`
		LastID  string               `json:"last_id"`
	}
	require.NoError(t, common.Unmarshal(emptyRecorder.Body.Bytes(), &anthropicResponse))
	require.Empty(t, anthropicResponse.Data)
	require.Empty(t, anthropicResponse.FirstID)
	require.Empty(t, anthropicResponse.LastID)
}

func TestSetupLoginDoesNotTouchPasswordWhenPasswordFieldOmitted(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AuditLog{}, &model.UserSession{}, &model.TwoFA{}, &model.PasskeyCredential{}))

	hashedPassword, err := common.Password2Hash("CurrentPassword123")
	require.NoError(t, err)
	user := &model.User{
		Username: "twofa-user",
		Password: hashedPassword,
		Role:     common.RoleCommonUser,
		Status:   common.UserStatusEnabled,
		Group:    "default",
	}
	require.NoError(t, db.Create(user).Error)

	router := gin.New()
	router.GET("/", func(c *gin.Context) {
		setupLogin(&model.User{
			Id:          user.Id,
			AuthVersion: user.AuthVersion,
			Username:    user.Username,
			Role:        user.Role,
			Status:      user.Status,
			Group:       user.Group,
		}, nil, c)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	var stored model.User
	require.NoError(t, db.First(&stored, user.Id).Error)
	assert.Equal(t, hashedPassword, stored.Password)
}

func runRetrieveModelRequest(modelId string, modelType int, setupCtx func(c *gin.Context)) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models/"+modelId, nil)
	ctx.Params = gin.Params{{Key: "model", Value: modelId}}
	if setupCtx != nil {
		setupCtx(ctx)
	}
	RetrieveModel(ctx, modelType)
	return recorder
}

func TestRetrieveModelMatchesListModelsVisibilityAndEnrichment(t *testing.T) {
	withSelfUseModeEnabled(t)
	db := setupModelListControllerTestDB(t)
	model.InvalidateCatalogMetadataCache()
	model.InvalidatePricingCache()
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		model.InvalidateCatalogMetadataCache()
		model.InvalidatePricingCache()
	})

	require.NoError(t, db.Create(&model.User{
		Id:       1101,
		Username: "retrieve-model-user",
		Password: "password",
		Group:    "default",
		Status:   common.UserStatusEnabled,
	}).Error)

	channel := &model.Channel{
		Id:     801,
		Type:   constant.ChannelTypeAdvancedCustom,
		Key:    "retrieve-custom-key",
		Status: common.ChannelStatusEnabled,
		Name:   "retrieve-custom-channel",
		Group:  "default",
		Models: "zz-custom-retrieve-model,zz-hidden-retrieve-model",
	}
	channel.SetOtherSettings(dto.ChannelOtherSettings{
		AdvancedCustom: &dto.AdvancedCustomConfig{
			Routes: []dto.AdvancedCustomRoute{
				{
					IncomingPath: "/v1/chat/completions",
					UpstreamPath: "/v1/chat/completions",
				},
				{
					IncomingPath: "/v1/responses",
					UpstreamPath: "/v1/responses",
					Models:       []string{"zz-custom-retrieve-model"},
				},
			},
		},
	})
	require.NoError(t, db.Create(channel).Error)
	require.NoError(t, db.Create(&model.Channel{
		Id:     802,
		Type:   constant.ChannelTypeOpenAI,
		Key:    "retrieve-vip-key",
		Status: common.ChannelStatusEnabled,
		Name:   "retrieve-vip-channel",
		Group:  "vip",
		Models: "gpt-4o-mini",
	}).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "default", Model: "zz-custom-retrieve-model", ChannelId: 801, Enabled: true},
		{Group: "default", Model: "zz-hidden-retrieve-model", ChannelId: 801, Enabled: true},
		{Group: "vip", Model: "gpt-4o-mini", ChannelId: 802, Enabled: true},
	}).Error)
	hiddenModel := model.Model{
		ModelName: "zz-hidden-retrieve-model",
		NameRule:  model.NameRuleExact,
	}
	require.NoError(t, db.Create(&hiddenModel).Error)
	require.NoError(t, db.Model(&hiddenModel).Update("status", 0).Error)

	model.InitChannelCache()
	model.InvalidateCatalogMetadataCache()
	model.InvalidatePricingCache()
	model.GetPricing()

	// Custom model visible to default user -> 200 with owner & supported_endpoint_types matching ListModels.
	listRecorder := httptest.NewRecorder()
	listCtx, _ := gin.CreateTestContext(listRecorder)
	listCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	listCtx.Set("id", 1101)
	ListModels(listCtx, constant.ChannelTypeOpenAI)
	listPayload := decodeListModelsPayload(t, listRecorder)
	require.Len(t, listPayload.Data, 1)

	openAIRec := runRetrieveModelRequest("zz-custom-retrieve-model", constant.ChannelTypeOpenAI, func(c *gin.Context) {
		c.Set("id", 1101)
	})
	require.Equal(t, http.StatusOK, openAIRec.Code)
	var retrievedOpenAI dto.OpenAIModels
	require.NoError(t, common.Unmarshal(openAIRec.Body.Bytes(), &retrievedOpenAI))
	assert.Equal(t, listPayload.Data[0], retrievedOpenAI)
	assert.Equal(t, channelOwnerName(constant.ChannelTypeAdvancedCustom), retrievedOpenAI.OwnedBy)
	assert.Equal(t, []constant.EndpointType{
		constant.EndpointTypeOpenAI,
		constant.EndpointTypeOpenAIResponse,
	}, retrievedOpenAI.SupportedEndpointTypes)

	// Anthropic format
	anthropicRec := runRetrieveModelRequest("zz-custom-retrieve-model", constant.ChannelTypeAnthropic, func(c *gin.Context) {
		c.Set("id", 1101)
	})
	require.Equal(t, http.StatusOK, anthropicRec.Code)
	var retrievedAnthropic dto.AnthropicModel
	require.NoError(t, common.Unmarshal(anthropicRec.Body.Bytes(), &retrievedAnthropic))
	assert.Equal(t, "zz-custom-retrieve-model", retrievedAnthropic.ID)
	assert.Equal(t, "zz-custom-retrieve-model", retrievedAnthropic.DisplayName)
	assert.Equal(t, "model", retrievedAnthropic.Type)
	assert.NotEmpty(t, retrievedAnthropic.CreatedAt)

	// Gemini format
	geminiRec := runRetrieveModelRequest("zz-custom-retrieve-model", constant.ChannelTypeGemini, func(c *gin.Context) {
		c.Set("id", 1101)
	})
	require.Equal(t, http.StatusOK, geminiRec.Code)
	var retrievedGemini dto.GeminiModel
	require.NoError(t, common.Unmarshal(geminiRec.Body.Bytes(), &retrievedGemini))
	assert.Equal(t, "zz-custom-retrieve-model", retrievedGemini.Name)
	assert.Equal(t, "zz-custom-retrieve-model", retrievedGemini.DisplayName)

	// Hidden catalog model, other-group static model (gpt-4o-mini only in vip), and unknown model -> 404.
	for _, hiddenOrMissing := range []string{"zz-hidden-retrieve-model", "gpt-4o-mini", "zz-nonexistent-model"} {
		rec := runRetrieveModelRequest(hiddenOrMissing, constant.ChannelTypeOpenAI, func(c *gin.Context) {
			c.Set("id", 1101)
		})
		require.Equal(t, http.StatusNotFound, rec.Code, "model=%s", hiddenOrMissing)
		var errResp struct {
			Error types.OpenAIError `json:"error"`
		}
		require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, "model_not_found", errResp.Error.Code)
		assert.Equal(t, "invalid_request_error", errResp.Error.Type)
		assert.Equal(t, "model", errResp.Error.Param)

		claudeMiss := runRetrieveModelRequest(hiddenOrMissing, constant.ChannelTypeAnthropic, func(c *gin.Context) {
			c.Set("id", 1101)
		})
		require.Equal(t, http.StatusNotFound, claudeMiss.Code, "claude model=%s", hiddenOrMissing)
		var claudeErrResp struct {
			Type  string            `json:"type"`
			Error types.ClaudeError `json:"error"`
		}
		require.NoError(t, common.Unmarshal(claudeMiss.Body.Bytes(), &claudeErrResp))
		assert.Equal(t, "error", claudeErrResp.Type)
		assert.Equal(t, "not_found_error", claudeErrResp.Error.Type)

		geminiMiss := runRetrieveModelRequest(hiddenOrMissing, constant.ChannelTypeGemini, func(c *gin.Context) {
			c.Set("id", 1101)
		})
		require.Equal(t, http.StatusNotFound, geminiMiss.Code, "gemini model=%s", hiddenOrMissing)
	}

	// Unauthenticated / empty group context must not leak static models -> 404.
	emptyUserRec := runRetrieveModelRequest("gpt-4o", constant.ChannelTypeOpenAI, nil)
	require.Equal(t, http.StatusNotFound, emptyUserRec.Code)

	// Database error while resolving user group -> 500 with protocol-matched error body.
	sqlDB, dbErr := db.DB()
	require.NoError(t, dbErr)
	require.NoError(t, sqlDB.Close())

	errRec := runRetrieveModelRequest("zz-custom-retrieve-model", constant.ChannelTypeOpenAI, func(c *gin.Context) {
		c.Set("id", 1101)
	})
	require.Equal(t, http.StatusInternalServerError, errRec.Code)
	var errPayload struct {
		Error types.OpenAIError `json:"error"`
	}
	require.NoError(t, common.Unmarshal(errRec.Body.Bytes(), &errPayload))
	assert.Equal(t, "get_user_group_failed", errPayload.Error.Code)

	claudeErrRec := runRetrieveModelRequest("zz-custom-retrieve-model", constant.ChannelTypeAnthropic, func(c *gin.Context) {
		c.Set("id", 1101)
	})
	require.Equal(t, http.StatusInternalServerError, claudeErrRec.Code)
	var claudeErrPayload struct {
		Type  string            `json:"type"`
		Error types.ClaudeError `json:"error"`
	}
	require.NoError(t, common.Unmarshal(claudeErrRec.Body.Bytes(), &claudeErrPayload))
	assert.Equal(t, "error", claudeErrPayload.Type)
	assert.Equal(t, "api_error", claudeErrPayload.Error.Type)
}

func TestRetrieveModelEnforcesBillingConfigTokenLimitsAndAutoGroups(t *testing.T) {
	withSelfUseModeDisabled(t)
	withTieredBillingConfig(t, map[string]string{
		"zz-retrieve-tiered-ok":    "tiered_expr",
		"zz-retrieve-tiered-empty": "tiered_expr",
	}, map[string]string{
		"zz-retrieve-tiered-ok":    `tier("base", p * 1 + c * 2)`,
		"zz-retrieve-tiered-empty": "   ",
	})

	originalMax := setting.GetMaxTokenAutoGroups()
	originalUsableGroups := setting.UserUsableGroups2JSONString()
	originalRatios := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, setting.UpdateMaxTokenAutoGroups("5"))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","vip":"VIP"}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":1}`))
	t.Cleanup(func() {
		require.NoError(t, setting.UpdateMaxTokenAutoGroups(fmt.Sprintf("%d", originalMax)))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(originalUsableGroups))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalRatios))
	})

	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.Create(&model.User{
		Id:       1102,
		Username: "retrieve-billing-strict-user",
		Password: "password",
		Group:    "default",
		AffCode:  "aff-1102",
		Status:   common.UserStatusEnabled,
	}).Error)
	optInUser := &model.User{
		Id:       1103,
		Username: "retrieve-billing-optin-user",
		Password: "password",
		Group:    "default",
		AffCode:  "aff-1103",
		Status:   common.UserStatusEnabled,
	}
	optInUser.SetSetting(dto.UserSetting{AcceptUnsetRatioModel: true})
	require.NoError(t, db.Create(optInUser).Error)

	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "default", Model: "zz-retrieve-tiered-ok", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "zz-retrieve-tiered-empty", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "zz-retrieve-unpriced", ChannelId: 1, Enabled: true},
		{Group: "vip", Model: "zz-retrieve-tiered-ok", ChannelId: 2, Enabled: true},
		{Group: "vip", Model: "qwen3.8-flash", ChannelId: 2, Enabled: true},
	}).Error)

	// Strict user sees billed model, rejects empty tiered expr and unpriced model.
	require.Equal(t, http.StatusOK, runRetrieveModelRequest("zz-retrieve-tiered-ok", constant.ChannelTypeOpenAI, func(c *gin.Context) {
		c.Set("id", 1102)
	}).Code)
	require.Equal(t, http.StatusNotFound, runRetrieveModelRequest("zz-retrieve-tiered-empty", constant.ChannelTypeOpenAI, func(c *gin.Context) {
		c.Set("id", 1102)
	}).Code)
	require.Equal(t, http.StatusNotFound, runRetrieveModelRequest("zz-retrieve-unpriced", constant.ChannelTypeOpenAI, func(c *gin.Context) {
		c.Set("id", 1102)
	}).Code)

	// Opt-in AcceptUnsetRatioModel user sees unpriced model.
	require.Equal(t, http.StatusOK, runRetrieveModelRequest("zz-retrieve-unpriced", constant.ChannelTypeOpenAI, func(c *gin.Context) {
		c.Set("id", 1103)
	}).Code)

	// Token auto snapshot + model limits (including canonical RoutingMatchModelName match).
	autoLimitSetup := func(c *gin.Context) {
		c.Set("id", 1103)
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenGroup, "auto")
		common.SetContextKey(c, constant.ContextKeyTokenAutoGroups, []string{"vip"})
		common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
		common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{
			"qwen-3.8-flash":       true, // canonical match for vip's qwen3.8-flash
			"zz-retrieve-unpriced": true, // only in default, outside vip snapshot!
		})
	}
	require.Equal(t, http.StatusOK, runRetrieveModelRequest("qwen3.8-flash", constant.ChannelTypeOpenAI, autoLimitSetup).Code)
	require.Equal(t, http.StatusNotFound, runRetrieveModelRequest("zz-retrieve-tiered-ok", constant.ChannelTypeOpenAI, autoLimitSetup).Code)
	require.Equal(t, http.StatusNotFound, runRetrieveModelRequest("zz-retrieve-unpriced", constant.ChannelTypeOpenAI, autoLimitSetup).Code)
}
