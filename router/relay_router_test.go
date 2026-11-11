package router

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
	"github.com/dengyie/apihub/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListModelsSupportsOpenAIAndGeminiAuthentication(t *testing.T) {
	setupRelayRouterTestDB(t)

	user := model.User{
		Username: "models-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    100,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId:         user.Id,
		Key:            "modelstestkey",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}).Error)

	engine := gin.New()
	SetRelayRouter(engine)

	tests := []struct {
		name           string
		path           string
		headerName     string
		expectedObject string
		expectedField  string
	}{
		{
			name:           "OpenAI bearer token",
			path:           "/v1/models",
			headerName:     "Authorization",
			expectedObject: "list",
			expectedField:  "data",
		},
		{
			name:          "Gemini API key header",
			path:          "/v1/models",
			headerName:    "x-goog-api-key",
			expectedField: "models",
		},
		{
			name:          "Gemini API key query",
			path:          "/v1/models?key=modelstestkey",
			expectedField: "models",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.headerName != "" {
				value := "modelstestkey"
				if test.headerName == "Authorization" {
					value = "Bearer " + value
				}
				request.Header.Set(test.headerName, value)
			}

			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code)
			var payload map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
			assert.Contains(t, payload, test.expectedField)
			assert.NotContains(t, payload, "error")
			if test.expectedObject != "" {
				assert.Equal(t, test.expectedObject, payload["object"])
			}
		})
	}
}

func TestRetrieveModelSupportsOpenAIAnthropicAndGeminiRoutes(t *testing.T) {
	setupRelayRouterTestDB(t)

	originalSelfUse := operation_setting.SelfUseModeEnabled
	operation_setting.SelfUseModeEnabled = true
	t.Cleanup(func() {
		operation_setting.SelfUseModeEnabled = originalSelfUse
	})

	user := model.User{
		Username: "retrieve-models-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    100,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId:         user.Id,
		Key:            "retrievetestkey",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:     901,
		Type:   constant.ChannelTypeGemini,
		Key:    "upstream-key",
		Status: common.ChannelStatusEnabled,
		Name:   "gemini-channel",
		Group:  "default",
		Models: "gemini-2.5-flash",
	}).Error)
	require.NoError(t, model.DB.Create(&model.Ability{
		Group:     "default",
		Model:     "gemini-2.5-flash",
		ChannelId: 901,
		Enabled:   true,
	}).Error)

	engine := gin.New()
	SetRelayRouter(engine)

	tests := []struct {
		name           string
		path           string
		headers        map[string]string
		expectedStatus int
		expectedKeys   []string
		absentKeys     []string
	}{
		{
			name:           "OpenAI retrieve visible model",
			path:           "/v1/models/gemini-2.5-flash",
			headers:        map[string]string{"Authorization": "Bearer retrievetestkey"},
			expectedStatus: http.StatusOK,
			expectedKeys:   []string{"id", "object", "owned_by", "supported_endpoint_types"},
			absentKeys:     []string{"error"},
		},
		{
			name:           "OpenAI retrieve missing model returns 404",
			path:           "/v1/models/missing-model",
			headers:        map[string]string{"Authorization": "Bearer retrievetestkey"},
			expectedStatus: http.StatusNotFound,
			expectedKeys:   []string{"error"},
		},
		{
			name: "Anthropic retrieve visible model",
			path: "/v1/models/gemini-2.5-flash",
			headers: map[string]string{
				"x-api-key":         "retrievetestkey",
				"anthropic-version": "2023-06-01",
			},
			expectedStatus: http.StatusOK,
			expectedKeys:   []string{"id", "created_at", "display_name", "type"},
			absentKeys:     []string{"error"},
		},
		{
			name: "Anthropic retrieve missing model returns 404",
			path: "/v1/models/missing-model",
			headers: map[string]string{
				"x-api-key":         "retrievetestkey",
				"anthropic-version": "2023-06-01",
			},
			expectedStatus: http.StatusNotFound,
			expectedKeys:   []string{"type", "error"},
		},
		{
			name:           "v1 models Gemini header retrieve",
			path:           "/v1/models/gemini-2.5-flash",
			headers:        map[string]string{"x-goog-api-key": "retrievetestkey"},
			expectedStatus: http.StatusOK,
			expectedKeys:   []string{"name", "displayName"},
			absentKeys:     []string{"error"},
		},
		{
			name:           "v1 models Gemini query retrieve",
			path:           "/v1/models/gemini-2.5-flash?key=retrievetestkey",
			expectedStatus: http.StatusOK,
			expectedKeys:   []string{"name", "displayName"},
			absentKeys:     []string{"error"},
		},
		{
			name:           "v1beta models Gemini retrieve",
			path:           "/v1beta/models/gemini-2.5-flash",
			headers:        map[string]string{"x-goog-api-key": "retrievetestkey"},
			expectedStatus: http.StatusOK,
			expectedKeys:   []string{"name", "displayName"},
			absentKeys:     []string{"error"},
		},
		{
			name:           "v1beta models Gemini retrieve with models prefix",
			path:           "/v1beta/models/models/gemini-2.5-flash",
			headers:        map[string]string{"x-goog-api-key": "retrievetestkey"},
			expectedStatus: http.StatusOK,
			expectedKeys:   []string{"name", "displayName"},
			absentKeys:     []string{"error"},
		},
		{
			name:           "v1beta models Gemini missing model returns 404",
			path:           "/v1beta/models/missing-model?key=retrievetestkey",
			expectedStatus: http.StatusNotFound,
			expectedKeys:   []string{"error"},
		},
		{
			name:           "v1beta openai compatible retrieve",
			path:           "/v1beta/openai/models/gemini-2.5-flash",
			headers:        map[string]string{"Authorization": "Bearer retrievetestkey"},
			expectedStatus: http.StatusOK,
			expectedKeys:   []string{"id", "object", "owned_by", "supported_endpoint_types"},
			absentKeys:     []string{"error"},
		},
		{
			name:           "v1beta openai compatible missing model returns 404",
			path:           "/v1beta/openai/models/missing-model",
			headers:        map[string]string{"x-goog-api-key": "retrievetestkey"},
			expectedStatus: http.StatusNotFound,
			expectedKeys:   []string{"error"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			for key, value := range test.headers {
				request.Header.Set(key, value)
			}

			engine.ServeHTTP(recorder, request)

			require.Equal(t, test.expectedStatus, recorder.Code)
			var payload map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
			for _, key := range test.expectedKeys {
				assert.Contains(t, payload, key)
			}
			for _, key := range test.absentKeys {
				assert.NotContains(t, payload, key)
			}
		})
	}
}

func setupRelayRouterTestDB(t *testing.T) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	originalIsMasterNode := common.IsMasterNode
	originalRedisEnabled := common.RedisEnabled
	originalSQLitePath := common.SQLitePath
	originalMainDatabaseType := common.MainDatabaseType()
	originalLogDatabaseType := common.LogDatabaseType()
	originalSQLDSN, hadSQLDSN := os.LookupEnv("SQL_DSN")

	common.IsMasterNode = false
	common.RedisEnabled = false
	common.SQLitePath = fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	require.NoError(t, os.Setenv("SQL_DSN", "local"))
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Ability{}, &model.Channel{}, &model.Model{}))
	model.InvalidateCatalogMetadataCache()
	model.InvalidatePricingCache()

	t.Cleanup(func() {
		model.InvalidateCatalogMetadataCache()
		model.InvalidatePricingCache()
		if sqlDB, err := model.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
		common.IsMasterNode = originalIsMasterNode
		common.RedisEnabled = originalRedisEnabled
		common.SQLitePath = originalSQLitePath
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		if hadSQLDSN {
			require.NoError(t, os.Setenv("SQL_DSN", originalSQLDSN))
		} else {
			require.NoError(t, os.Unsetenv("SQL_DSN"))
		}
	})
}
