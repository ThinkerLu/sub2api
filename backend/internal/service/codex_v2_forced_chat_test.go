package service

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCodexV2ForcedChatCompletionsRejectionPredicate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	newCtx := func(groupID int64) *gin.Context {
		c, _ := newCodexV2BridgeTestContextWithRecorder(t, groupID)
		return c
	}

	// 探测确认不支持 Responses 的 OpenAI API Key 账号：实际路由为 forced CC 回退。
	probedFalse := codexV2BridgeOpenAIAccount()
	probedFalse.Extra[openai_compat.ExtraKeyResponsesSupported] = false
	rejection := codexV2ForcedChatCompletionsRejection(newCtx(77), probedFalse)
	require.Error(t, rejection)
	require.True(t, IsCodexV2GroupPolicyRejectionError(rejection))
	require.Equal(t, CodexV2ForcedChatCompletionsClientMessage, codexV2ClientPolicyErrorMessage(rejection))
	// 本地拒绝不得计入账号调度失败。
	require.False(t, (&OpenAIGatewayService{}).ReportOpenAIAccountScheduleResult(probedFalse, "gpt-5.6-sol", false, nil, rejection))

	// 账号级显式覆盖成 force_chat_completions：同样命中。
	forcedMode := codexV2BridgeOpenAIAccount()
	forcedMode.Extra[openai_compat.ExtraKeyResponsesMode] = string(openai_compat.ResponsesSupportModeForceChatCompletions)
	require.Error(t, codexV2ForcedChatCompletionsRejection(newCtx(77), forcedMode))

	// 关闭分组保持原回退行为。
	require.NoError(t, codexV2ForcedChatCompletionsRejection(newCtx(88), probedFalse))

	// 原生 Responses 账号不受影响。
	require.NoError(t, codexV2ForcedChatCompletionsRejection(newCtx(77), codexV2BridgeOpenAIAccount()))

	// 非 OpenAI 平台的 Chat-only 账号属于子端支持范围，不能被这个父端限制误伤。
	// 即便 extra 上带有同样的探测结果，也只按 platform 判定，不按 extra 判。
	childPlatform := &Account{
		ID: 9101, Platform: PlatformDeepseek, Type: AccountTypeAPIKey,
		Extra: map[string]any{
			"api_protocol":                           APIProtocolChatCompletions,
			openai_compat.ExtraKeyResponsesSupported: false,
		},
	}
	require.NoError(t, codexV2ForcedChatCompletionsRejection(newCtx(77), childPlatform))

	// 无 API Key 请求（拿不到分组）保持关闭。
	anonymous, _ := newCodexV2BridgeTestContextWithRecorder(t, 0)
	anonymous.Keys = nil
	require.NoError(t, codexV2ForcedChatCompletionsRejection(anonymous, probedFalse))
}

func TestCodexV2RequestPolicyErrorWritesCleanLocalResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c, recorder := newCodexV2BridgeTestContextWithRecorder(t, 77)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	WriteCodexV2RequestPolicyError(c, newCodexV2ForcedChatCompletionsRejectionError())

	require.True(t, IsResponseCommitted(c),
		"a local rejection must be marked committed so the handler does not append a response.failed frame")
	require.True(t, HasOpsClientBusinessLimited(c),
		"a local rejection must be attributed to the client side, not the upstream")
	// 本地拒绝把“当前上游错误”归因字段显式清成零值（MarkOpsLocalClientRejection），
	// 因此这里断言的是值而不是键是否存在。
	if value, exists := c.Get(OpsUpstreamStatusCodeKey); exists {
		if code, ok := value.(int); ok {
			require.Zero(t, code,
				"a local rejection must clear the current upstream status attribution")
		}
	}
	require.True(t, HasOpsLocalClientRejection(c),
		"a local rejection must be marked so a prior attempt's upstream context cannot decide the final attribution")
	body := recorder.Body.String()
	require.JSONEq(t, `{"error":{"type":"invalid_request_error","message":"`+CodexV2ForcedChatCompletionsClientMessage+`","param":"input"}}`, body)
	require.NotContains(t, body, "response.failed", "no SSE terminal frame may be appended to the local error")
}

func TestCodexV2ForwardRejectsForcedChatCompletionsBeforeUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	svc, upstream := newCodexV2BridgeTestService(t)
	account := codexV2BridgeOpenAIAccount()
	account.Extra[openai_compat.ExtraKeyResponsesSupported] = false
	c, recorder := newCodexV2BridgeTestContextWithRecorder(t, 77)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)

	_, forwardErr := svc.Forward(
		c.Request.Context(),
		c,
		account,
		[]byte(`{"model":"gpt-5.6-sol","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
	)
	require.Error(t, forwardErr)
	require.True(t, IsCodexV2GroupPolicyRejectionError(forwardErr),
		"forced Chat Completions must be rejected as a local policy error, got %v", forwardErr)
	require.Empty(t, upstream.bodies, "no upstream call may happen before the rejection")
	require.JSONEq(t, `{"error":{"type":"invalid_request_error","message":"`+CodexV2ForcedChatCompletionsClientMessage+`","param":"input"}}`,
		recorder.Body.String())
	require.False(t, codexV2ParentPlaintextAttemptActive(c),
		"no parent schema rewrite attempt may be registered on a rejected request")
}

func TestCodexV2ForwardKeepsDisabledGroupForcedChatCompletions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	svc, upstream := newCodexV2BridgeTestService(t)
	account := codexV2BridgeOpenAIAccount()
	account.Extra[openai_compat.ExtraKeyResponsesSupported] = false
	c, _ := newCodexV2BridgeTestContextWithRecorder(t, 88)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)

	// 关闭组必须继续走原来的 Responses→Chat Completions 回退：给回退上游一份
	// 正常的 chat completions 响应，证明请求真的到达了回退路径。
	upstream.responses = append(upstream.responses, &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"chatcmpl_1","object":"chat.completion","model":"gpt-5.6-sol","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)),
	})

	_, forwardErr := svc.Forward(
		c.Request.Context(),
		c,
		account,
		[]byte(`{"model":"gpt-5.6-sol","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
	)
	require.False(t, IsCodexV2GroupPolicyRejectionError(forwardErr),
		"disabled groups must keep the original fallback, got %v", forwardErr)
	require.NotEmpty(t, upstream.bodies,
		"disabled groups must still reach the Chat Completions fallback upstream")
}
