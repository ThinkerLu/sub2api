package service

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const codexV2ChildPlaintextEnvelope = "FIXTURE_ENVELOPE parent=/root child=/root/task"
const codexV2ChildPlaintextPayload = "FIXTURE_PAYLOAD run the focused tests"

func codexV2ChildBodyWithAgentMessage(content string) string {
	return `{"model":"deepseek-flash","stream":true,"previous_response_id":null,"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"existing"}]},` +
		`{"type":"agent_message","id":"amsg_1","author":"/root","recipient":"/root/task","content":[` + content + `],` +
		`"internal_chat_message_metadata_passthrough":{"turn_id":"turn_1","create_time":1759000000}},` +
		`{"type":"reasoning","id":"rs_1","summary":[]}]}`
}

func TestRewriteCodexV2ChildAgentMessagesPlaintext(t *testing.T) {
	t.Parallel()

	body := codexV2ChildBodyWithAgentMessage(
		`{"type":"input_text","text":"` + codexV2ChildPlaintextEnvelope + `"},` +
			`{"type":"encrypted_content","encrypted_content":"` + codexV2ChildPlaintextPayload + `"}`,
	)
	rewritten, changed, err := rewriteCodexV2ChildAgentMessages([]byte(body))
	require.NoError(t, err)
	require.True(t, changed)

	item := gjson.GetBytes(rewritten, "input.1")
	require.Equal(t, "message", item.Get("type").String())
	require.Equal(t, "user", item.Get("role").String())
	require.Equal(t, codexV2ChildPlaintextEnvelope, item.Get("content.0.text").String())
	require.Equal(t, codexV2ChildPlaintextPayload, item.Get("content.1.text").String())
	require.Equal(t, "input_text", item.Get("content.0.type").String())
	require.Equal(t, "input_text", item.Get("content.1.type").String())
	require.False(t, item.Get("id").Exists(), "the agent_message envelope id must not leak into a message item")
	require.False(t, item.Get("author").Exists())
	require.False(t, item.Get("internal_chat_message_metadata_passthrough").Exists())

	require.Equal(t, "existing", gjson.GetBytes(rewritten, "input.0.content.0.text").String())
	require.Equal(t, "reasoning", gjson.GetBytes(rewritten, "input.2.type").String())
	require.Equal(t, "deepseek-flash", gjson.GetBytes(rewritten, "model").String())
	require.True(t, gjson.ValidBytes(rewritten))
	require.NotContains(t, string(rewritten), "agent_message")
}

func TestRewriteCodexV2ChildAgentMessagesPreservesUnrelatedAgentMessageShapes(t *testing.T) {
	t.Parallel()

	// 字符串 content 的明文 agent_message 也按原始片段顺序转成普通 user 消息。
	body := `{"model":"deepseek-flash","input":[{"type":"agent_message","content":"plain task text"}]}`
	rewritten, changed, err := rewriteCodexV2ChildAgentMessages([]byte(body))
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "user", gjson.GetBytes(rewritten, "input.0.role").String())
	require.Equal(t, "plain task text", gjson.GetBytes(rewritten, "input.0.content.0.text").String())

	// 正文里只是出现 agent_message 字样时不得改写。
	mentioned := `{"model":"deepseek-flash","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"discuss agent_message handling"}]}]}`
	rewritten, changed, err = rewriteCodexV2ChildAgentMessages([]byte(mentioned))
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, mentioned, string(rewritten))
}

func TestRewriteCodexV2ChildAgentMessagesFailClosed(t *testing.T) {
	t.Parallel()

	ciphertext := "gAAAAABq0mQ1eTk9U2VjcmV0UGF5bG9hZA" + strings.Repeat("Ab3xQ9", 6)
	longBase64 := strings.Repeat("Zm9vQmFyQmF6", 12)

	tests := []struct {
		name   string
		body   string
		reason string
	}{
		{
			name: "fernet_ciphertext",
			body: codexV2ChildBodyWithAgentMessage(
				`{"type":"input_text","text":"` + codexV2ChildPlaintextEnvelope + `"},` +
					`{"type":"encrypted_content","encrypted_content":"` + ciphertext + `"}`,
			),
			reason: "real ciphertext",
		},
		{
			name: "long_base64_payload",
			body: codexV2ChildBodyWithAgentMessage(
				`{"type":"encrypted_content","encrypted_content":"` + longBase64 + `"}`,
			),
			reason: "real ciphertext",
		},
		{
			name:   "empty_content",
			body:   codexV2ChildBodyWithAgentMessage(`{"type":"input_text","text":""}`),
			reason: "empty text part",
		},
		{
			name:   "empty_encrypted_content",
			body:   codexV2ChildBodyWithAgentMessage(`{"type":"encrypted_content","encrypted_content":""}`),
			reason: "empty encrypted_content part",
		},
		{
			name: "unsupported_part",
			body: codexV2ChildBodyWithAgentMessage(
				`{"type":"input_text","text":"` + codexV2ChildPlaintextEnvelope + `"},` +
					`{"type":"input_image","image_url":"https://example.invalid/a.png"}`,
			),
			reason: "unsupported part type",
		},
		{
			name:   "no_content",
			body:   `{"model":"deepseek-flash","input":[{"type":"agent_message","id":"amsg_2"}]}`,
			reason: "empty or has an unsupported shape",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rewritten, changed, err := rewriteCodexV2ChildAgentMessages([]byte(tt.body))
			require.Error(t, err)
			require.False(t, changed)
			require.Equal(t, tt.body, string(rewritten), "fail-closed must keep the original body")

			var policyErr *codexV2ChildMessagePolicyError
			require.True(t, errors.As(err, &policyErr), "error must be a policy error, got %T", err)
			require.Contains(t, err.Error(), tt.reason)

			// 失败日志/错误不得泄露正文或密文。
			require.NotContains(t, err.Error(), ciphertext)
			require.NotContains(t, err.Error(), codexV2ChildPlaintextEnvelope)
			require.NotContains(t, err.Error(), "FIXTURE_PAYLOAD")
			require.NotContains(t, err.Error(), longBase64)
		})
	}
}

func TestApplyCodexV2UpstreamRequestPoliciesChildSide(t *testing.T) {
	gin.SetMode(gin.TestMode)

	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"12\":\n    enabled: true\n"))
	require.NoError(t, err)
	restore := SetCurrentCodexV2GroupPolicyForTest(policy)
	t.Cleanup(restore)

	groupID := int64(12)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("api_key", &APIKey{ID: 1, GroupID: &groupID})

	deepSeekAccount := &Account{ID: 2, Platform: PlatformDeepseek, Type: AccountTypeAPIKey}
	body := codexV2ChildBodyWithAgentMessage(
		`{"type":"input_text","text":"` + codexV2ChildPlaintextEnvelope + `"},` +
			`{"type":"encrypted_content","encrypted_content":"` + codexV2ChildPlaintextPayload + `"}`,
	)
	policied, err := applyCodexV2UpstreamRequestPolicies(c, deepSeekAccount, []byte(body))
	require.NoError(t, err)
	require.Equal(t, "user", gjson.GetBytes(policied, "input.1.role").String())
	require.False(t, codexV2ParentPlaintextAttemptActive(c))

	// OpenAI 父/子端同样接收普通明文消息，不能把外来明文交给解密路径。
	openAIAccount := &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	parentBody := codexV2ChildBodyWithAgentMessage(`{"type":"input_text","text":"hello"}`)
	policied, err = applyCodexV2UpstreamRequestPolicies(c, openAIAccount, []byte(parentBody))
	require.NoError(t, err)
	require.Equal(t, "message", gjson.GetBytes(policied, "input.1.type").String())

	// 子端密文：非 OpenAI 上游必须 fail-closed，且错误文案不含正文。
	ciphertext := "gAAAAABq0mQ1eTk9U2VjcmV0UGF5bG9hZA" + strings.Repeat("Zz9K2m", 8)
	cipherBody := codexV2ChildBodyWithAgentMessage(`{"type":"encrypted_content","encrypted_content":"` + ciphertext + `"}`)
	_, err = applyCodexV2UpstreamRequestPolicies(c, deepSeekAccount, []byte(cipherBody))
	require.Error(t, err)
	require.Equal(t, codexV2ChildPolicyClientMessage, codexV2ClientPolicyErrorMessage(err))
	require.NotContains(t, codexV2ClientPolicyErrorMessage(err), ciphertext)

	// 未启用分组保持原样。
	otherGroup := int64(34)
	disabledCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	disabledCtx.Set("api_key", &APIKey{ID: 4, GroupID: &otherGroup})
	policied, err = applyCodexV2UpstreamRequestPolicies(disabledCtx, deepSeekAccount, []byte(cipherBody))
	require.NoError(t, err)
	require.Equal(t, cipherBody, string(policied))
}
