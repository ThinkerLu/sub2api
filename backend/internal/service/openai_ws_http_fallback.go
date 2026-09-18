package service

import (
	"sync"
	"time"
)

const openAIWSHTTPFallbackMarkerTTL = 5 * time.Minute

var openAIWSHTTPFallbackMarkers sync.Map

// MarkOpenAIWSHTTPFallback remembers that a client session has already probed
// a model with no native WS route. Codex immediately falls back to HTTP when
// its next WS handshake receives 426, avoiding the normal five stream retries.
func MarkOpenAIWSHTTPFallback(sessionKey string) {
	if sessionKey == "" {
		return
	}
	openAIWSHTTPFallbackMarkers.Store(sessionKey, time.Now().Add(openAIWSHTTPFallbackMarkerTTL))
}

// OpenAIWSHTTPFallbackRequired reports whether a reconnect for this client
// session should be rejected with HTTP 426.
func OpenAIWSHTTPFallbackRequired(sessionKey string) bool {
	if sessionKey == "" {
		return false
	}
	value, ok := openAIWSHTTPFallbackMarkers.Load(sessionKey)
	if !ok {
		return false
	}
	expiresAt, ok := value.(time.Time)
	if !ok || time.Now().After(expiresAt) {
		openAIWSHTTPFallbackMarkers.Delete(sessionKey)
		return false
	}
	return true
}
