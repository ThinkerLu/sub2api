package handler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIWSNextAttemptMessageUsesCurrentTurnPayload(t *testing.T) {
	firstMessage := []byte(`{"type":"response.create","input":"first"}`)
	currentTurn := []byte(`{"type":"response.create","input":"turn-281"}`)

	next, ok := openAIWSNextAttemptMessage(firstMessage, currentTurn, true)

	require.True(t, ok)
	require.Equal(t, currentTurn, next)
	next[0] = 'x'
	require.Equal(t, byte('{'), currentTurn[0], "retry payload must be cloned")
}

func TestOpenAIWSNextAttemptMessageRejectsMissingCurrentTurnPayload(t *testing.T) {
	next, ok := openAIWSNextAttemptMessage([]byte(`{"type":"response.create"}`), nil, true)

	require.False(t, ok)
	require.Nil(t, next)
}

func TestOpenAIWSNextAttemptMessageKeepsInitialMessageForFirstTurnFailover(t *testing.T) {
	firstMessage := []byte(`{"type":"response.create","input":"first"}`)

	next, ok := openAIWSNextAttemptMessage(firstMessage, nil, false)

	require.True(t, ok)
	require.Equal(t, firstMessage, next)
}

func TestOpenAIWSNextAttemptMessageCannotReplayReleasedFirstTurn(t *testing.T) {
	initial := &openAIWSAttemptPayload{body: []byte(`{"type":"response.create","input":"first"}`)}
	// Failed first attempts retain the initial body. A completed turn releases it.
	require.NotEmpty(t, initial.load())
	initial.store(nil)
	next, ok := openAIWSNextAttemptMessage(initial.load(), nil, false)
	require.False(t, ok)
	require.Nil(t, next)
	// Subsequent turns still fail over using their own complete history.
	current := []byte(`{"type":"response.create","input":"later"}`)
	next, ok = openAIWSNextAttemptMessage(initial.load(), current, true)
	require.True(t, ok)
	require.Equal(t, current, next)
	initial.store(next)
	require.Equal(t, current, initial.load())
}
