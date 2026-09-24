package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUsageTrafficMiddlewareCountsRequestAndResponseBodies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(UsageTrafficMiddleware())

	var snapshot service.UsageTraffic
	router.POST("/traffic", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		require.Equal(t, "request-body", string(body))
		c.String(http.StatusOK, "response-body")
		snapshot = service.UsageTrafficCounterFromContext(c.Request.Context()).Snapshot()
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/traffic", strings.NewReader("request-body"))
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, int64(len("request-body")), snapshot.IngressBodyBytes)
	require.Equal(t, int64(len("response-body")), snapshot.EgressBodyBytes)
	require.Zero(t, snapshot.UpstreamRequestBodyBytes)
	require.Zero(t, snapshot.UpstreamResponseBodyBytes)
}
