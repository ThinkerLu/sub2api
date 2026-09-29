package service

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Only an actual failed handshake proves no response.create was submitted.
// Pool capacity, authentication and quota failures are not fallback signals.
func (s *OpenAIGatewayService) canFallbackHTTPAfterWSHandshake(ctx context.Context, c *gin.Context, account *Account, body []byte, err error) bool {
	if ctx.Err() != nil || c == nil || GetOpenAIClientTransport(c) != OpenAIClientTransportHTTP ||
		c.GetBool(openAIHTTPWSWriteAttemptedKey) || c.Writer.Written() ||
		!s.IsOpenAIWSAdaptive(account) || !openAIWSHasCompatibleHTTP(account) {
		return false
	}
	var fallback *openAIWSFallbackError
	var dial *openAIWSDialError
	if !errors.As(err, &fallback) || fallback == nil || !errors.As(fallback.Err, &dial) || dial == nil ||
		fallback.Reason != classifyOpenAIWSAcquireError(dial) || errors.Is(dial.Err, context.Canceled) {
		return false
	}
	switch dial.StatusCode {
	case 0:
		if dial.Err == nil {
			return false
		}
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusUpgradeRequired,
		http.StatusInternalServerError, http.StatusNotImplemented, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
	default:
		return false
	}
	if isOpenAIWSRateLimitError(extractUpstreamErrorCode(dial.ResponseBody),
		gjson.GetBytes(dial.ResponseBody, "error.type").String(), extractUpstreamErrorMessage(dial.ResponseBody)) {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "" ||
		gjson.GetBytes(body, "generate").Type == gjson.False {
		return false
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() && input.Type != gjson.String {
		return false
	}
	coverage := AnalyzeToolCallOutputContextCoverageBytes(body)
	return !coverage.HasFunctionCallOutput || coverage.ContextCoversAllCallIDs
}

func writeOpenAIHTTPFallbackExecutionUnknown(c *gin.Context) error {
	if c != nil && !c.Writer.Written() {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
			"type": "upstream_error", "code": "upstream_execution_unknown",
			"message": "HTTP fallback failed after submission; automatic replay is disabled",
		}})
	}
	// Deliberately do not unwrap a failover error: the handler must not resubmit.
	return errors.New("HTTP fallback execution unknown; automatic replay is disabled")
}
