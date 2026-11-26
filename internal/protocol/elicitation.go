package protocol

import (
	"encoding/json/v2"
	"strings"
)

const (
	// MaxElicitationTextChars caps readable elicitation text stored in RunDigest.Question.Text.
	MaxElicitationTextChars = 120
	// PulseNeedsYouExcerptMaxChars caps the needs_you excerpt shown on a BoardPulse Trigger line.
	PulseNeedsYouExcerptMaxChars = 60
)

// FormatElicitationText renders an elicitation as a short, human-readable line with raw
// tool JSON stripped and length capped at MaxElicitationTextChars (~120 runes):
//   - permission elicitations: "permission: run <cmd>" / "permission: edit <file>" / "permission: read <file>"
//   - question elicitations: the question text itself
func FormatElicitationText(el *AgentElicitation) string {
	if el == nil {
		return ""
	}
	if el.Kind != ElicitPermission {
		text := cleanElicitationFragment(el.Title)
		if text == "" {
			text = cleanElicitationFragment(el.Detail)
		}
		return truncateRunesEllipsis(text, MaxElicitationTextChars)
	}

	var toolInput, toolKind, toolName string
	var toolPaths []string
	if el.ToolCall != nil {
		toolInput = el.ToolCall.Input
		toolKind = strings.ToLower(strings.TrimSpace(el.ToolCall.Kind))
		toolName = strings.ToLower(strings.TrimSpace(el.ToolCall.Name))
		toolPaths = el.ToolCall.Paths
	}

	fields := parseToolJSONFields(toolInput, el.Detail)

	if cmd := firstFieldString(fields, "CommandLine", "command", "cmd"); cmd != "" {
		return truncateRunesEllipsis("permission: run "+collapseWhitespace(cmd), MaxElicitationTextChars)
	}

	path := firstFieldString(fields,
		"file_path", "path", "filePath", "target_path", "targetPath",
		"TargetFile", "AbsolutePath", "notebook_path",
	)
	if path == "" && len(toolPaths) > 0 {
		path = strings.TrimSpace(toolPaths[0])
	}
	if path != "" {
		verb := permissionVerbForPath(toolKind, toolName)
		return truncateRunesEllipsis("permission: "+verb+" "+collapseWhitespace(path), MaxElicitationTextChars)
	}

	// Fallback: clean the title (or detail) with any raw JSON stripped.
	fallback := cleanPermissionTitle(el.Title)
	if fallback == "" {
		fallback = cleanPermissionTitle(el.Detail)
	}
	if fallback == "" {
		fallback = "tool call"
	}
	if strings.HasPrefix(strings.ToLower(fallback), "permission:") {
		return truncateRunesEllipsis(fallback, MaxElicitationTextChars)
	}
	return truncateRunesEllipsis("permission: "+fallback, MaxElicitationTextChars)
}

// FormatPulseNeedsYouExcerpt formats a digest question/permission string into a <=60-char
// excerpt suitable for the BoardPulse Trigger line (e.g. "permission to run ls -la").
func FormatPulseNeedsYouExcerpt(text string) string {
	s := cleanElicitationFragment(text)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(s), "permission: ") {
		s = "permission to " + strings.TrimSpace(s[len("permission: "):])
	}
	return truncateRunesEllipsis(s, PulseNeedsYouExcerptMaxChars)
}

func permissionVerbForPath(kind, name string) string {
	switch kind {
	case "read":
		return "read"
	case "search":
		return "search"
	case "edit", "delete", "move":
		return "edit"
	}
	switch {
	case strings.Contains(name, "read") || strings.Contains(name, "view") || strings.Contains(name, "list"):
		return "read"
	case strings.Contains(name, "search") || name == "glob" || name == "grep":
		return "search"
	default:
		return "edit"
	}
}

func parseToolJSONFields(sources ...string) map[string]any {
	out := map[string]any{}
	for _, src := range sources {
		s := strings.TrimSpace(src)
		if s == "" {
			continue
		}
		start := strings.IndexByte(s, '{')
		if start < 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(s[start:]), &m, JSONOptions) != nil || len(m) == 0 {
			continue
		}
		if nested, ok := m["rawInput"].(map[string]any); ok {
			for k, v := range nested {
				if _, exists := out[k]; !exists {
					out[k] = v
				}
			}
		}
		for k, v := range m {
			if k == "rawInput" {
				continue
			}
			if _, exists := out[k]; !exists {
				out[k] = v
			}
		}
	}
	return out
}

func firstFieldString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		for k, v := range m {
			if strings.EqualFold(k, key) {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	return ""
}

func cleanPermissionTitle(raw string) string {
	s := cleanElicitationFragment(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "Allow ") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "Allow "))
		s = strings.TrimSuffix(s, "?")
		s = strings.TrimSpace(s)
	}
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "run: "):
		return "run " + strings.TrimSpace(s[len("run: "):])
	case strings.HasPrefix(lower, "bash: "):
		return "run " + strings.TrimSpace(s[len("bash: "):])
	case strings.HasPrefix(lower, "shell: "):
		return "run " + strings.TrimSpace(s[len("shell: "):])
	case strings.HasPrefix(lower, "edit: "):
		return "edit " + strings.TrimSpace(s[len("edit: "):])
	case strings.HasPrefix(lower, "write: "):
		return "edit " + strings.TrimSpace(s[len("write: "):])
	case strings.HasPrefix(lower, "read: "):
		return "read " + strings.TrimSpace(s[len("read: "):])
	}
	return s
}

func cleanElicitationFragment(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// Strip an inline/appended JSON object (e.g. "cmd: {\"CommandLine\":...}" or raw "{...}").
	if idx := strings.IndexByte(s, '{'); idx >= 0 {
		if idx == 0 {
			return ""
		}
		s = strings.TrimSpace(s[:idx])
		s = strings.TrimRight(s, ":")
		s = strings.TrimSpace(s)
	}
	return collapseWhitespace(s)
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncateRunesEllipsis(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	if maxRunes <= 3 {
		return string(r[:maxRunes])
	}
	return strings.TrimSpace(string(r[:maxRunes-3])) + "..."
}
