package service

// Diagnostic-only overlay. Keep the production websocket implementation and
// OAuth path; redirect only the upstream dial target to a loopback test server.
import (
	"context"
	"net/http"
)

type diagnosticWSDialer struct {
	target string
	inner  openAIWSClientDialer
}

func (d *diagnosticWSDialer) Dial(ctx context.Context, _ string, headers http.Header, _ string) (openAIWSClientConn, int, http.Header, error) {
	return d.inner.Dial(ctx, d.target, headers, "")
}
func DiagnosticWSRedirectUpstream(s *OpenAIGatewayService, target string) {
	s.openaiWSPassthroughDialer = &diagnosticWSDialer{target: target, inner: newDefaultOpenAIWSClientDialer()}
}
