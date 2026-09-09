package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPassthroughIngressReportsFirstTokenForEveryTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	controlCtx, cancelControl := context.WithCancelCause(context.Background())
	defer cancelControl(context.Canceled)
	upstream := newStagedPassthroughConn()
	type completion struct {
		turn   int
		result *OpenAIForwardResult
		err    error
	}
	completed := make(chan completion, 4)
	hooks := &OpenAIWSIngressHooks{
		InitialTurnStartedAt: time.Now(),
		AfterTurn: func(turn int, result *OpenAIForwardResult, err error) {
			completed <- completion{turn, result, err}
		},
	}
	server, serverErr := startPassthroughHookRecordingServer(t, controlCtx,
		newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream),
		passthroughLifecycleAccount(), hooks)
	defer server.Close()
	client := dialPassthroughLifecycleClient(t, server)
	defer func() { _ = client.CloseNow() }()

	for turn := 1; turn <= 3; turn++ {
		if turn > 1 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := client.Write(ctx, coderws.MessageText, []byte(fmt.Sprintf(
				`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_ttft_%d","input":[{"role":"user","content":"continue"}]}`, turn-1)))
			cancel()
			require.NoError(t, err)
		}
		require.Equal(t, "response.create", gjson.GetBytes(requirePassthroughUpstreamWrite(t, upstream, time.Second), "type").String())
		upstream.Send(fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_ttft_%d"}}`, turn))
		upstream.Send(`{"type":"response.output_text.delta","delta":"hello"}`)
		upstream.Send(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_ttft_%d","model":"gpt-5.1","usage":{"input_tokens":2,"output_tokens":1}}}`, turn))
		for _, eventType := range []string{"response.created", "response.output_text.delta", "response.completed"} {
			frame, err := readPassthroughLifecycleFrame(t, client, time.Second)
			require.NoError(t, err)
			require.Equal(t, eventType, gjson.GetBytes(frame, "type").String())
		}
		select {
		case got := <-completed:
			require.NoError(t, got.err)
			require.Equal(t, turn, got.turn)
			require.NotNil(t, got.result)
			require.NotNil(t, got.result.FirstTokenMs, "turn %d TTFT must reach the usage-recording hook", turn)
			require.GreaterOrEqual(t, *got.result.FirstTokenMs, 0)
			require.LessOrEqual(t, int64(*got.result.FirstTokenMs), got.result.Duration.Milliseconds())
			require.True(t, got.result.OpenAIWSMode)
			require.Equal(t, 1, got.result.Usage.OutputTokens)
		case <-time.After(time.Second):
			t.Fatal("turn completion did not reach usage-recording hook")
		}
	}
	_ = client.CloseNow()
	cancelControl(context.Canceled)
	select {
	case <-serverErr:
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough ingress did not exit")
	}
}
