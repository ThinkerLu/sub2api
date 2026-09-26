package service

import (
	"encoding/json"
	"errors"
	"sync"
)

// Retained items must not pin unrelated bytes in their source frame.
func ownedOpenAIWSReplayItems(items []json.RawMessage) []json.RawMessage {
	owned := make([]json.RawMessage, len(items))
	for i, item := range items {
		owned[i] = make([]byte, len(item))
		copy(owned[i], item)
	}
	return owned
}

var errOpenAIWSAdaptiveSocketLimit = errors.New("websocket account connection limit reached")

func (s *OpenAIGatewayService) acquireOpenAIWSAdaptiveSocket(accountID int64, limit int) (func(), error) {
	s.openaiWSAdaptiveSocketMu.Lock()
	defer s.openaiWSAdaptiveSocketMu.Unlock()
	if accountID <= 0 || limit <= 0 || s.openaiWSAdaptiveSockets[accountID] >= limit {
		return nil, errOpenAIWSAdaptiveSocketLimit
	}
	if s.openaiWSAdaptiveSockets == nil {
		s.openaiWSAdaptiveSockets = make(map[int64]int)
	}
	s.openaiWSAdaptiveSockets[accountID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.openaiWSAdaptiveSocketMu.Lock()
			defer s.openaiWSAdaptiveSocketMu.Unlock()
			s.openaiWSAdaptiveSockets[accountID]--
			if s.openaiWSAdaptiveSockets[accountID] == 0 {
				delete(s.openaiWSAdaptiveSockets, accountID)
			}
		})
	}, nil
}
