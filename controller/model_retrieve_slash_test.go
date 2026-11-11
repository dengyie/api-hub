package controller

import (
	"net/http"
	"testing"

	"github.com/dengyie/apihub/common"
	"github.com/dengyie/apihub/constant"
	"github.com/dengyie/apihub/model"
	"github.com/dengyie/apihub/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRetrieveModelStripsCatchAllLeadingSlash 钉住模型斜杠路由的取值侧。
//
// 路由侧原先用 `GET("/:model")` 注册，而模型 ID 本身允许包含 `/`
// （openrouter/free、meta-llama/Llama-3-70B…）。`:model` 只匹配单个路径段，
// `/v1/models/openrouter/free` 在路由层就被拒成 404，请求根本到不了
// RetrieveModel —— 客户端拿到的 404 不是「这个模型不存在」，而是「这个 URL
// 不存在」，两者对客户端的行为完全不同（前者会提示模型名写错，后者像是网关
// 挂了）。
//
// 改成 catch-all 后，Gin 会把前导 `/` 一起捕获进参数：`/v1/models/hy4` 取到
// 的是 `/hy4`。取值侧必须剥掉，否则存在性判断拿 "/hy4" 去比对，索引正常也
// 一律 404。这个用例同时喂「带前导斜杠」和「裸模型名」两种参数，证明剥与不剥
// 都指向同一个模型。
func TestRetrieveModelStripsCatchAllLeadingSlash(t *testing.T) {
	withSelfUseModeEnabled(t)
	db := setupModelListControllerTestDB(t)
	model.InvalidateCatalogMetadataCache()
	model.InvalidatePricingCache()

	require.NoError(t, db.Create(&model.User{
		Id:       1201,
		Username: "slash-model-user",
		Password: "password",
		Group:    "default",
		Status:   common.UserStatusEnabled,
	}).Error)
	require.NoError(t, db.Create(&model.Channel{
		Id:     811,
		Type:   constant.ChannelTypeOpenAI,
		Key:    "slash-route-key",
		Status: common.ChannelStatusEnabled,
		Name:   "slash-route-channel",
		Group:  "default",
		Models: "openrouter/free,hy4",
	}).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "default", Model: "openrouter/free", ChannelId: 811, Enabled: true},
		{Group: "default", Model: "hy4", ChannelId: 811, Enabled: true},
	}).Error)
	model.InitChannelCache()
	model.InvalidateCatalogMetadataCache()
	model.InvalidatePricingCache()
	model.GetPricing()

	setup := func(c *gin.Context) { c.Set("id", 1201) }

	// 带斜杠的模型名：Gin catch-all 传进来的参数是 "/openrouter/free"。
	for _, param := range []string{"/openrouter/free", "openrouter/free"} {
		rec := runRetrieveModelRequest(param, constant.ChannelTypeOpenAI, setup)
		require.Equal(t, http.StatusOK, rec.Code, "param=%q", param)

		var retrieved dto.OpenAIModels
		require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &retrieved))
		assert.Equal(t, "openrouter/free", retrieved.Id, "param=%q 必须解析回原模型名", param)
	}

	// 无斜杠模型名经过 catch-all 同样会带前导 "/"：剥掉后仍是命中，不剥就 404。
	rec := runRetrieveModelRequest("/hy4", constant.ChannelTypeOpenAI, setup)
	require.Equal(t, http.StatusOK, rec.Code)
	var hy4 dto.OpenAIModels
	require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &hy4))
	assert.Equal(t, "hy4", hy4.Id)

	// 反向守卫：剥前导斜杠不能把不存在的模型变成存在。
	missing := runRetrieveModelRequest("/vendor/not-exists", constant.ChannelTypeOpenAI, setup)
	require.Equal(t, http.StatusNotFound, missing.Code)

	// Gemini 的 "models/" 前缀仍然在剥完前导斜杠之后生效。
	gemini := runRetrieveModelRequest("/models/openrouter/free", constant.ChannelTypeGemini, setup)
	require.Equal(t, http.StatusOK, gemini.Code)
	var geminiModel dto.GeminiModel
	require.NoError(t, common.Unmarshal(gemini.Body.Bytes(), &geminiModel))
	assert.Equal(t, "openrouter/free", geminiModel.Name, "Gemini 响应里的模型名不带 models/ 前缀")
}
