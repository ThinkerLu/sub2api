package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexV2NativeWSReady(t *testing.T) {
	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	t.Cleanup(SetCurrentCodexV2GroupPolicyForTest(policy))
	require.NoError(t, codexV2WSv2GuardError(newCodexV2BridgeTestContext(t, 77), false))
}

func TestCodexV2NativeWSFrameRoundTripAndInheritance(t *testing.T) {
	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	t.Cleanup(SetCurrentCodexV2GroupPolicyForTest(policy))
	for _, lite := range []bool{false, true} {
		c := newCodexV2BridgeTestContext(t, 77)
		p := newCodexV2NativeWSPolicy(c)
		body := `{"type":"response.create",` + codexV2BridgeToolsDeclaration + `,"input":[]}`
		path := "tools.0.name"
		if lite {
			body = `{"type":"response.create","input":[` + codexV2BridgeLiteCollaborationDeclaration + `]}`
			path = "input.0.tools.0.name"
		}
		first, err := p.Prepare([]byte(body))
		require.NoError(t, err)
		require.Equal(t, codexV2UpstreamNamespace, gjson.GetBytes(first, path).String())
		require.NotContains(t, string(first), `"encrypted"`)
		second, err := p.Prepare([]byte(`{"type":"response.create","previous_response_id":"resp_1","input":[{"type":"function_call","name":"spawn_agent","namespace":"collaboration","arguments":"{}"}]}`))
		require.NoError(t, err)
		require.Contains(t, string(second), "sub2api_collaboration")
		require.NotContains(t, string(second), `"namespace":"collaboration"`)
		state, _ := openAIWSHTTPBridgeToolStateFromContext(c)
		require.Contains(t, string(state.ClientTools)+string(state.ClientAdditionalTools), `"encrypted":true`)
		response := []byte(`{"item":{"type":"function_call","namespace":"sub2api_collaboration","name":"spawn_agent","arguments":"{\"message\":\"read the fixture\"}"}}`)
		out := p.Restore(response)
		require.Equal(t, "collaboration", gjson.GetBytes(out, "item.namespace").String())
		require.Equal(t, "[]", gjson.GetBytes(out, "item.encrypted_function_args").Raw)
		_, err = p.Prepare([]byte(`{"type":"response.create","tools":[],"input":[]}`))
		require.NoError(t, err)
		require.Equal(t, response, p.Restore(response))
		_, err = p.Prepare([]byte(`{"type":"response.create","input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"gAAAA-real-cipher"}]}]}`))
		require.Error(t, err)
		cancel := []byte(`{"type":"response.cancel"}`)
		out, err = p.Prepare(cancel)
		require.NoError(t, err)
		require.Equal(t, cancel, out)
	}
}

func TestCodexV2NativeWSDisabledGroupByteIdentity(t *testing.T) {
	p := newCodexV2NativeWSPolicy(newCodexV2BridgeTestContext(t, 88))
	body := []byte(`{"type":"response.create",` + codexV2BridgeToolsDeclaration + `,"input":[]}`)
	out, err := p.Prepare(body)
	require.NoError(t, err)
	require.Equal(t, body, out)
}
