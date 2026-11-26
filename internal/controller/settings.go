package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/BryanSant/syre/internal/commander"
	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// errAdminScopeRequired is returned when a caller without the admin scope (an MCP
// client holding an MCP-only device credential) tries to change where the LLM API key is
// sent (llm_base_url) or the key itself.
var errAdminScopeRequired = errors.New("changing llm_base_url or the LLM API key needs a full-access credential, not an MCP-only one")

// errInvalidSettings wraps validation failures so handlers can map them to 400.
type errInvalidSettings struct{ err error }

func (e errInvalidSettings) Error() string { return "invalid settings: " + e.err.Error() }
func (e errInvalidSettings) Unwrap() error { return e.err }

// SettingsUpdate is a partial settings change. Nil fields are left untouched.
// REST (PUT /api/settings) and MCP (update_settings) both funnel into applySettings.
type SettingsUpdate struct {
	LLMProvider           *string `json:"llm_provider"`
	LLMModel              *string `json:"llm_model"`
	LLMBaseURL            *string `json:"llm_base_url"`
	LLMAPIKey             *string `json:"llm_api_key"`
	ClearAPIKey           bool    `json:"clear_api_key"`
	DefaultAgent          *string `json:"default_agent"`
	DefaultModel          *string `json:"default_model"`
	DefaultPermissionMode *string `json:"default_permission_mode"`
	TerminalFontSize      *int    `json:"terminal_font_size"`
	NotifySound           *bool   `json:"notify_sound"`
	NotifyDesktop         *bool   `json:"notify_desktop"`
	VetoAckMinutes        *int    `json:"veto_ack_minutes"`
	// DeviceIdleDays is the sliding expiry of device credentials (1-3650 days).
	DeviceIdleDays *int `json:"device_idle_days"`
	// Global delivery defaults (SYR-H50YW).
	DefaultDeliveryMode *string `json:"default_delivery_mode"`
	DefaultMergeStyle   *string `json:"default_merge_style"`
	// TicketLeaseTTLSeconds is the duration of a ticket lease in seconds (default 600 = 10m).
	TicketLeaseTTLSeconds *int `json:"ticket_lease_ttl_seconds"`
	// VerifyTimeoutSeconds is the verification command timeout in seconds (default 1200 = 20m).
	VerifyTimeoutSeconds *int `json:"verify_timeout_seconds"`
	// PulseDebounceSeconds is the board pulse batching window in seconds (default 20s).
	PulseDebounceSeconds *int `json:"pulse_debounce_seconds"`
	// AutopilotGlobal enables Autopilot Play across all projects unless explicitly paused.
	AutopilotGlobal *bool `json:"autopilot_global"`
	// CommanderPolicyWrites is tighten (default), confirm, or none.
	CommanderPolicyWrites *string `json:"commander_policy_writes"`
}

// applySettings validates, persists and activates a settings change in one place:
// it updates s.cfg under cfgMu, writes SQLite, and hot-reloads the commander.
// admin reports whether the caller holds the admin scope.
func (s *Server) applySettings(ctx context.Context, u SettingsUpdate, admin bool) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	next := s.cfg.Commander
	updates := make(map[string]string)

	if u.LLMProvider != nil {
		if v := strings.TrimSpace(*u.LLMProvider); v != "" {
			next.Provider = v
			updates["llm_provider"] = v
		}
	}
	if u.LLMModel != nil {
		v := strings.TrimSpace(*u.LLMModel)
		next.Model = v
		updates["llm_model"] = v
	}
	keyOrURLChange := false
	if u.LLMBaseURL != nil {
		v := strings.TrimSpace(*u.LLMBaseURL)
		if v != next.BaseURL {
			keyOrURLChange = true
		}
		next.BaseURL = v
		updates["llm_base_url"] = v
	}
	if u.ClearAPIKey {
		keyOrURLChange = true
		next.APIKey = ""
		updates["llm_api_key"] = ""
	} else if u.LLMAPIKey != nil && strings.TrimSpace(*u.LLMAPIKey) != "" {
		keyOrURLChange = true
		v := strings.TrimSpace(*u.LLMAPIKey)
		next.APIKey = v
		sealed, err := s.secrets.Seal(v)
		if err != nil {
			return fmt.Errorf("encrypt API key: %w", err)
		}
		updates["llm_api_key"] = sealed
	}
	if keyOrURLChange && !admin {
		return errAdminScopeRequired
	}
	if err := validateCommanderConfig(next); err != nil {
		return errInvalidSettings{err}
	}

	if u.DefaultAgent != nil {
		updates["default_agent"] = strings.TrimSpace(*u.DefaultAgent)
	}
	if u.DefaultModel != nil {
		updates["default_model"] = strings.TrimSpace(*u.DefaultModel)
	}
	if u.DefaultPermissionMode != nil {
		v := strings.TrimSpace(*u.DefaultPermissionMode)
		if v == protocol.PermissionFullAccess || v == protocol.PermissionSandboxed {
			updates["default_permission_mode"] = v
		}
	}
	if u.TerminalFontSize != nil && *u.TerminalFontSize >= 10 && *u.TerminalFontSize <= 24 {
		updates["terminal_font_size"] = strconv.Itoa(*u.TerminalFontSize)
	}
	if u.NotifySound != nil {
		updates["notify_sound"] = strconv.FormatBool(*u.NotifySound)
	}
	if u.NotifyDesktop != nil {
		updates["notify_desktop"] = strconv.FormatBool(*u.NotifyDesktop)
	}
	if u.VetoAckMinutes != nil {
		if *u.VetoAckMinutes < 1 || *u.VetoAckMinutes > 24*60 {
			return errInvalidSettings{errors.New("veto_ack_minutes must be between 1 and 1440")}
		}
		updates[vetoAckSetting] = strconv.Itoa(*u.VetoAckMinutes)
	}
	if u.DeviceIdleDays != nil {
		if *u.DeviceIdleDays < 1 || *u.DeviceIdleDays > maxDeviceIdleDays {
			return errInvalidSettings{errors.New("device_idle_days must be between 1 and 3650")}
		}
		updates[deviceIdleSetting] = strconv.Itoa(*u.DeviceIdleDays)
	}
	if u.DefaultDeliveryMode != nil {
		v := strings.TrimSpace(*u.DefaultDeliveryMode)
		if !store.ValidDeliveryMode(v) {
			return errInvalidSettings{fmt.Errorf("invalid default_delivery_mode %q", v)}
		}
		updates[deliveryModeSetting] = v
	}
	if u.DefaultMergeStyle != nil {
		v := strings.TrimSpace(*u.DefaultMergeStyle)
		if !store.ValidMergeStyle(v) {
			return errInvalidSettings{fmt.Errorf("invalid default_merge_style %q", v)}
		}
		updates[mergeStyleSetting] = v
	}
	if u.TicketLeaseTTLSeconds != nil {
		if *u.TicketLeaseTTLSeconds < minTicketLeaseTTLSeconds || *u.TicketLeaseTTLSeconds > maxTicketLeaseTTLSeconds {
			return errInvalidSettings{fmt.Errorf("%s must be between %d and %d", ticketLeaseTTLSetting, minTicketLeaseTTLSeconds, maxTicketLeaseTTLSeconds)}
		}
		updates[ticketLeaseTTLSetting] = strconv.Itoa(*u.TicketLeaseTTLSeconds)
	}
	if u.VerifyTimeoutSeconds != nil {
		if *u.VerifyTimeoutSeconds < minVerifyTimeoutSeconds || *u.VerifyTimeoutSeconds > maxVerifyTimeoutSeconds {
			return errInvalidSettings{fmt.Errorf("%s must be between %d and %d", verifyTimeoutSetting, minVerifyTimeoutSeconds, maxVerifyTimeoutSeconds)}
		}
		updates[verifyTimeoutSetting] = strconv.Itoa(*u.VerifyTimeoutSeconds)
	}
	if u.PulseDebounceSeconds != nil {
		if *u.PulseDebounceSeconds < minPulseDebounceSeconds || *u.PulseDebounceSeconds > maxPulseDebounceSeconds {
			return errInvalidSettings{fmt.Errorf("%s must be between %d and %d", pulseDebounceSetting, minPulseDebounceSeconds, maxPulseDebounceSeconds)}
		}
		updates[pulseDebounceSetting] = strconv.Itoa(*u.PulseDebounceSeconds)
	}
	if u.AutopilotGlobal != nil {
		updates[autopilotGlobalSetting] = strconv.FormatBool(*u.AutopilotGlobal)
	}
	if u.CommanderPolicyWrites != nil {
		v, err := normalizePolicyWrites(*u.CommanderPolicyWrites)
		if err != nil {
			return errInvalidSettings{err}
		}
		updates[commanderPolicyWritesSetting] = v
	}

	if len(updates) > 0 {
		if err := s.store.SetSettings(ctx, updates); err != nil {
			return err
		}
	}
	if next != s.cfg.Commander {
		s.cfg.Commander = next
		if err := s.orch.UpdateConfig(next); err != nil {
			return errInvalidSettings{err}
		}
		s.orch.SetMaxTokens(next.MaxTokens)
	}
	if u.DeviceIdleDays != nil {
		s.auth.SetIdleExpiry(idleDuration(*u.DeviceIdleDays))
	}
	recordSettingsAudit(ctx, s.hub, updates)
	if u.AutopilotGlobal != nil {
		if *u.AutopilotGlobal {
			s.hub.clearAllAutopilotFinished()
			if projects, err := s.store.ListProjects(ctx); err == nil {
				for _, p := range projects {
					if !p.AutopilotPaused {
						s.hub.markProjectActive(p.ID)
					}
				}
			}
		}
		s.hub.broadcastAutopilotUpdate(ctx)
		if *u.AutopilotGlobal && !s.hub.fleetPaused(ctx) {
			s.hub.kickScheduler()
		}
	}
	return nil
}

// recordSettingsAudit audits a settings change by key name only. Values are never
// recorded: llm_api_key is a secret and the others are not needed to audit.
func recordSettingsAudit(ctx context.Context, h *Hub, updates map[string]string) {
	if len(updates) == 0 {
		return
	}
	keys := make([]string, 0, len(updates))
	for k := range updates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h.recordAudit(ctx, store.AuditEvent{
		Action:   AuditSettingsUpdate,
		Message:  "Changed settings: " + strings.Join(keys, ", "),
		MetaJSON: auditMeta(map[string]any{"keys": keys}),
	})
}

// validateCommanderConfig applies config validation plus the commander's own
// provider check (config cannot import commander, so both run here).
func validateCommanderConfig(c config.CommanderConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return commander.ValidateProvider(c.Provider)
}

// settingsUpdateFromMap converts the string map used by the MCP update_settings tool.
func settingsUpdateFromMap(m map[string]string) (SettingsUpdate, error) {
	var u SettingsUpdate
	var unknown []string
	str := func(v string) *string { return &v }
	for k, raw := range m {
		v := strings.TrimSpace(raw)
		switch k {
		case "llm_provider":
			u.LLMProvider = str(v)
		case "llm_model":
			u.LLMModel = str(v)
		case "llm_base_url":
			u.LLMBaseURL = str(v)
		case "llm_api_key":
			u.LLMAPIKey = str(v)
		case "clear_api_key":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return u, errInvalidSettings{fmt.Errorf("clear_api_key: %w", err)}
			}
			u.ClearAPIKey = b
		case "default_agent":
			u.DefaultAgent = str(v)
		case "default_model":
			u.DefaultModel = str(v)
		case "default_permission_mode":
			u.DefaultPermissionMode = str(v)
		case "terminal_font_size":
			n, err := strconv.Atoi(v)
			if err != nil {
				return u, errInvalidSettings{fmt.Errorf("terminal_font_size: %w", err)}
			}
			u.TerminalFontSize = &n
		case vetoAckSetting:
			n, err := strconv.Atoi(v)
			if err != nil {
				return u, errInvalidSettings{fmt.Errorf("%s: %w", k, err)}
			}
			u.VetoAckMinutes = &n
		case deviceIdleSetting:
			n, err := strconv.Atoi(v)
			if err != nil {
				return u, errInvalidSettings{fmt.Errorf("%s: %w", k, err)}
			}
			u.DeviceIdleDays = &n
		case deliveryModeSetting:
			u.DefaultDeliveryMode = str(v)
		case mergeStyleSetting:
			u.DefaultMergeStyle = str(v)
		case ticketLeaseTTLSetting:
			n, err := strconv.Atoi(v)
			if err != nil {
				return u, errInvalidSettings{fmt.Errorf("%s: %w", k, err)}
			}
			u.TicketLeaseTTLSeconds = &n
		case verifyTimeoutSetting:
			n, err := strconv.Atoi(v)
			if err != nil {
				return u, errInvalidSettings{fmt.Errorf("%s: %w", k, err)}
			}
			u.VerifyTimeoutSeconds = &n
		case pulseDebounceSetting:
			n, err := strconv.Atoi(v)
			if err != nil {
				return u, errInvalidSettings{fmt.Errorf("%s: %w", k, err)}
			}
			u.PulseDebounceSeconds = &n
		case autopilotGlobalSetting:
			b, err := strconv.ParseBool(v)
			if err != nil {
				return u, errInvalidSettings{fmt.Errorf("%s: %w", k, err)}
			}
			u.AutopilotGlobal = &b
		case commanderPolicyWritesSetting:
			mode, err := normalizePolicyWrites(v)
			if err != nil {
				return u, errInvalidSettings{err}
			}
			u.CommanderPolicyWrites = str(mode)
		case "notify_sound", "notify_desktop":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return u, errInvalidSettings{fmt.Errorf("%s: %w", k, err)}
			}
			if k == "notify_sound" {
				u.NotifySound = &b
			} else {
				u.NotifyDesktop = &b
			}
		default:
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return u, errInvalidSettings{fmt.Errorf("unknown settings keys: %s", strings.Join(unknown, ", "))}
	}
	return u, nil
}

// settingsToMap renders the live settings for MCP. The API key is never included.
func settingsToMap(r settingsResponse) map[string]any {
	return map[string]any{
		"llm_provider":             r.LLMProvider,
		"llm_model":                r.LLMModel,
		"llm_base_url":             r.LLMBaseURL,
		"llm_api_key_set":          r.LLMAPIKeySet,
		"default_agent":            r.DefaultAgent,
		"default_model":            r.DefaultModel,
		"default_permission_mode":  r.DefaultPermissionMode,
		"terminal_font_size":       r.TerminalFontSize,
		"notify_sound":             r.NotifySound,
		"notify_desktop":           r.NotifyDesktop,
		"veto_ack_minutes":         r.VetoAckMinutes,
		"device_idle_days":         r.DeviceIdleDays,
		"default_delivery_mode":    r.DefaultDeliveryMode,
		"default_merge_style":      r.DefaultMergeStyle,
		"ticket_lease_ttl_seconds": r.TicketLeaseTTLSeconds,
		"verify_timeout_seconds":   r.VerifyTimeoutSeconds,
		"pulse_debounce_seconds":   r.PulseDebounceSeconds,
		"autopilot_global":         r.AutopilotGlobal,
		"commander_policy_writes":  r.CommanderPolicyWrites,
		"commander_backend":        r.CommanderBackend,
		"api_key_required":         r.APIKeyRequired,
	}
}

// serverControlPlane is the MCP backend used by the controller's /mcp endpoint. It is
// the FleetService with settings routed through the Server, so MCP settings changes
// are validated, take effect immediately, and honor the caller's scope.
type serverControlPlane struct {
	*FleetService
	srv   *Server
	admin bool
}

func (b serverControlPlane) GetSettingsMap(ctx context.Context) (map[string]any, error) {
	return settingsToMap(b.srv.buildSettingsResponse(ctx)), nil
}

// SteerTask, CancelTask and UpdateSettingsMap attribute MCP-initiated control
// actions to the calling MCP credential (or "mcp" when there is none) in the audit log.
func (b serverControlPlane) SteerTask(ctx context.Context, taskID, inputText string, appendNewline bool) (ActionResult, error) {
	return b.FleetService.SteerTask(withDefaultAuditActor(ctx, auditSourceMCP, auditSourceMCP), taskID, inputText, appendNewline)
}

func (b serverControlPlane) CancelTask(ctx context.Context, taskID string) (ActionResult, error) {
	return b.FleetService.CancelTask(withDefaultAuditActor(ctx, auditSourceMCP, auditSourceMCP), taskID)
}

func (b serverControlPlane) UpdateSettingsMap(ctx context.Context, updates map[string]string) (map[string]any, error) {
	ctx = withDefaultAuditActor(ctx, auditSourceMCP, auditSourceMCP)
	u, err := settingsUpdateFromMap(updates)
	if err != nil {
		return nil, err
	}
	if err := b.srv.applySettings(ctx, u, b.admin); err != nil {
		return nil, err
	}
	return b.GetSettingsMap(ctx)
}

// writeSettingsError maps applySettings errors to HTTP statuses.
func writeSettingsError(w http.ResponseWriter, err error) {
	var inv errInvalidSettings
	switch {
	case errors.Is(err, errAdminScopeRequired):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	case errors.As(err, &inv):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

// effectiveModel is the API-key backend's configured model. There is no built-in
// default: agent chats take models from capabilities, and the optional API-key
// backend needs an explicit llm_model.
func effectiveModel(c config.CommanderConfig) string {
	return strings.TrimSpace(c.Model)
}
