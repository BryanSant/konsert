package orchestrator

import (
	"regexp"
	"strings"
)

// secretPatterns match credentials that may leak into error strings (URLs with
// key query params, auth headers echoed by proxies, well-known key prefixes).
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)([?&](?:key|api_key|apikey|access_token|token)=)[^&\s"']+`),
	regexp.MustCompile(`(?i)((?:x-goog-api-key|x-api-key|authorization)["']?\s*[:=]\s*["']?)[^\s"',}]+`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{20,}`),
}

const redacted = "[REDACTED]"

// RedactSecrets removes the given secret values and anything that looks like a
// credential from s. It is applied to every error before it leaves the
// orchestrator (OnError callbacks and returned errors), because those strings
// are broadcast to browsers and MCP callers.
func RedactSecrets(s string, secrets ...string) string {
	for _, sec := range secrets {
		if sec = strings.TrimSpace(sec); len(sec) >= 4 {
			s = strings.ReplaceAll(s, sec, redacted)
		}
	}
	for _, re := range secretPatterns {
		if re.NumSubexp() > 0 {
			s = re.ReplaceAllString(s, "${1}"+redacted)
		} else {
			s = re.ReplaceAllString(s, redacted)
		}
	}
	return s
}

// redactedError carries a scrubbed message while keeping the original error
// reachable for errors.Is / errors.As.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

func redactError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := RedactSecrets(err.Error(), secrets...)
	if msg == err.Error() {
		return err
	}
	return &redactedError{msg: msg, err: err}
}
