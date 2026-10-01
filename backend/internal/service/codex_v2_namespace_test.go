package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexV2NamespaceReservedSchemaUsesAlias(t *testing.T) {
	for _, carrier := range []string{"top", "lite"} {
		t.Run(carrier, func(t *testing.T) {
			body := []byte(codexV2ParentToolsFixture)
			path := "tools.0"
			if carrier == "lite" {
				body = []byte(`{"input":[{"type":"additional_tools","tools":` + gjson.GetBytes(body, "tools").Raw + `}]}`)
				path = "input.0.tools.0"
			}
			out, changed, err := rewriteCodexV2ParentPlaintextToolSchema(body)
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, "sub2api_collaboration", gjson.GetBytes(out, path+".name").String())
			require.False(t, gjson.GetBytes(out, path+".tools.2.parameters.properties.message.encrypted").Exists())
		})
	}
}

func TestCodexV2NamespaceAllPlatformsUsePlaintext(t *testing.T) {
	policy, err := NewCodexV2GroupPolicyFromBytes([]byte("version: 1\ngroups:\n  \"77\":\n    enabled: true\n"))
	require.NoError(t, err)
	t.Cleanup(SetCurrentCodexV2GroupPolicyForTest(policy))
	for _, platform := range []string{PlatformOpenAI, PlatformDeepseek} {
		t.Run(platform, func(t *testing.T) {
			c := newCodexV2BridgeTestContext(t, 77)
			body := []byte(`{"tools":` + gjson.Get(codexV2ParentToolsFixture, "tools").Raw + `,"input":[` +
				`{"type":"agent_message","content":[{"type":"input_text","text":"Payload:"},{"type":"encrypted_content","encrypted_content":"Read the small fixture."}]},` +
				`{"type":"function_call","name":"followup_task","namespace":"collaboration","arguments":"{\"message\":\"read the second fixture\"}"}]}`)
			out, err := applyCodexV2UpstreamRequestPoliciesForAttempt(c, &Account{Platform: platform}, body)
			require.NoError(t, err)
			require.True(t, out.ParentPlaintextSchema)
			require.Equal(t, "sub2api_collaboration", gjson.GetBytes(out.Body, "tools.0.name").String())
			require.Equal(t, "message", gjson.GetBytes(out.Body, "input.0.type").String())
			require.Equal(t, "sub2api_collaboration", gjson.GetBytes(out.Body, "input.1.namespace").String())
		})
	}
}

func TestCodexV2NamespaceResponseRestoresIdentity(t *testing.T) {
	c := newCodexV2BridgeTestContext(t, 77)
	markCodexV2ParentPlaintextAttempt(c)
	for _, tool := range []string{"spawn_agent", "followup_task", "wait_agent", "list_agents"} {
		for _, flat := range []bool{false, true} {
			name, ns := tool, `,"namespace":"sub2api_collaboration"`
			if flat {
				name, ns = "sub2api_collaboration__"+tool, ""
			}
			body := []byte(`{"type":"response.output_item.done","item":{"type":"function_call","name":"` + name + `"` + ns + `,"arguments":"{\"message\":\"read the fixture\"}"}}`)
			out := applyCodexV2ParentResponsePolicy(c, body)
			require.Equal(t, "collaboration", gjson.GetBytes(out, "item.namespace").String())
			require.Equal(t, tool, gjson.GetBytes(out, "item.name").String())
			require.Equal(t, codexV2IsCollaborationMessageTool(tool), gjson.GetBytes(out, "item.encrypted_function_args").Exists())
		}
	}
	empty := []byte(`{"item":{"type":"function_call","name":"spawn_agent","namespace":"sub2api_collaboration","arguments":""}}`)
	require.False(t, gjson.GetBytes(applyCodexV2ParentResponsePolicy(c, empty), "item.encrypted_function_args").Exists())
	cipher := []byte(`{"item":{"type":"function_call","name":"spawn_agent","namespace":"sub2api_collaboration","arguments":"{\"message\":\"gAAAA-invalid-cipher\"}"}}`)
	require.False(t, gjson.GetBytes(applyCodexV2ParentResponsePolicy(c, cipher), "item.encrypted_function_args").Exists())
}

func TestCodexV2NamespaceAliasCollisionRejected(t *testing.T) {
	for _, injected := range []string{
		`{"type":"namespace","name":"sub2api_collaboration","tools":[]}`,
		`{"type":"function","name":"sub2api_collaboration__spawn_agent"}`,
	} {
		body := []byte(`{"tools":[` + injected + `,` + gjson.Get(codexV2ParentToolsFixture, "tools.0").Raw + `]}`)
		_, _, err := rewriteCodexV2ParentPlaintextToolSchema(body)
		require.Error(t, err)
	}
	body := []byte(`{"input":[{"type":"function_call","name":"spawn_agent","namespace":"sub2api_collaboration"}],"tools":` + gjson.Get(codexV2ParentToolsFixture, "tools").Raw + `}`)
	_, _, err := rewriteCodexV2ParentPlaintextToolSchema(body)
	require.Error(t, err)
}
