package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dengyie/apihub/common"
	"github.com/dengyie/apihub/constant"
	"github.com/dengyie/apihub/middleware"
	"github.com/dengyie/apihub/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVideoProxyNativeTaskAdaptorServesAuthedMedia covers the halo-style
// native task adaptor flow: the task carries no plugin execution, the stored
// result URL is our own gateway-relative content path, and the media lives
// behind upstream Bearer auth. VideoProxy must route through the platform's
// TaskContentRequestProvider (which injects the channel key) instead of the
// credentialless fallback — the old behavior returned 410 artifact_gone here.
func TestVideoProxyNativeTaskAdaptorServesAuthedMedia(t *testing.T) {
	gin.SetMode(gin.TestMode)
	task := setupGenericTaskTest(t)

	var upstreamAuth, upstreamRange string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth = r.Header.Get("Authorization")
		upstreamRange = r.Header.Get("Range")
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("halo-bytes"))
	}))
	defer upstream.Close()
	allowPrivateTaskMediaTest(t)
	previousMemoryCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemoryCache })

	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", task.ChannelId).Updates(map[string]any{
		"type":     constant.ChannelTypeHalo,
		"key":      "vgn_channel_key",
		"base_url": upstream.URL,
	}).Error)
	task.Platform = constant.TaskPlatform("67")
	task.Action = constant.TaskActionTextToVideo
	// What ParseTaskResult persisted on success: the gateway-relative proxy
	// URL (never the authed upstream path).
	task.PrivateData.ResultURL = "/v1/videos/" + task.TaskID + "/content"
	task.PrivateData.PluginState = []byte(`{"conversation_id":"conv-1","job_id":"job-9","media_path":"/api/v1/jobs/job-9/media?download=1"}`)
	require.NoError(t, model.DB.Save(task).Error)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(middleware.TaskArtifactAccessContextKey, true)
	c.Params = gin.Params{{Key: "task_id", Value: task.TaskID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/"+task.TaskID+"/content", nil)
	c.Request.Header.Set("Range", "bytes=0-4")

	VideoProxy(c)

	require.Equal(t, http.StatusOK, recorder.Code, "body: %s", recorder.Body.String())
	assert.Equal(t, "halo-bytes", recorder.Body.String())
	assert.Equal(t, "Bearer vgn_channel_key", upstreamAuth)
	assert.Equal(t, "bytes=0-4", upstreamRange)
}

// TestVideoProxyNativeAdaptorMissingKeyFailsClosed verifies that when neither
// the task private key nor the channel key is available, the native provider
// path refuses to build a credentialless request (no unauthed media fetch).
func TestVideoProxyNativeAdaptorMissingKeyFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	task := setupGenericTaskTest(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called without credentials")
	}))
	defer upstream.Close()
	allowPrivateTaskMediaTest(t)
	previousMemoryCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemoryCache })

	// Channel with an empty key.
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", task.ChannelId).Updates(map[string]any{
		"type": constant.ChannelTypeHalo,
		"key":  "",
	}).Error)
	task.Platform = constant.TaskPlatform("67")
	task.Action = constant.TaskActionTextToVideo
	task.PrivateData.ResultURL = "/v1/videos/" + task.TaskID + "/content"
	task.PrivateData.PluginState = []byte(`{"job_id":"job-9"}`)
	require.NoError(t, model.DB.Save(task).Error)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(middleware.TaskArtifactAccessContextKey, true)
	c.Params = gin.Params{{Key: "task_id", Value: task.TaskID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/"+task.TaskID+"/content", nil)

	VideoProxy(c)

	// Under artifact-access auth the gateway masks all failures as 404 to
	// avoid leaking task existence; what matters is that no bytes were served
	// and no unauthenticated upstream request was made.
	assert.Equal(t, http.StatusNotFound, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "halo-bytes")
}
