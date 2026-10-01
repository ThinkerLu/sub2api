package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIWSTransportDiagnostic(t *testing.T) {
	require.Equal(t, "fail to write frame: broken pipe", openAIWSTransportDiagnostic("failed to write frame: broken pipe"))
	for _, message := range []string{
		"Bearer test-secret", "api_key=test-secret", "access_token=test-secret",
		"password: test-secret", "sk-test-secret", "eyJtest.payload.signature",
	} {
		t.Run(message, func(t *testing.T) {
			got := openAIWSTransportDiagnostic(message)
			require.NotContains(t, got, "test-secret")
			require.NotContains(t, got, "eyJtest")
		})
	}
	require.NotContains(t, openAIWSTransportDiagnostic("broken\npipe\r\n"), "\n")
	require.LessOrEqual(t, len(openAIWSTransportDiagnostic(strings.Repeat("x", 2048))), 1027)
}
