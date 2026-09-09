package openai_ws_v2

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestRelay_FirstTokenMs_PerTurnWithoutResponseID(t *testing.T) {
	t.Parallel()

	for _, eventType := range []string{
		"response.output_text.delta",
		"response.function_call_arguments.delta",
		"response.reasoning_summary_text.delta",
		"response.output_text.done",
		"response.function_call_arguments.done",
	} {
		t.Run(eventType, func(t *testing.T) {
			t.Parallel()
			// An empty warmup must not create a first-token sample. Subsequent
			// generations share the socket, and their output frames omit response_id.
			frames := []passthroughTestFrame{
				{msgType: coderws.MessageText, payload: []byte(`{"type":"response.created","response":{"id":"resp_warmup"}}`)},
				{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"resp_warmup","usage":{"input_tokens":2,"output_tokens":0}}}`)},
			}
			for turn := 1; turn <= 3; turn++ {
				frames = append(frames,
					passthroughTestFrame{msgType: coderws.MessageText, payload: []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_%d"}}`, turn))},
					passthroughTestFrame{msgType: coderws.MessageText, payload: []byte(fmt.Sprintf(`{"type":%q,"delta":"hello","text":"hello","arguments":"{}"}`, eventType))},
					passthroughTestFrame{msgType: coderws.MessageText, payload: []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","usage":{"input_tokens":2,"output_tokens":1}}}`, turn))},
				)
			}
			client := newPassthroughTestFrameConn(nil, false)
			upstream := newPassthroughTestFrameConn(frames, true)
			base := time.Unix(100, 0)
			var tick atomic.Int64
			var turns []RelayTurnResult
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			result, relayExit := Relay(ctx, client, upstream,
				[]byte(`{"type":"response.create","model":"gpt-5.3-codex","input":[]}`), RelayOptions{
					Now:            func() time.Time { return base.Add(time.Duration(tick.Add(1)) * 10 * time.Millisecond) },
					OnTurnComplete: func(turn RelayTurnResult) { turns = append(turns, turn) },
				})
			require.Nil(t, relayExit)
			require.Len(t, turns, 4)
			require.Nil(t, turns[0].FirstTokenMs, "warmup has no semantic output")
			for i, turn := range turns[1:] {
				require.NotNil(t, turn.FirstTokenMs, "generation %d must have its own TTFT", i+1)
				require.Positive(t, *turn.FirstTokenMs)
				require.Less(t, int64(*turn.FirstTokenMs), turn.Duration.Milliseconds())
			}
			require.NotNil(t, result.FirstTokenMs)
			require.Equal(t, 8, result.Usage.InputTokens)
			require.Equal(t, 3, result.Usage.OutputTokens)
			require.Len(t, client.Writes(), len(frames), "timing must not consume output frames")
		})
	}
}

func TestObserveUpstreamMessage_FirstTokenMs_IndependentOfConnection(t *testing.T) {
	t.Parallel()
	for _, withResponseID := range []bool{false, true} {
		t.Run(fmt.Sprintf("response_id_%t", withResponseID), func(t *testing.T) {
			t.Parallel()
			state := &relayState{}
			base := time.Unix(100, 0)
			for turn := 1; turn <= 3; turn++ {
				start := base.Add(time.Duration(turn-1) * time.Second)
				state.setPendingTurnStartedAt(start)
				observe := func(payload string, elapsed time.Duration) observedUpstreamEvent {
					return observeUpstreamMessage(state, []byte(payload), base, func() time.Time { return start.Add(elapsed) }, nil)
				}
				observe(fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_%d"}}`, turn), 10*time.Millisecond)
				delta := `{"type":"response.output_text.delta","delta":"hello"}`
				if withResponseID {
					delta = fmt.Sprintf(`{"type":"response.output_text.delta","response_id":"resp_%d","delta":"hello"}`, turn)
				}
				observe(delta, time.Duration(turn*20)*time.Millisecond)
				observe(delta, time.Duration(turn*20+10)*time.Millisecond)
				got := observe(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d"}}`, turn), 100*time.Millisecond)
				require.True(t, got.terminal)
				require.NotNil(t, got.firstToken, "turn %d must record its first output", turn)
				require.Equal(t, turn*20, *got.firstToken, "TTFT uses this turn's start and is recorded once")
				require.NotNil(t, state.firstTokenMs)
				require.Equal(t, 20, *state.firstTokenMs, "connection TTFT must remain the first generation's value")
			}
		})
	}
}
