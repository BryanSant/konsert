package chat

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

func loadFixture(t *testing.T, name string) (acpInitResult, *acpSessionResult) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "acp", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Initialize struct {
			Result acpInitResult `json:"result"`
		} `json:"initialize"`
		SessionNew struct {
			Result *acpSessionResult `json:"result"`
		} `json:"session_new"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Initialize.Result, f.SessionNew.Result
}

func modelByID(t *testing.T, ms []protocol.AgentModel, id string) protocol.AgentModel {
	t.Helper()
	for _, m := range ms {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("model %s not found in %d models", id, len(ms))
	return protocol.AgentModel{}
}

func effortIDs(m protocol.AgentModel) string {
	var ids []string
	for _, e := range m.Efforts {
		ids = append(ids, e.ID)
	}
	return strings.Join(ids, ",")
}

func TestACPCatalogGrok(t *testing.T) {
	init, res := loadFixture(t, "grok")
	if init.version() != "1.0.92" || !init.AgentCapabilities.MCPCapabilities.HTTP || !init.AgentCapabilities.SessionCapabilities.Close.set || !init.AgentCapabilities.SessionCapabilities.Resume.set {
		t.Errorf("init caps = %+v", init.AgentCapabilities)
	}
	cat := buildCatalog(res)
	ms := cat.Models()
	if len(ms) != 4 || cat.CurrentModel != "grok-4.5" || cat.CurrentEffort != "high" {
		t.Fatalf("catalog = %+v", cat)
	}
	m := modelByID(t, ms, "grok-4.7")
	if m.DisplayName != "Grok 4.7" || m.Description == "" || effortIDs(m) != "low,medium,high,xhigh" || m.DefaultEffort != "high" || m.ContextTokens != 256000 {
		t.Errorf("grok-4.7 = %+v", m)
	}
	if m.Efforts[3].Label != "Extra High" {
		t.Errorf("label = %q", m.Efforts[3].Label)
	}
	if m := modelByID(t, ms, "grok-4.5"); effortIDs(m) != "low,medium,high" || !m.IsDefault {
		t.Errorf("grok-4.5 = %+v", m)
	}
	sel, ok := cat.resolve("grok-4.7", "low")
	if !ok || sel.ModelID != "grok-4.7" || sel.EffortConfig != "reasoning_effort" || sel.Effort != "low" {
		t.Errorf("resolve = %+v", sel)
	}
	if sel, _ := cat.resolve("grok-4.5", "xhigh"); sel.Note == "" {
		t.Error("unsupported effort accepted silently")
	}
}

func TestACPCatalogAgy(t *testing.T) {
	init, res := loadFixture(t, "agy")
	if !init.systemPromptAppend() || init.AgentCapabilities.MCPCapabilities.HTTP || init.version() != "1.5.14" {
		t.Errorf("agy init = %+v", init)
	}
	cat := buildCatalog(res)
	ms := cat.Models()
	if cat.CurrentModel != "gemini-4-neon" || cat.CurrentEffort != "high" {
		t.Fatalf("current = %s/%s", cat.CurrentModel, cat.CurrentEffort)
	}
	neon := modelByID(t, ms, "gemini-4-neon")
	if neon.DisplayName != "Gemini 4 Neon" || effortIDs(neon) != "low,medium,high,xhigh" || neon.DefaultEffort != "high" || !neon.IsDefault {
		t.Errorf("neon = %+v", neon)
	}
	if m := modelByID(t, ms, "claude-opus-5-5"); effortIDs(m) != "low,medium,high" || m.DisplayName != "Claude Opus 5.5" || m.DefaultEffort != "high" {
		t.Errorf("opus = %+v", m)
	}
	if m := modelByID(t, ms, "gemini-pro-commander-neon"); len(m.Efforts) != 0 || m.DisplayName != "Commander (Neon)" {
		t.Errorf("commander = %+v", m)
	}
	if m := modelByID(t, ms, "gpt-oss-120b"); effortIDs(m) != "medium" || m.DefaultEffort != "medium" {
		t.Errorf("gpt-oss = %+v", m)
	}
	if len(ms) != 9 {
		t.Errorf("%d agy models, want 9 (grouped)", len(ms))
	}
	for _, c := range []struct{ model, effort, want string }{
		{"gemini-4-neon", "low", "gemini-4-neon-low"},
		{"gemini-4-neon", "", "gemini-4-neon-high"},
		{"claude-sonnet-5-5", "medium", "claude-sonnet-5-5-medium"},
		{"gemini-4-hydrogen-xhigh", "", "gemini-4-hydrogen-xhigh"}, // raw id accepted
		{"gemini-pro-commander-neon", "high", "gemini-pro-commander-neon"},
		{"", "low", "gemini-4-neon-low"},
	} {
		sel, ok := cat.resolve(c.model, c.effort)
		if !ok || sel.ModelID != c.want {
			t.Errorf("resolve(%q,%q) = %+v, want %s", c.model, c.effort, sel, c.want)
		}
	}
	if _, ok := cat.resolve("nope", ""); ok {
		t.Error("unknown model resolved")
	}
}

func TestACPCatalogCursor(t *testing.T) {
	init, res := loadFixture(t, "cursor")
	if init.AgentCapabilities.SessionCapabilities.Close.set || !init.AgentCapabilities.LoadSession {
		t.Errorf("cursor caps = %+v", init.AgentCapabilities)
	}
	cat := buildCatalog(res)
	ms := cat.Models()
	if len(ms) != 42 || cat.CurrentModel != "default" {
		t.Fatalf("%d models, current %q", len(ms), cat.CurrentModel)
	}
	if m := modelByID(t, ms, "default"); m.DisplayName != "Auto" || !m.IsDefault || len(m.Efforts) != 0 {
		t.Errorf("auto = %+v", m)
	}
	opus := modelByID(t, ms, "claude-opus-5-5")
	if opus.DisplayName != "Claude Opus 5.5" || opus.DefaultEffort != "medium" || opus.ContextTokens != 300000 {
		t.Errorf("opus = %+v", opus)
	}
	if m := modelByID(t, ms, "gemini-4-neon"); m.DisplayName != "Gemini 4 Neon" || m.DefaultEffort != "high" {
		t.Errorf("neon = %+v", m)
	}
	if m := modelByID(t, ms, "grok-4.7"); m.DisplayName != "Grok 4.7" || m.DefaultEffort != "xhigh" {
		t.Errorf("grok-4.7 = %+v", m)
	}
	if m := modelByID(t, ms, "gpt-5.5"); m.DefaultEffort != "medium" {
		t.Errorf("gpt-5.5 = %+v", m)
	}
	sel, _ := cat.resolve("claude-opus-5-5", "high")
	if sel.ModelID != "claude-opus-5-5[context=300k,effort=medium,fast=false]" || sel.Effort != "medium" || sel.Note == "" {
		t.Errorf("resolve = %+v", sel)
	}
}

func TestACPCopilotFixture(t *testing.T) {
	init, res := loadFixture(t, "copilot")
	if cmd, ok := init.AuthMethods[0].terminalAuth(); !ok || !strings.HasSuffix(cmd, "copilot login") {
		t.Errorf("terminal-auth = %q", cmd)
	}
	if r := copilotAdapter.reloginFor(Account{Binary: "/x/copilot"}, &init); !strings.HasSuffix(r.Command, "copilot login") {
		t.Errorf("relogin = %+v", r)
	}
	if len(buildCatalog(res).Entries) != 0 {
		t.Error("copilot lists no models")
	}
	if res.Modes == nil || len(res.Modes.AvailableModes) != 3 || res.Modes.CurrentModeID != copilotModeURL+"agent" {
		t.Errorf("modes = %+v", res.Modes)
	}
	if o, ok := findConfig(res.ConfigOptions, "allow_all"); !ok || o.current() != "off" || !o.has("on") {
		t.Errorf("allow_all = %+v", o)
	}
	for _, ap := range copilotAdapter.Approvals {
		if ap.ACPMode == "" {
			continue
		}
		found := false
		for _, m := range res.Modes.AvailableModes {
			found = found || m.ID == ap.ACPMode
		}
		if !found {
			t.Errorf("copilot approval %s maps to unknown mode %s", ap.Mode.ID, ap.ACPMode)
		}
	}
}

func TestACPModesMatchFixtures(t *testing.T) {
	for _, agent := range []string{"agy", "cursor"} {
		_, res := loadFixture(t, agent)
		for _, ap := range acpAdapters[agent].Approvals {
			found := false
			for _, m := range res.Modes.AvailableModes {
				found = found || m.ID == ap.ACPMode
			}
			if !found {
				t.Errorf("%s approval %s maps to unknown mode %q", agent, ap.Mode.ID, ap.ACPMode)
			}
		}
	}
}

func TestEffortParsing(t *testing.T) {
	for in, want := range map[string][2]string{
		"gemini-4-neon-high":     {"gemini-4-neon", "high"},
		"claude-opus-5-5-low":    {"claude-opus-5-5", "low"},
		"gpt-5.5-extra-high":     {"gpt-5.5", "xhigh"},
		"gemini-3.8-flash":       {"gemini-3.8-flash", ""},
		"gpt-oss-120b-medium":    {"gpt-oss-120b", "medium"},
		"claude-opus-5-5-max":    {"claude-opus-5-5", "max"},
		"gemini-4-hydrogen-none": {"gemini-4-hydrogen", "none"},
	} {
		b, e := splitEffortSuffix(in)
		if b != want[0] || e != want[1] {
			t.Errorf("splitEffortSuffix(%q) = %q,%q", in, b, e)
		}
	}
	base, params, ok := parseBracketModel("grok-4.7[context=256k,reasoning_effort=xhigh,fast=false]")
	if !ok || base != "grok-4.7" || bracketEffort(params) != "xhigh" || parseTokenCount(params["context"]) != 256000 {
		t.Errorf("bracket = %s %v", base, params)
	}
	if _, p, _ := parseBracketModel("gpt-5.5[context=272k,reasoning=medium]"); bracketEffort(p) != "medium" {
		t.Errorf("reasoning param = %v", p)
	}
	if got := stripEffortFromName("Gemini 4 Neon  Extra High", "xhigh"); got != "Gemini 4 Neon" {
		t.Errorf("strip = %q", got)
	}
	if got := stripEffortFromName("Gemini 3.8 Flash (Medium)", "medium"); got != "Gemini 3.8 Flash" {
		t.Errorf("strip = %q", got)
	}
}

func TestClassifyAndUsageWindow(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	for text, want := range map[string]string{
		"Authentication required. Please run 'agent login' first":                         protocol.ErrKindAuth,
		"Not signed in. To authenticate without a browser, run: grok login --device-code": protocol.ErrKindAuth,
		"OAuth token has expired":                               protocol.ErrKindAuth,
		"Individual quota reached for model. Resets in 2h27m4s": protocol.ErrKindUsageLimit,
		"You've reached your monthly limit of premium requests": protocol.ErrKindUsageLimit,
		"429 Too Many Requests":                                 protocol.ErrKindRateLimit,
		"The model is overloaded, try again later":              protocol.ErrKindRateLimit,
		"Invalid model value: bogus":                            protocol.ErrKindModel,
		"Cannot use this model: x. Available models: a, b":      protocol.ErrKindModel,
		"something else entirely":                               "",
	} {
		if got := classifyText(text); got != want {
			t.Errorf("classifyText(%q) = %q, want %q", text, got, want)
		}
	}
	at, win := parseUsageWindow("Individual quota reached. Resets in 2h27m4s", now)
	if at == nil || !at.Equal(now.Add(2*time.Hour+27*time.Minute+4*time.Second)) || win != "" {
		t.Errorf("resets in = %v %q", at, win)
	}
	at, win = parseUsageWindow("You have hit your 5-hour limit. Resets at 3:40 PM", now)
	if at == nil || at.Hour() != 15 || at.Minute() != 40 || win != "five_hour" {
		t.Errorf("resets at clock = %v %s", at, win)
	}
	at, win = parseUsageWindow("Weekly usage limit reached, resets at 2026-11-25T00:00:00Z", now)
	if at == nil || at.Day() != 25 || win != "seven_day" {
		t.Errorf("resets at date = %v %s", at, win)
	}
	at, _ = parseUsageWindow("limit reached; resets at 9am", now)
	if at == nil || at.Day() != 24 || at.Hour() != 9 {
		t.Errorf("next-day clock = %v", at)
	}
	if _, win = parseUsageWindow("monthly limit reached", now); win != "monthly" {
		t.Errorf("window = %q", win)
	}
}

func TestClassifyErrors(t *testing.T) {
	ec := errContext{ad: cursorAdapter, acct: Account{Label: "Cursor", Binary: "/opt/bin/cursor-agent", Env: map[string]string{"CURSOR_API_KEY": "key-abcdefgh123", "CURSOR_CONFIG_DIR": "/c"}}}
	e := ec.classify(&rpcError{Code: -32000, Message: "Authentication required", Data: jsontext.Value(`{"message":"Please run 'agent login' first"}`)})
	if e.Kind != protocol.ErrKindAuth || e.Relogin.Command != "cursor-agent login" || e.Relogin.Env["CURSOR_CONFIG_DIR"] != "/c" || e.Relogin.Env["CURSOR_API_KEY"] != "" {
		t.Errorf("auth = %+v %+v", e, e.Relogin)
	}
	if e := ec.classify(&rpcError{Code: -32602, Message: "Invalid params", Data: jsontext.Value(`{"message":"Invalid model value: x"}`)}); e.Kind != protocol.ErrKindModel {
		t.Errorf("model = %+v", e)
	}
	if e := ec.classify(&protocolError{msg: "bad json"}); e.Kind != protocol.ErrKindProtocol {
		t.Errorf("protocol = %+v", e)
	}
	if e := ec.classify(&NotInstalledError{Binary: "x"}); e.Kind != protocol.ErrKindNotFound {
		t.Errorf("not installed = %+v", e)
	}
	if e := ec.classify(errors.New("upstream said key-abcdefgh123 is wrong")); strings.Contains(e.Message, "key-abcdefgh123") {
		t.Errorf("secret leaked: %q", e.Message)
	}
}

func TestRedactSecrets(t *testing.T) {
	in := "Authorization: Bearer abcdefghijklmnop token=xyz12345 ghp_aaaaaaaaaaaaaaaaaaaaaaaa xai-abcdefghijklmnopqrst eyJhbGciOiJIUzI1.eyJzdWIiOiIxMjM0.c2lnbmF0dXJlMTIz mysecretvalue"
	out := RedactSecrets(in, "mysecretvalue")
	for _, leak := range []string{"abcdefghijklmnop", "xyz12345", "ghp_aaaa", "xai-abcd", "eyJhbGci", "mysecretvalue"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q in %q", leak, out)
		}
	}
	if s := EnvSecrets(map[string]string{"GH_TOKEN": "t1", "HOME": "/h", "XAI_API_KEY": "k"}); len(s) != 2 {
		t.Errorf("EnvSecrets = %v", s)
	}
}

func TestParseCursorStatus(t *testing.T) {
	st, email, _ := parseCursorStatus(`{"status":"authenticated","isAuthenticated":true,"userInfo":{"email":"a@example.com"}}`)
	if st != protocol.AuthOK || email != "a@example.com" {
		t.Errorf("ok = %s %s", st, email)
	}
	st, _, detail := parseCursorStatus("noise\n{\n \"status\": \"unauthenticated\",\n \"isAuthenticated\": false,\n \"message\": \"Not logged in\"\n}")
	if st != protocol.AuthLoggedOut || detail != "Not logged in" {
		t.Errorf("logged out = %s %s", st, detail)
	}
	if st, _, _ := parseCursorStatus("garbage"); st != protocol.AuthUnknown {
		t.Errorf("garbage = %s", st)
	}
}

func TestMergeEnv(t *testing.T) {
	got := MergeEnv([]string{"A=1", "B=2"}, map[string]string{"B": "3", "C": "4"})
	if strings.Join(got, ",") != "A=1,B=3,C=4" {
		t.Errorf("MergeEnv = %v", got)
	}
}
