package orchestrator

import "strings"

// Default models per provider. This is the single source of truth for the
// orchestrator; a model set in config (or per session) always overrides it.
//
//   - Anthropic: claude-sonnet-5-5, the current Sonnet.
//   - OpenAI: gpt-5.1, a current general model with tool calling.
//   - Gemini: gemini-pro-latest, Google's alias that tracks the newest Pro
//     model, so the default does not go stale as dated versions are retired.
const (
	DefaultOpenAIModel    = "gpt-5.1"
	DefaultAnthropicModel = "claude-sonnet-5-5"
	DefaultGeminiModel    = "gemini-pro-latest"
)

// DefaultMaxTokens is the output-token ceiling sent to Anthropic (where
// max_tokens is required) when none is configured. Responses are streamed, so a
// generous ceiling does not risk HTTP timeouts, and adaptive thinking on current
// Claude models counts against it. OpenAI and Gemini only receive a limit when
// one is configured explicitly, because compatible servers disagree on the
// parameter name and on acceptable ranges.
const DefaultMaxTokens = 32000

// DefaultModel returns the default model for a provider name ("" means openai).
// Unknown providers return "".
func DefaultModel(provider string) string {
	switch normalizeProvider(provider) {
	case "openai":
		return DefaultOpenAIModel
	case "anthropic":
		return DefaultAnthropicModel
	case "gemini":
		return DefaultGeminiModel
	}
	return ""
}

func normalizeProvider(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == "" {
		return "openai"
	}
	return p
}
