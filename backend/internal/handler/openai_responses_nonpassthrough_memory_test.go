//go:build !race

package handler

import (
	"bytes"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type nonPassthroughMemoryUpstream struct {
	bytes  int64
	digest [32]byte
}

func (u *nonPassthroughMemoryUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	hash := sha256.New()
	n, err := io.Copy(hash, req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	u.bytes = n
	copy(u.digest[:], hash.Sum(nil))
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_memory\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
				"data: [DONE]\n\n",
		)),
	}, nil
}

func (u *nonPassthroughMemoryUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func TestOpenAIResponsesNonPassthroughLargeInputAllocation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.4","instructions":"memory test","stream":true,"store":false,"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("A", 8<<20) + `"}]}]}`)
	wantDigest := sha256.Sum256(body)
	account := service.Account{
		ID:          1,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "synthetic", "chatgpt_account_id": "synthetic"},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	upstream := &nonPassthroughMemoryUpstream{}
	billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	svc := service.NewOpenAIGatewayService(
		&openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{account}},
		nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, billing, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
	)
	h := NewOpenAIGatewayHandler(svc, service.NewConcurrencyService(nil), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("session_id", "synthetic-session")
	groupID := int64(1)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      1,
		GroupID: &groupID,
		User:    &service.User{ID: 2, Status: service.StatusActive},
		Group:   &service.Group{ID: 1, Platform: service.PlatformOpenAI, Status: service.StatusActive},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 2, Concurrency: 0})

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	h.Responses(c)
	runtime.ReadMemStats(&after)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, int64(len(body)), upstream.bytes)
	require.Equal(t, wantDigest, upstream.digest)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("body=%d allocated=%d ratio=%.3f", len(body), allocated, float64(allocated)/float64(len(body)))
	require.Less(t, allocated, uint64(3*len(body)), "non-passthrough Responses should avoid whole-body decode/encode for a no-op transform")
}
