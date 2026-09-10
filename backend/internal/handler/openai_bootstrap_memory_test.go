package handler

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestBootstrapImageFastPathAndEscapedCandidate(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`)
	got, changed := normalizeCodexDelegationBootstrap(body)
	require.False(t, changed)
	require.Equal(t, &body[0], &got[0])
	candidate := []byte(`{"input":[{"type":"function_call_outp\u0075t","namespace":"codex_app","name":"create_thread","output":"` + delegationEnvelope + `"}]}`)
	_, changed = normalizeCodexDelegationBootstrap(candidate)
	require.True(t, changed)
}

func BenchmarkBootstrapImageContext(b *testing.B) {
	body := append([]byte(`{"input":[{"type":"message","content":[{"type":"input_image","image_url":"`), bytes.Repeat([]byte("A"), 8<<20)...)
	body = append(body, []byte(`"}]}]}`)...)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		normalizeCodexDelegationBootstrap(body)
		normalizeCodexAutomationBootstrap(body)
	}
}
