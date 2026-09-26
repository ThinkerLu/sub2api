package service

import (
	"net/url"
	"strings"
)

const ResponsesWSProfileOpenAIV2 = "openai_responses_v2"

func (a *Account) ResponsesWSV2Profile() string {
	if a == nil {
		return ""
	}
	if a.IsOpenAI() {
		return ResponsesWSProfileOpenAIV2
	}
	if a.Type != AccountTypeAPIKey || (!a.IsCNProvider() && !a.IsOpenCodeGo()) {
		return ""
	}
	profile, _ := a.Extra["responses_ws_profile"].(string)
	endpoint, _ := a.Extra["responses_ws_url"].(string)
	httpBase := strings.TrimSpace(a.GetCredential("base_url"))
	if bases, ok := a.Credentials["api_base_urls"].(map[string]any); ok && httpBase == "" {
		httpBase, _ = bases[APIProtocolResponses].(string)
	}
	if profile != ResponsesWSProfileOpenAIV2 || strings.TrimSpace(httpBase) == "" {
		return ""
	}
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "wss" && parsed.Scheme != "ws") {
		return ""
	}
	return profile
}

func openAIWSHasCompatibleHTTP(account *Account) bool {
	if account == nil {
		return false
	}
	if account.IsOpenAI() {
		return account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses) && !shouldForwardOpenAIResponsesViaRawChatCompletions(account)
	}
	return account.Type == AccountTypeAPIKey && (account.UsesNativeCNResponses() || (account.ResponsesWSV2Profile() != "" && account.IsAdaptiveAPIProtocol()))
}
