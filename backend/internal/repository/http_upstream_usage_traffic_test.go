package repository

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/stretchr/testify/require"
)

func TestHTTPUpstreamUsageTrafficTracking(t *testing.T) {
	counter := service.NewUsageTrafficCounter()
	ctx := service.WithUsageTrafficCounter(t.Context(), counter)
	req := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", strings.NewReader("request"))
	req = req.WithContext(ctx)

	trackUsageTrafficRequest(req)
	_, err := io.ReadAll(req.Body)
	require.NoError(t, err)

	resp := &http.Response{
		Body:    io.NopCloser(strings.NewReader("response")),
		Request: req,
	}
	trackUsageTrafficResponse(resp)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)

	got := counter.Snapshot()
	require.Equal(t, int64(len("request")), got.UpstreamRequestBodyBytes)
	require.Equal(t, int64(len("response")), got.UpstreamResponseBodyBytes)
}
