package chat

// Cursor plan usage (SYR-621BR) from cursor.com's dashboard API, with the login
// cursor-agent itself uses. The token is read per account: the file store
// ($HOME/.cursor/auth.json, AGENT_CLI_CREDENTIAL_STORE=file), or the Linux keyring
// (secret-tool). The macOS Keychain is not read: a read of another program's item can
// pop an access dialog on the user's screen even from a background syred, so such
// accounts get a note on switching to the file store instead.

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

var cursorHTTP = newUsageClient()

// cursorBase is cursor.com (tests point it at a fake).
var cursorBase = "https://cursor.com"

// cursorToken finds the account's access token; note explains when there is none.
var cursorToken = func(ctx context.Context, acct Account) (token, note string) {
	dir := accountDir(acct, "CURSOR_CONFIG_DIR", ".cursor")
	if raw, err := os.ReadFile(filepath.Join(dir, "auth.json")); err == nil {
		var f struct {
			AccessToken string `json:"accessToken"`
		}
		if unmarshal(raw, &f) == nil && f.AccessToken != "" {
			return f.AccessToken, ""
		}
	}
	switch runtime.GOOS {
	case "linux":
		out, code, err := RunQuick(ctx, "secret-tool", []string{"lookup", "service", "cursor-credentials", "account", "cursor-user"}, acct.Env, 5*time.Second)
		if err == nil && code == 0 {
			var f struct {
				AccessToken string `json:"accessToken"`
			}
			if unmarshal([]byte(strings.TrimSpace(out)), &f) == nil && f.AccessToken != "" {
				return f.AccessToken, ""
			}
		}
		return "", "Cursor's sign-in isn't readable here (keyring locked?)"
	case "darwin":
		return "", "Cursor keeps its sign-in in the macOS Keychain: set AGENT_CLI_CREDENTIAL_STORE=file for this account to show usage"
	}
	return "", "Cursor's sign-in isn't readable here"
}

// cursorUsage fetches the account's plan usage.
func cursorUsage(ctx context.Context, acct Account) acpUsage {
	token, note := cursorToken(ctx, acct)
	if token == "" {
		return acpUsage{Note: note}
	}
	sub, exp, ok := jwtSubExp(token)
	if !ok {
		return acpUsage{Err: errors.New("Cursor's sign-in token is unreadable: run cursor-agent login")}
	}
	if !exp.IsZero() && time.Now().After(exp) {
		return acpUsage{Err: errors.New("Cursor sign-in expired: run cursor-agent login")}
	}
	user := sub
	if i := strings.LastIndexByte(sub, '|'); i >= 0 {
		user = sub[i+1:]
	}
	h := http.Header{}
	h.Set("Cookie", "WorkosCursorSessionToken="+url.QueryEscape(user)+"%3A%3A"+token)
	h.Set("Origin", "https://cursor.com")
	h.Set("Referer", "https://cursor.com/dashboard")
	key := "cursor:" + acct.ID
	body, err := cursorHTTP.Get(ctx, key, cursorBase+"/api/usage-summary", h)
	if err != nil {
		// Old request-based plans: per-model request counts.
		if legacy, lerr := cursorHTTP.Get(ctx, key, cursorBase+"/api/usage?user="+url.QueryEscape(user), h); lerr == nil {
			if w, perr := parseCursorLegacyUsage(legacy); perr == nil {
				return acpUsage{Windows: w}
			}
		}
		return acpUsage{Err: err}
	}
	u, perr := parseCursorUsageSummary(body)
	// Unified plans (and accounts whose pools all read 0%) report their usage in the
	// current-period endpoint.
	if perr != nil || strings.HasPrefix(u.Plan, "unified_") || allZero(u.Windows) {
		if pb, err := cursorHTTP.Post(ctx, key, cursorBase+"/api/dashboard/get-current-period-usage", "{}", h); err == nil {
			if pu, err := parseCursorPeriodUsage(pb); err == nil && !allZero(pu.Windows) {
				pu.Plan = cmpOrString(pu.Plan, u.Plan)
				return pu
			}
		}
	}
	if perr != nil {
		return acpUsage{Err: perr}
	}
	return u
}

func allZero(ws []protocol.AgentRateLimit) bool {
	for _, w := range ws {
		if w.UsedPercent > 0 {
			return false
		}
	}
	return true
}

func jwtSubExp(token string) (string, time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", time.Time{}, false
	}
	var c struct {
		Sub string  `json:"sub"`
		Exp float64 `json:"exp"`
	}
	if unmarshal(raw, &c) != nil || c.Sub == "" {
		return "", time.Time{}, false
	}
	var exp time.Time
	if c.Exp > 0 {
		exp = time.Unix(int64(c.Exp), 0)
	}
	return c.Sub, exp, true
}

type cursorPool struct {
	Enabled          *bool    `json:"enabled"`
	Used             float64  `json:"used"`
	Limit            *float64 `json:"limit"`
	AutoPercentUsed  *float64 `json:"autoPercentUsed"`
	APIPercentUsed   *float64 `json:"apiPercentUsed"`
	TotalPercentUsed *float64 `json:"totalPercentUsed"`
}

func (p *cursorPool) percent() (float64, bool) {
	if p == nil || p.Enabled != nil && !*p.Enabled {
		return 0, false
	}
	if p.TotalPercentUsed != nil {
		return *p.TotalPercentUsed, true
	}
	if p.Limit != nil && *p.Limit > 0 {
		return p.Used / *p.Limit * 100, true
	}
	return 0, false
}

type cursorSummary struct {
	BillingCycleEnd string `json:"billingCycleEnd"`
	MembershipType  string `json:"membershipType"`
	IsUnlimited     bool   `json:"isUnlimited"`
	IndividualUsage struct {
		Plan     *cursorPool `json:"plan"`
		Overall  *cursorPool `json:"overall"`
		OnDemand *cursorPool `json:"onDemand"`
	} `json:"individualUsage"`
	TeamUsage struct {
		OnDemand *cursorPool `json:"onDemand"`
	} `json:"teamUsage"`
}

// parseCursorUsageSummary maps usage-summary: the plan total (monthly), Cursor Models
// and Other Models (individual plans), On-demand when enabled; all reset at the end of
// the billing cycle. An unlimited plan has no windows.
func parseCursorUsageSummary(raw []byte) (acpUsage, error) {
	var s cursorSummary
	if err := unmarshal(raw, &s); err != nil {
		return acpUsage{}, errors.New("cursor usage answer unreadable")
	}
	u := acpUsage{Plan: s.MembershipType}
	if s.IsUnlimited {
		u.Note = "Unlimited plan"
		return u, nil
	}
	reset := cursorTime(s.BillingCycleEnd)
	add := func(window, label string, pct float64) {
		u.Windows = append(u.Windows, cursorWindow(window, label, pct, reset))
	}
	total := s.IndividualUsage.Plan
	if total == nil || total.TotalPercentUsed == nil && total.Limit == nil {
		total = s.IndividualUsage.Overall
	}
	if pct, ok := total.percent(); ok {
		add("monthly", "Plan total", pct)
	}
	if p := s.IndividualUsage.Plan; p != nil {
		if p.AutoPercentUsed != nil {
			add("monthly_cursor_models", "Cursor Models", *p.AutoPercentUsed)
		}
		if p.APIPercentUsed != nil {
			add("monthly_other_models", "Other Models", *p.APIPercentUsed)
		}
	}
	od := s.IndividualUsage.OnDemand
	if od == nil || od.Enabled != nil && !*od.Enabled {
		od = s.TeamUsage.OnDemand
	}
	if pct, ok := od.percent(); ok {
		add("monthly_on_demand", "On-demand", pct)
	}
	if len(u.Windows) == 0 {
		return u, errors.New("cursor reported no usage pools")
	}
	return u, nil
}

// parseCursorPeriodUsage maps get-current-period-usage (unified plans).
func parseCursorPeriodUsage(raw []byte) (acpUsage, error) {
	var s struct {
		BillingCycleEnd string      `json:"billingCycleEnd"`
		PlanUsage       *cursorPool `json:"planUsage"`
	}
	if err := unmarshal(raw, &s); err != nil || s.PlanUsage == nil {
		return acpUsage{}, errors.New("cursor period usage has no plan usage")
	}
	reset := cursorTime(s.BillingCycleEnd)
	var u acpUsage
	if pct, ok := s.PlanUsage.percent(); ok {
		u.Windows = append(u.Windows, cursorWindow("monthly", "Plan total", pct, reset))
	}
	if p := s.PlanUsage.AutoPercentUsed; p != nil {
		u.Windows = append(u.Windows, cursorWindow("monthly_cursor_models", "Cursor Models", *p, reset))
	}
	if p := s.PlanUsage.APIPercentUsed; p != nil {
		u.Windows = append(u.Windows, cursorWindow("monthly_other_models", "Other Models", *p, reset))
	}
	if len(u.Windows) == 0 {
		return u, errors.New("cursor period usage has no pools")
	}
	return u, nil
}

// parseCursorLegacyUsage maps the old /api/usage form: per-model request counts against
// a monthly maximum, from startOfMonth.
func parseCursorLegacyUsage(raw []byte) ([]protocol.AgentRateLimit, error) {
	var m map[string]any
	if err := unmarshal(raw, &m); err != nil {
		return nil, errors.New("cursor legacy usage unreadable")
	}
	var reset *time.Time
	if s, ok := m["startOfMonth"].(string); ok {
		if t := cursorTime(s); t != nil {
			r := t.AddDate(0, 1, 0)
			reset = &r
		}
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []protocol.AgentRateLimit
	for _, name := range names {
		mm, ok := m[name].(map[string]any)
		if !ok {
			continue
		}
		n, _ := mm["numRequests"].(float64)
		max, ok := mm["maxRequestUsage"].(float64)
		if !ok || max <= 0 {
			continue
		}
		out = append(out, cursorWindow("monthly_"+codexLimitSlug(name), name, n/max*100, reset))
	}
	if len(out) == 0 {
		return nil, errors.New("cursor legacy usage has no limited models")
	}
	return out, nil
}

func cursorWindow(window, label string, pct float64, reset *time.Time) protocol.AgentRateLimit {
	pct = math.Round(min(max(pct, 0), 100)*10) / 10
	rl := protocol.AgentRateLimit{Window: window, Label: label, UsedPercent: pct, ResetsAt: reset, State: claudePercentState(pct)}
	if pct >= 100 {
		rl.State = "exhausted"
	}
	return rl
}

// cursorTime reads an RFC 3339 time or Unix milliseconds.
func cursorTime(s string) *time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		t = t.UTC()
		return &t
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil && ms > 0 {
		t := time.UnixMilli(ms).UTC()
		return &t
	}
	return nil
}
