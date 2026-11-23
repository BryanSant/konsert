package chat

// Probe: capabilities of an ACP account without sending a prompt. initialize + session/new
// in a scratch directory give the version, auth methods, models with efforts and modes;
// the probe session is closed (session/close where supported) and the files it left in
// the CLI's home are removed. Results are cached for acpProbeTTL.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

type acpProbeEntry struct {
	mu  sync.Mutex
	at  time.Time
	res protocol.AgentAccount
}

func probeKey(acct Account) string {
	h := sha256.New()
	h.Write([]byte(acct.ID + "\x00" + acct.Binary + "\x00" + strings.Join(acct.ArgsPrefix, "\x01")))
	keys := make([]string, 0, len(acct.Env))
	for k := range acct.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte("\x00" + k + "=" + acct.Env[k]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Probe reports the account's capabilities (cached for a few minutes; concurrent probes of
// one account share a single run).
func (e *acpEngine) Probe(ctx context.Context, acct Account) protocol.AgentAccount {
	key := probeKey(acct)
	e.probeMu.Lock()
	ent := e.probes[key]
	if ent == nil {
		ent = &acpProbeEntry{}
		e.probes[key] = ent
	}
	e.probeMu.Unlock()

	ent.mu.Lock()
	defer ent.mu.Unlock()
	if !ent.at.IsZero() && time.Since(ent.at) < acpProbeTTL {
		return ent.res
	}
	res := e.probe(ctx, acct)
	// Only healthy results are cached: a logged-out or failing account is re-probed on the
	// next call, so a fresh login shows up immediately.
	if ctx.Err() == nil && res.Auth.State == protocol.AuthOK {
		ent.at, ent.res = time.Now(), res
	} else {
		ent.at = time.Time{}
	}
	return res
}

func (e *acpEngine) baseAccount(acct Account) protocol.AgentAccount {
	label := acct.Label
	if label == "" {
		label = e.ad.Label
	}
	id := acct.ID
	if id == "" {
		id = e.ad.Agent
	}
	return protocol.AgentAccount{
		ID: id, Agent: e.ad.Agent, Label: label, IsDefault: acct.IsDefault,
		Auth:            protocol.AgentAuthStatus{State: protocol.AuthUnknown},
		Approvals:       e.ad.modes(),
		DefaultApproval: e.ad.DefaultApproval,
		UsageNote:       e.ad.UsageNote,
		ChatSupported:   true,
		CheckedAt:       time.Now(),
	}
}

func (e *acpEngine) probe(ctx context.Context, acct Account) protocol.AgentAccount {
	out := e.baseAccount(acct)

	// Optional cheap CLI status check, in parallel with the ACP handshake.
	type status struct{ state, email, detail string }
	var stCh chan status
	if e.ad.statusCheck != nil {
		stCh = make(chan status, 1)
		go func() {
			st, em, d := e.ad.statusCheck(ctx, acct)
			stCh <- status{st, em, d}
		}()
	}

	// Agents whose initialize carries no version (cursor) get it from --version.
	var verCh chan string
	if len(e.ad.VersionArgs) > 0 {
		verCh = make(chan string, 1)
		go func() {
			v, _, _ := RunQuick(ctx, e.binary(acct), append(append([]string{}, acct.ArgsPrefix...), e.ad.VersionArgs...), acct.Env, 10*time.Second)
			verCh <- strings.TrimSpace(strings.SplitN(strings.TrimSpace(v), "\n", 2)[0])
		}()
	}

	res, init, err := e.probeACP(ctx, acct)
	if init != nil {
		out.Version = init.version()
	}
	if verCh != nil {
		if v := <-verCh; out.Version == "" && len(v) < 80 {
			out.Version = v
		}
	}
	if err == nil {
		out.Auth = protocol.AgentAuthStatus{State: protocol.AuthOK}
		e.fillFromSession(&out, res)
	} else {
		ce := errContext{ad: e.ad, acct: acct, init: init, phase: "probe"}.classifyPlain(err)
		switch ce.Kind {
		case protocol.ErrKindAuth:
			out.Auth = protocol.AgentAuthStatus{State: protocol.AuthLoggedOut, Detail: ce.Message, Relogin: e.ad.reloginFor(acct, init)}
		case protocol.ErrKindNotFound:
			out.Auth = protocol.AgentAuthStatus{State: protocol.AuthUnknown, Detail: ce.Message}
			out.ChatSupported = false
		default:
			out.Auth = protocol.AgentAuthStatus{State: protocol.AuthUnknown, Detail: ce.Message}
		}
		if len(e.ad.FallbackModels) > 0 {
			out.Models, out.ModelsSource = e.ad.FallbackModels, "fallback"
		}
	}
	if stCh != nil {
		st := <-stCh
		switch {
		case st.state == protocol.AuthLoggedOut && out.Auth.State != protocol.AuthLoggedOut:
			// The CLI says logged out; a session/new that still worked means a cached
			// credential is about to fail. Trust the explicit status.
			out.Auth = protocol.AgentAuthStatus{State: protocol.AuthLoggedOut, Detail: firstNonEmpty(st.detail, "Not logged in"), Relogin: e.ad.reloginFor(acct, init)}
		case st.state == protocol.AuthOK && out.Auth.State == protocol.AuthUnknown:
			out.Auth.State = protocol.AuthOK
		}
		if out.Auth.State == protocol.AuthOK && st.email != "" {
			out.Auth.Email = st.email
		}
	}
	if out.Auth.State == protocol.AuthOK && out.Auth.Method == "" {
		out.Auth.Method = "oauth"
	}
	out.CheckedAt = time.Now()
	return out
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// fillFromSession maps models, the default model and plan details from session/new.
func (e *acpEngine) fillFromSession(out *protocol.AgentAccount, res *acpSessionResult) {
	cat := buildCatalog(res)
	if len(cat.Entries) > 0 {
		out.Models, out.ModelsSource, out.DefaultModel = cat.Models(), "discovered", cat.CurrentModel
	} else if len(e.ad.FallbackModels) > 0 {
		out.Models, out.ModelsSource = e.ad.FallbackModels, "fallback"
		for _, m := range e.ad.FallbackModels {
			if m.IsDefault {
				out.DefaultModel = m.ID
			}
		}
	}
	// agy reports the subscription tier in session/new _meta.antigravity.tier.name.
	var meta struct {
		Antigravity struct {
			Tier struct {
				Name string `json:"name"`
			} `json:"tier"`
		} `json:"antigravity"`
	}
	if len(res.Meta) > 0 && unmarshal(res.Meta, &meta) == nil && meta.Antigravity.Tier.Name != "" {
		out.Plan = meta.Antigravity.Tier.Name
	}
}

// probeACP runs initialize + session/new in a scratch dir and tears everything down.
func (e *acpEngine) probeACP(ctx context.Context, acct Account) (*acpSessionResult, *acpInitResult, error) {
	dir, err := ScratchDir(acpProbePrefix + e.ad.Agent + "-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)

	// Agent requests during a probe (none expected) are refused.
	var self atomic.Pointer[acpConn]
	conn, err := e.start(ctx, acct, dir, acpLaunch{}, func(method string, id jsontext.Value, _ jsontext.Value) {
		if c := self.Load(); c != nil && id != nil {
			_ = c.rpc.ReplyError(id, rpcMethodNotFound, "Method not found: "+method)
		}
	})
	if conn == nil {
		return nil, nil, err
	}
	self.Store(conn)
	defer conn.kill()
	if err != nil {
		return nil, &conn.init, &probeErr{err: err, conn: conn}
	}
	sctx, cancel := context.WithTimeout(ctx, acpSessionTimeout)
	defer cancel()
	var res acpSessionResult
	if err := conn.rpc.Call(sctx, "session/new", map[string]any{"cwd": dir, "mcpServers": []any{}}, &res); err != nil {
		return nil, &conn.init, &probeErr{err: err, conn: conn}
	}
	if res.SessionID != "" {
		if conn.init.AgentCapabilities.SessionCapabilities.Close.set {
			cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
			_ = conn.rpc.Call(cctx, "session/close", map[string]any{"sessionId": res.SessionID}, nil)
			ccancel()
		}
		conn.kill()
		e.ad.removeSessionFiles(acct, res.SessionID, dir)
	}
	return &res, &conn.init, nil
}

// probeErr keeps the process so classification can read its exit code and stderr.
type probeErr struct {
	err  error
	conn *acpConn
}

func (p *probeErr) Error() string { return p.err.Error() }
func (p *probeErr) Unwrap() error { return p.err }

// classifyPlain classifies a probe error, using the probe process when available.
func (ec errContext) classifyPlain(err error) *protocol.AgentChatError {
	if pe, ok := err.(*probeErr); ok && pe.conn != nil {
		ec.proc = pe.conn.proc
		return ec.classify(pe.err)
	}
	return ec.classify(err)
}
