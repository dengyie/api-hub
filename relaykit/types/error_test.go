package types

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type customTestError struct {
	Code int
}

func (e *customTestError) Error() string {
	return "custom test error"
}

func TestNewOpenAIError_PreservesUnderlyingError(t *testing.T) {
	orig := &customTestError{Code: 42}
	apiErr := NewOpenAIError(orig, ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	require.NotNil(t, apiErr)

	var target *customTestError
	assert.True(t, errors.As(apiErr, &target), "errors.As should find underlying customTestError")
	assert.Equal(t, 42, target.Code)
	assert.True(t, errors.Is(apiErr, orig), "errors.Is should match original error")
}

func TestWithUpstreamErrorsPreserveEmptyTypeWhileDefaultingSerializedOutput(t *testing.T) {
	oaiErr := WithOpenAIError(OpenAIError{Message: "validation error"}, http.StatusUnprocessableEntity)
	require.NotNil(t, oaiErr)
	rawOAI, ok := oaiErr.RelayError.(OpenAIError)
	require.True(t, ok)
	assert.Empty(t, rawOAI.Type, "WithOpenAIError must not synthesize upstream_error into RelayError.Type")
	assert.Equal(t, ErrorCode("unknown_error"), oaiErr.GetErrorCode())
	assert.Equal(t, string(ErrorTypeUpstreamError), oaiErr.ToOpenAIError().Type)

	claudeErr := WithClaudeError(ClaudeError{Message: "invalid input"}, http.StatusBadRequest)
	require.NotNil(t, claudeErr)
	rawClaude, ok := claudeErr.RelayError.(ClaudeError)
	require.True(t, ok)
	assert.Empty(t, rawClaude.Type, "WithClaudeError must not synthesize upstream_error into RelayError.Type")
	assert.Equal(t, ErrorCode(ErrorTypeUpstreamError), claudeErr.GetErrorCode())
	assert.Equal(t, string(ErrorTypeUpstreamError), claudeErr.ToClaudeError().Type)
	assert.Equal(t, string(ErrorTypeUpstreamError), claudeErr.ToOpenAIError().Type)
}
