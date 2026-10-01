package service

import (
	"regexp"
	"strings"
)

var openAIWSTransportSecret = regexp.MustCompile(`(?i)(bearer\s+)[^\s"',;]+|((?:api[_-]?key|access[_-]?token|refresh[_-]?token|authorization|password)\s*[=:]\s*)[^\s"',;&]+|sk-[a-zA-Z0-9_-]+|eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+`)

// Transport errors contain no request body; redact credential-shaped values before truncating.
func openAIWSTransportDiagnostic(message string) string {
	message = sanitizeUpstreamErrorMessage(message)
	message = strings.Join(strings.Fields(message), " ")
	message = openAIWSTransportSecret.ReplaceAllString(message, `${1}${2}[REDACTED]`)
	return truncateOpenAIWSLogValue(message, 1024)
}
