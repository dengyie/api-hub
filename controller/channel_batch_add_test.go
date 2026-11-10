package controller

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/dengyie/apihub/common"
	"github.com/dengyie/apihub/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddChannelBatchNamePrefixDoesNotAccumulate(t *testing.T) {
	setupTaskPluginBindChannelTest(t)

	// Windows CRLF line endings + 3 distinct keys.
	keys := "sk-11111111\r\nsk-22222222\r\nsk-33333333"
	body := fmt.Sprintf(`{
		"mode": "batch",
		"batch_add_set_key_prefix_2_name": true,
		"channel": {
			"type": 1,
			"name": "my-chan",
			"key": %q,
			"models": "gpt-4",
			"group": "default",
			"base_url": "https://api.openai.com"
		}
	}`, keys)

	rec := postAddChannel(t, 1, common.RoleRootUser, body)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"success":true`)

	var channels []model.Channel
	require.NoError(t, model.DB.Order("id asc").Find(&channels).Error)
	require.Len(t, channels, 3)

	assert.Equal(t, "my-chan sk-11111", channels[0].Name)
	assert.Equal(t, "sk-11111111", channels[0].Key)

	// If pointer aliasing was present, channel 1's prefix would have accumulated here.
	assert.Equal(t, "my-chan sk-22222", channels[1].Name)
	assert.Equal(t, "sk-22222222", channels[1].Key)

	assert.Equal(t, "my-chan sk-33333", channels[2].Name)
	assert.Equal(t, "sk-33333333", channels[2].Key)
}
