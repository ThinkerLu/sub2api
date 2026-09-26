package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestWSMemoryBoundedReaderRejectsGrowthAndReleases(t *testing.T) {
	g := &openAIWSMemoryGovernor{limit: 300 << 10, perAccount: 300 << 10, accounts: make(map[int64]int64)}
	result := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			result <- err
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(1 << 20)
		session := &openAIWSMemorySession{g: g}
		openAIWSMemorySessions.Store(conn, session)
		_, _, err = ReadOpenAIWSClientMessage(r.Context(), conn, time.Second, coderws.StatusPolicyViolation, "idle")
		openAIWSMemorySessions.Delete(conn)
		session.close()
		result <- err
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()
	_ = client.Write(ctx, coderws.MessageText, []byte(strings.Repeat("x", 128<<10)))
	select {
	case err := <-result:
		require.ErrorIs(t, err, errOpenAIWSMemoryBudget)
	case <-ctx.Done():
		t.Fatal("bounded read did not reject")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	require.Zero(t, g.reserved)
}

func TestWSMemoryGovernorAtomicallyEnforcesAccountAndProcessLimits(t *testing.T) {
	g := &openAIWSMemoryGovernor{limit: 160, perAccount: 80, accounts: make(map[int64]int64)}
	a, b := &openAIWSMemorySession{g: g}, &openAIWSMemorySession{g: g}
	require.NoError(t, a.bindAccount(1))
	require.NoError(t, b.bindAccount(1))
	require.NoError(t, a.growWorking(8))
	require.ErrorIs(t, b.growWorking(3), errOpenAIWSMemoryBudget)
	require.EqualValues(t, 64, g.reserved)
	require.NoError(t, b.bindAccount(2))
	require.NoError(t, b.growWorking(10))
	require.ErrorIs(t, a.growWorking(3), errOpenAIWSMemoryBudget)
	require.EqualValues(t, 144, g.reserved)
	require.ErrorIs(t, b.bindAccount(1), errOpenAIWSMemoryBudget)
	require.EqualValues(t, 2, b.account)
	require.NoError(t, a.finishTurn(1))
	require.EqualValues(t, 88, g.reserved)
	a.close()
	a.close()
	b.close()
	require.Zero(t, g.reserved)
	require.Empty(t, g.accounts)
	require.ErrorIs(t, a.growWorking(1), errOpenAIWSMemoryBudget)
}

func TestWSMemoryGovernorConcurrentGrowthNeverOversubscribes(t *testing.T) {
	g := &openAIWSMemoryGovernor{limit: 80, perAccount: 80, accounts: make(map[int64]int64)}
	s := &openAIWSMemorySession{g: g}
	require.NoError(t, s.bindAccount(1))
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.growWorking(1) == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 10, accepted.Load())
	require.EqualValues(t, 80, g.reserved)
	s.close()
	require.Zero(t, g.reserved)
}

func TestDualLegMemoryRejectionDoesNotStartEitherUpstream(t *testing.T) {
	s, account, ws, httpUp := adaptiveFixture()
	s.cfg.Gateway.OpenAIWS.DualLegEnabled = true
	s.cfg.Gateway.OpenAIWS.DualLegMemoryBudgetBytes = 1 << 20
	s.cfg.Gateway.OpenAIWS.DualLegAccountMemoryBudgetBytes = 1 << 20
	server, errs := startAdaptiveServer(t, s, account, nil)
	client := dialAdaptive(t, server)
	writeAdaptive(t, client, `{"type":"response.create","model":"gpt-5.1","input":"small"}`)
	select {
	case err := <-errs:
		require.ErrorIs(t, err, errOpenAIWSMemoryBudget)
	case <-time.After(3 * time.Second):
		t.Fatal("memory rejection did not end the turn")
	}
	require.Empty(t, httpUp.snapshot())
	require.Empty(t, ws.writes)
	s.openaiWSMemory.mu.Lock()
	require.Zero(t, s.openaiWSMemory.reserved)
	s.openaiWSMemory.mu.Unlock()
}
