package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Codex V2 一期分组策略的本地拒绝必须走 ops_error_logger 的真实分类路径：
// 归因 client、businessLimited=true、phase 不是 upstream，且不写上游错误上下文。
// 同时确认本地 400 之后 handler 不会追加 response.failed 流内错误帧。
func TestWriteCodexV2RequestPolicyErrorOpsAttribution(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	err := service.ErrCodexV2ForcedChatCompletionsUnsupported
	service.WriteCodexV2RequestPolicyError(c, err)

	message := service.CodexV2ForcedChatCompletionsClientMessage
	require.NotEmpty(t, message)

	// 本地拒绝不得留下任何上游错误上下文，否则分类会落到 phase=upstream。
	require.False(t, hasOpsUpstreamErrorContext(c),
		"a local group-policy rejection must not carry upstream error context")
	require.True(t, service.HasOpsClientBusinessLimited(c),
		"a local group-policy rejection must be marked as client business limited")

	errType := normalizeOpsErrorType("invalid_request_error", message)
	phase, isBusinessLimited, errorOwner, errorSource := classifyOpsErrorLog(
		c, errType, message, "", http.StatusBadRequest,
	)
	require.NotEqual(t, "upstream", phase)
	require.True(t, isBusinessLimited)
	require.Equal(t, "client", errorOwner)
	require.Equal(t, "client_request", errorSource)

	// 已提交的本地 400 不得再被追加 SSE response.failed。
	h := &OpenAIGatewayHandler{}
	require.False(t, h.ensureForwardErrorResponse(c, false),
		"the handler must not append a fallback error after the local rejection")
	body := rec.Body.String()
	require.False(t, strings.Contains(body, "response.failed"),
		"no SSE response.failed frame may be appended to a local JSON error, got %q", body)
	require.False(t, strings.Contains(body, "event:"),
		"no SSE frame may be appended to a local JSON error, got %q", body)
	require.JSONEq(t, `{"error":{"type":"invalid_request_error","message":"`+message+`","param":"input"}}`, body)
}

// 同一次 HTTP 请求先有一次上游失败并换号，随后某一账号才命中本地策略拒绝：
// 最终归因必须取本地 client 侧，不能被先前尝试留下的上游错误上下文（状态码 +
// 累积事件）决定；先前尝试的诊断事件仍要保留在 ops 条目里。
func TestWriteCodexV2RequestPolicyErrorOpsAttributionAfterUpstreamFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	// 第 1 次尝试：上游 500，换号路径写入上游错误上下文与诊断事件。
	c.Set(service.OpsUpstreamStatusCodeKey, http.StatusInternalServerError)
	c.Set(service.OpsUpstreamErrorMessageKey, "upstream exploded")
	c.Set(service.OpsUpstreamErrorDetailKey, "upstream detail")
	c.Set(service.OpsUpstreamErrorsKey, []*service.OpsUpstreamErrorEvent{{
		Stage:              string(service.GatewayFailureStageInference),
		Scope:              string(service.GatewayFailureScopeRequest),
		Kind:               "failover",
		AccountID:          4242,
		UpstreamStatusCode: http.StatusInternalServerError,
		Message:            "upstream exploded",
	}})

	// 第 2 次尝试：本地策略拒绝。
	service.WriteCodexV2RequestPolicyError(c, service.ErrCodexV2ForcedChatCompletionsUnsupported)

	message := service.CodexV2ForcedChatCompletionsClientMessage
	errType := normalizeOpsErrorType("invalid_request_error", message)
	phase, isBusinessLimited, errorOwner, errorSource := classifyOpsErrorLog(
		c, errType, message, "", http.StatusBadRequest,
	)
	require.NotEqual(t, "upstream", phase,
		"a prior attempt's upstream failure must not decide the final classification")
	require.True(t, isBusinessLimited)
	require.Equal(t, "client", errorOwner)
	require.Equal(t, "client_request", errorSource)

	// 条目级：单次上游归因字段被清空，按尝试累积的诊断事件保留。
	entry := &service.OpsInsertErrorLogInput{}
	applyOpsUpstreamFieldsFromContext(c, entry)
	suppressOpsUpstreamAttributionForLocalRejection(c, entry)
	require.Nil(t, entry.UpstreamStatusCode,
		"the final entry must not carry the prior attempt's upstream status code")
	require.Nil(t, entry.UpstreamErrorMessage)
	require.Nil(t, entry.UpstreamErrorDetail)
	require.Len(t, entry.UpstreamErrors, 1,
		"the prior attempt's diagnostic event must be preserved")
	require.Equal(t, http.StatusInternalServerError, entry.UpstreamErrors[0].UpstreamStatusCode)
	require.Equal(t, "upstream exploded", entry.UpstreamErrors[0].Message)

	require.False(t, strings.Contains(rec.Body.String(), "response.failed"))
	require.False(t, strings.Contains(rec.Body.String(), "event:"))
}
