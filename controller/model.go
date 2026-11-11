package controller

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dengyie/apihub/common"
	"github.com/dengyie/apihub/constant"
	"github.com/dengyie/apihub/model"
	"github.com/dengyie/apihub/pkg/jsplugin"
	"github.com/dengyie/apihub/relay"
	"github.com/dengyie/apihub/relay/channel/ai360"
	"github.com/dengyie/apihub/relay/channel/lingyiwanwu"
	"github.com/dengyie/apihub/relay/channel/minimax"
	"github.com/dengyie/apihub/relay/channel/moonshot"
	relaycommon "github.com/dengyie/apihub/relay/common"
	"github.com/dengyie/apihub/relay/helper"
	"github.com/dengyie/apihub/relaykit/dto"
	"github.com/dengyie/apihub/relaykit/types"
	"github.com/dengyie/apihub/service"
	"github.com/dengyie/apihub/setting/operation_setting"
	"github.com/dengyie/apihub/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

// https://platform.openai.com/docs/api-reference/models/list

var openAIModels []dto.OpenAIModels
var openAIModelsMap map[string]dto.OpenAIModels
var channelId2Models map[int][]string

func init() {
	// https://platform.openai.com/docs/models/model-endpoint-compatibility
	for i := range constant.APITypeDummy {
		if i == constant.APITypeAIProxyLibrary {
			continue
		}
		adaptor := relay.GetAdaptor(i)
		channelName := adaptor.GetChannelName()
		modelNames := adaptor.GetModelList()
		for _, modelName := range modelNames {
			openAIModels = append(openAIModels, dto.OpenAIModels{
				Id:      modelName,
				Object:  "model",
				Created: 1626777600,
				OwnedBy: channelName,
			})
		}
	}
	for _, modelName := range ai360.ModelList {
		openAIModels = append(openAIModels, dto.OpenAIModels{
			Id:      modelName,
			Object:  "model",
			Created: 1626777600,
			OwnedBy: ai360.ChannelName,
		})
	}
	for _, modelName := range moonshot.ModelList {
		openAIModels = append(openAIModels, dto.OpenAIModels{
			Id:      modelName,
			Object:  "model",
			Created: 1626777600,
			OwnedBy: moonshot.ChannelName,
		})
	}
	for _, modelName := range lingyiwanwu.ModelList {
		openAIModels = append(openAIModels, dto.OpenAIModels{
			Id:      modelName,
			Object:  "model",
			Created: 1626777600,
			OwnedBy: lingyiwanwu.ChannelName,
		})
	}
	for _, modelName := range minimax.ModelList {
		openAIModels = append(openAIModels, dto.OpenAIModels{
			Id:      modelName,
			Object:  "model",
			Created: 1626777600,
			OwnedBy: minimax.ChannelName,
		})
	}
	for modelName, _ := range constant.MidjourneyModel2Action {
		openAIModels = append(openAIModels, dto.OpenAIModels{
			Id:      modelName,
			Object:  "model",
			Created: 1626777600,
			OwnedBy: "midjourney",
		})
	}
	openAIModelsMap = make(map[string]dto.OpenAIModels)
	for _, aiModel := range openAIModels {
		openAIModelsMap[aiModel.Id] = aiModel
	}
	channelId2Models = make(map[int][]string)
	for i := 1; i <= constant.ChannelTypeDummy; i++ {
		apiType, success := common.ChannelType2APIType(i)
		if !success || apiType == constant.APITypeAIProxyLibrary {
			if plugin, ok := jsplugin.DefaultRegistry.GetByChannelType(i); ok {
				channelId2Models[i] = append([]string(nil), plugin.Meta.Models...)
			}
			continue
		}
		meta := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType: i,
		}}
		adaptor := relay.GetAdaptor(apiType)
		adaptor.Init(meta)
		channelId2Models[i] = adaptor.GetModelList()
		if len(channelId2Models[i]) == 0 {
			if plugin, ok := jsplugin.DefaultRegistry.GetByChannelType(i); ok {
				channelId2Models[i] = append([]string(nil), plugin.Meta.Models...)
			}
		}
	}
	openAIModels = lo.UniqBy(openAIModels, func(m dto.OpenAIModels) string {
		return m.Id
	})
}

func channelOwnerName(channelType int) string {
	apiType, success := common.ChannelType2APIType(channelType)
	if !success {
		return strings.ToLower(constant.GetChannelTypeName(channelType))
	}
	adaptor := relay.GetAdaptor(apiType)
	if adaptor == nil {
		return strings.ToLower(constant.GetChannelTypeName(channelType))
	}
	adaptor.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelType: channelType,
	}})
	if name := strings.TrimSpace(adaptor.GetChannelName()); name != "" {
		return name
	}
	return strings.ToLower(constant.GetChannelTypeName(channelType))
}

func getPreferredModelOwners(modelNames []string, groups []string) map[string]string {
	channelTypes, err := model.GetPreferredModelOwnerChannelTypes(modelNames, groups)
	if err != nil {
		common.SysLog(fmt.Sprintf("GetPreferredModelOwnerChannelTypes error: %v", err))
		return map[string]string{}
	}

	ownerByChannelType := make(map[int]string)
	owners := make(map[string]string, len(channelTypes))
	for modelName, channelType := range channelTypes {
		owner, ok := ownerByChannelType[channelType]
		if !ok {
			owner = channelOwnerName(channelType)
			ownerByChannelType[channelType] = owner
		}
		if owner != "" {
			owners[modelName] = owner
		}
	}
	return owners
}

func buildOpenAIModel(modelName string, ownerByModel map[string]string) dto.OpenAIModels {
	var oaiModel dto.OpenAIModels
	if staticModel, ok := openAIModelsMap[modelName]; ok {
		oaiModel = staticModel
	} else {
		oaiModel = dto.OpenAIModels{
			Id:      modelName,
			Object:  "model",
			Created: 1626777600,
			OwnedBy: "custom",
		}
	}
	if owner, ok := ownerByModel[modelName]; ok && owner != "" {
		oaiModel.OwnedBy = owner
	}
	oaiModel.SupportedEndpointTypes = model.GetModelSupportEndpointTypes(modelName)
	return oaiModel
}

type modelListGroups struct {
	userGroup   string
	tokenGroup  string
	ownerGroups []string
}

func getModelListGroups(c *gin.Context) (modelListGroups, error) {
	tokenGroup := common.GetContextKeyString(c, constant.ContextKeyTokenGroup)
	userGroup := common.GetContextKeyString(c, constant.ContextKeyUserGroup)
	if userGroup == "" && (tokenGroup == "" || tokenGroup == "auto") {
		var err error
		userGroup, err = model.GetUserGroup(c.GetInt("id"), false)
		if err != nil {
			return modelListGroups{}, err
		}
	}

	if tokenGroup == "auto" {
		return modelListGroups{
			userGroup:   userGroup,
			tokenGroup:  tokenGroup,
			ownerGroups: service.GetRequestAutoGroups(c, userGroup),
		}, nil
	}

	group := userGroup
	if tokenGroup != "" {
		group = tokenGroup
	}
	return modelListGroups{
		userGroup:   userGroup,
		tokenGroup:  tokenGroup,
		ownerGroups: []string{group},
	}, nil
}

type visibleModelContext struct {
	acceptUnsetRatioModel bool
	ownerGroups           []string
	modelLimitEnable      bool
	tokenModelLimit       map[string]bool
}

func resolveVisibleModelContext(c *gin.Context) (visibleModelContext, error) {
	acceptUnsetRatioModel := operation_setting.SelfUseModeEnabled
	if !acceptUnsetRatioModel {
		userId := c.GetInt("id")
		if userId > 0 {
			userSettings, _ := model.GetUserSetting(userId, false)
			if userSettings.AcceptUnsetRatioModel {
				acceptUnsetRatioModel = true
			}
		}
	}

	groups, err := getModelListGroups(c)
	if err != nil {
		return visibleModelContext{}, err
	}

	modelLimitEnable := common.GetContextKeyBool(c, constant.ContextKeyTokenModelLimitEnabled)
	var tokenModelLimit map[string]bool
	if modelLimitEnable {
		s, ok := common.GetContextKey(c, constant.ContextKeyTokenModelLimit)
		if ok {
			tokenModelLimit, _ = s.(map[string]bool)
		}
		if tokenModelLimit == nil {
			tokenModelLimit = map[string]bool{}
		}
	}

	return visibleModelContext{
		acceptUnsetRatioModel: acceptUnsetRatioModel,
		ownerGroups:           groups.ownerGroups,
		modelLimitEnable:      modelLimitEnable,
		tokenModelLimit:       tokenModelLimit,
	}, nil
}

func (v visibleModelContext) allowsModel(modelName string) bool {
	if v.modelLimitEnable {
		matchingName := ratio_setting.RoutingMatchModelName(modelName)
		if !v.tokenModelLimit[modelName] && !v.tokenModelLimit[matchingName] {
			return false
		}
	}
	if !v.acceptUnsetRatioModel && !helper.HasModelBillingConfig(modelName) {
		return false
	}
	return true
}

func toAnthropicModel(aiModel dto.OpenAIModels) dto.AnthropicModel {
	return dto.AnthropicModel{
		ID:          aiModel.Id,
		CreatedAt:   time.Unix(int64(aiModel.Created), 0).UTC().Format(time.RFC3339),
		DisplayName: aiModel.Id,
		Type:        "model",
	}
}

func toGeminiModel(aiModel dto.OpenAIModels) dto.GeminiModel {
	return dto.GeminiModel{
		Name:        aiModel.Id,
		DisplayName: aiModel.Id,
	}
}

func ListModels(c *gin.Context, modelType int) {
	visCtx, err := resolveVisibleModelContext(c)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "get user group failed",
		})
		return
	}

	userModelNames := make([]string, 0)
	models := service.GetGroupsEnabledModels(visCtx.ownerGroups)
	for _, modelName := range models {
		if !visCtx.allowsModel(modelName) {
			continue
		}
		userModelNames = append(userModelNames, modelName)
	}

	ownerByModel := map[string]string{}
	if len(visCtx.ownerGroups) > 0 {
		ownerByModel = getPreferredModelOwners(userModelNames, visCtx.ownerGroups)
	}
	userOpenAiModels := make([]dto.OpenAIModels, 0, len(userModelNames))
	for _, modelName := range userModelNames {
		userOpenAiModels = append(userOpenAiModels, buildOpenAIModel(modelName, ownerByModel))
	}

	switch modelType {
	case constant.ChannelTypeAnthropic:
		useranthropicModels := make([]dto.AnthropicModel, len(userOpenAiModels))
		for i, model := range userOpenAiModels {
			useranthropicModels[i] = toAnthropicModel(model)
		}
		firstID := ""
		lastID := ""
		if len(useranthropicModels) > 0 {
			firstID = useranthropicModels[0].ID
			lastID = useranthropicModels[len(useranthropicModels)-1].ID
		}
		c.JSON(200, gin.H{
			"data":     useranthropicModels,
			"first_id": firstID,
			"has_more": false,
			"last_id":  lastID,
		})
	case constant.ChannelTypeGemini:
		userGeminiModels := make([]dto.GeminiModel, len(userOpenAiModels))
		for i, model := range userOpenAiModels {
			userGeminiModels[i] = toGeminiModel(model)
		}
		c.JSON(200, gin.H{
			"models":        userGeminiModels,
			"nextPageToken": nil,
		})
	default:
		c.JSON(200, gin.H{
			"success": true,
			"data":    userOpenAiModels,
			"object":  "list",
		})
	}
}

func ChannelListModels(c *gin.Context) {
	c.JSON(200, gin.H{
		"success": true,
		"data":    openAIModels,
	})
}

func DashboardListModels(c *gin.Context) {
	modelsByChannel := make(map[int][]string, len(channelId2Models))
	for channelType, models := range channelId2Models {
		modelsByChannel[channelType] = append([]string(nil), models...)
	}
	for channelType := 1; channelType <= constant.ChannelTypeDummy; channelType++ {
		if plugin, ok := jsplugin.DefaultRegistry.GetByChannelType(channelType); ok {
			modelsByChannel[channelType] = append([]string(nil), plugin.Meta.Models...)
		}
	}
	c.JSON(200, gin.H{
		"success": true,
		"data":    modelsByChannel,
	})
}

func EnabledListModels(c *gin.Context) {
	c.JSON(200, gin.H{
		"success": true,
		"data":    model.GetEnabledModels(),
	})
}

func writeRetrieveModelError(c *gin.Context, modelType int, statusCode int, claudeType string, openAIType string, openAICode string, param string, message string) {
	switch modelType {
	case constant.ChannelTypeAnthropic:
		c.JSON(statusCode, gin.H{
			"type": "error",
			"error": types.ClaudeError{
				Type:    claudeType,
				Message: message,
			},
		})
	default:
		c.JSON(statusCode, gin.H{
			"error": types.OpenAIError{
				Message: message,
				Type:    openAIType,
				Param:   param,
				Code:    openAICode,
			},
		})
	}
}

func writeRetrieveModelNotFound(c *gin.Context, modelType int, modelId string) {
	writeRetrieveModelError(
		c,
		modelType,
		http.StatusNotFound,
		"not_found_error",
		"invalid_request_error",
		"model_not_found",
		"model",
		fmt.Sprintf("The model '%s' does not exist", modelId),
	)
}

func RetrieveModel(c *gin.Context, modelType int) {
	modelId := c.Param("model")
	// 路由用 catch-all 注册（模型 ID 可以带 `/`），Gin 会把前导斜杠一起
	// 捕获进参数：`/v1/models/hy4` 取到的是 `/hy4`。不剥掉这个前导斜杠，
	// 后续的模型存在性判断拿 "/hy4" 去比对，必然不命中，返回 404。
	modelId = strings.TrimPrefix(modelId, "/")
	if modelType == constant.ChannelTypeGemini {
		modelId = strings.TrimPrefix(modelId, "models/")
	}
	visCtx, err := resolveVisibleModelContext(c)
	if err != nil {
		writeRetrieveModelError(
			c,
			modelType,
			http.StatusInternalServerError,
			"api_error",
			string(types.ErrorTypeNewAPIError),
			"get_user_group_failed",
			"",
			"get user group failed",
		)
		return
	}

	if modelId == "" {
		writeRetrieveModelNotFound(c, modelType, modelId)
		return
	}

	models := service.GetGroupsEnabledModels(visCtx.ownerGroups)
	if !slices.Contains(models, modelId) || !visCtx.allowsModel(modelId) {
		writeRetrieveModelNotFound(c, modelType, modelId)
		return
	}

	ownerByModel := map[string]string{}
	if len(visCtx.ownerGroups) > 0 {
		ownerByModel = getPreferredModelOwners([]string{modelId}, visCtx.ownerGroups)
	}
	aiModel := buildOpenAIModel(modelId, ownerByModel)

	switch modelType {
	case constant.ChannelTypeAnthropic:
		c.JSON(http.StatusOK, toAnthropicModel(aiModel))
	case constant.ChannelTypeGemini:
		c.JSON(http.StatusOK, toGeminiModel(aiModel))
	default:
		c.JSON(http.StatusOK, aiModel)
	}
}
