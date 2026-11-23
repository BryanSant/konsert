package chat

// acpEngine drives agent CLIs that speak the Agent Client Protocol (JSON-RPC 2.0 over
// stdio): grok (`grok agent stdio`), agy (`agy acp`), copilot (`copilot --acp`) and
// cursor (`cursor-agent acp`). One process per chat session; per-agent differences live
// in acp_agents.go.

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker/proctree"
)

// Tunables (variables so tests can shorten them).
var (
	acpInitTimeout    = 60 * time.Second
	acpSessionTimeout = 90 * time.Second
	acpControlTimeout = 30 * time.Second
	acpWatchdog       = 10 * time.Minute
	acpWatchdogTick   = 30 * time.Second
	acpKillGrace      = 3 * time.Second
	acpProbeTTL       = 5 * time.Minute
)

const (
	acpToolTextMax = 2000
	acpAttachMax   = 256 << 10
	acpClientName  = "konsert"
)

// NewACPEngine returns the engine for one ACP agent (grok, agy, copilot, cursor).
func NewACPEngine(agent string) (Engine, error) {
	ad, ok := acpAdapters[agent]
	if !ok {
		return nil, fmt.Errorf("acp: unknown agent %q", agent)
	}
	return &acpEngine{ad: ad, probes: map[string]*acpProbeEntry{}}, nil
}

type acpEngine struct {
	ad *acpAdapter

	probeMu sync.Mutex
	probes  map[string]*acpProbeEntry
}

func (e *acpEngine) Agent() string { return e.ad.Agent }

// --- process + handshake ----------------------------------------------------------------

// acpConn is one running agent process with its JSON-RPC connection.
type acpConn struct {
	proc *Proc
	rpc  *rpcConn
	init acpInitResult
	argv []string
}

func (e *acpEngine) binary(acct Account) string {
	if acct.Binary != "" {
		return acct.Binary
	}
	return e.ad.DefaultBinary
}

// start launches the agent and runs initialize. handler receives agent→client traffic.
func (e *acpEngine) start(ctx context.Context, acct Account, cwd string, l acpLaunch, handler rpcHandler) (*acpConn, error) {
	argv := append(append([]string{}, acct.ArgsPrefix...), e.ad.argv(l)...)
	proc, err := StartProc(ProcSpec{Binary: e.binary(acct), Args: argv, Dir: cwd, Env: acct.Env})
	if err != nil {
		return nil, err
	}
	c := &acpConn{proc: proc, argv: argv}
	c.rpc = newRPCConn(proc.Stdout, proc.Stdin, handler)
	ictx, cancel := context.WithTimeout(ctx, acpInitTimeout)
	defer cancel()
	err = c.rpc.Call(ictx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			// The agent uses its own file and terminal tools.
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]any{"name": acpClientName, "title": "Konsert", "version": "1"},
	}, &c.init)
	if err != nil {
		return c, err
	}
	return c, nil
}

func (c *acpConn) kill() {
	if c != nil && c.proc != nil {
		c.proc.Kill(acpKillGrace)
	}
}

// bridgeCommand is the executable used to bridge HTTP MCP servers to stdio for agents
// that don't accept HTTP MCP (konsertd itself: `konsertd mcp-bridge`). Tests override it.
var bridgeCommand = func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// mcpServers converts opts.MCPServers to ACP session/new entries: HTTP when the agent
// advertises mcpCapabilities.http, otherwise a stdio entry running `konsertd mcp-bridge`
// with the URL and headers in its environment (agy accepts only stdio MCP over ACP).
func mcpServers(init *acpInitResult, servers []protocol.AgentMCPServer) []any {
	out := []any{}
	if !init.AgentCapabilities.MCPCapabilities.HTTP {
		exe := bridgeCommand()
		if exe == "" {
			return out
		}
		for _, s := range servers {
			hdr, _ := marshalLine(s.Headers)
			out = append(out, map[string]any{
				"name":    s.Name,
				"command": exe,
				"args":    []string{"mcp-bridge"},
				"env": []map[string]string{
					{"name": "KONSERT_MCP_BRIDGE_URL", "value": s.URL},
					{"name": "KONSERT_MCP_BRIDGE_HEADERS", "value": string(hdr)},
				},
			})
		}
		return out
	}
	for _, s := range servers {
		headers := []map[string]string{}
		for k, v := range s.Headers {
			headers = append(headers, map[string]string{"name": k, "value": v})
		}
		out = append(out, map[string]any{"type": "http", "name": s.Name, "url": s.URL, "headers": headers})
	}
	return out
}

// --- session ----------------------------------------------------------------------------

type acpPerm struct {
	rpcID   jsontext.Value
	options []protocol.AgentElicitationOption
	tool    *protocol.AgentToolCall
}

type acpTurn struct {
	id        string
	msgN      int
	msgID     string
	text      strings.Builder
	lastEvent time.Time
	cancelled bool
	timedOut  bool
	tools     map[string]*protocol.AgentToolCall
}

// acpSession is one live chat. Locking: opMu serializes operations that talk to the agent
// and may block (connect, restart, set_model/set_mode); mu guards state shared with the
// JSON-RPC read loop and is never held across an RPC call, because agents send
// notifications (config_option_update, current_mode_update) before their responses.
type acpSession struct {
	eng  *acpEngine
	ad   *acpAdapter
	acct Account
	opts protocol.AgentChatOpen
	emit Emit

	opMu sync.Mutex

	mu          sync.Mutex
	suspended   bool // process tree frozen by the user; the watchdog does not run
	conn        *acpConn
	sessionID   string
	sess        acpSessionResult
	catalog     acpCatalog
	model       string // user-facing model id
	effort      string
	approval    string // Konsert approval id
	launch      acpLaunch
	needRestart bool
	restartWhy  string
	turn        *acpTurn
	perms       map[string]*acpPerm
	permN       int
	loading     bool // session/load replay in progress: drop replayed updates
	sysPending  string
	commands    []string
	closed      bool
	dead        bool // process exited outside a turn
	connecting  bool // connect is running: its own calls report an exit
	stopWatch   chan struct{}
	// watchdog settings, captured at Open.
	wdAfter, wdTick time.Duration
}

// Open starts (or resumes) a session.
func (e *acpEngine) Open(ctx context.Context, acct Account, opts protocol.AgentChatOpen, emit Emit) (Session, error) {
	s := &acpSession{eng: e, ad: e.ad, acct: acct, opts: opts, emit: emit, perms: map[string]*acpPerm{}, stopWatch: make(chan struct{}),
		wdAfter: acpWatchdog, wdTick: acpWatchdogTick}
	if _, ok := e.ad.approval(opts.ApprovalMode); !ok {
		ce := &protocol.AgentChatError{Kind: protocol.ErrKindOther, Message: fmt.Sprintf("%s has no approval mode %q", e.ad.Label, opts.ApprovalMode)}
		s.fail(ce)
		return nil, &acpOpenError{ce}
	}
	s.approval = opts.ApprovalMode
	if s.approval == "" {
		s.approval = e.ad.DefaultApproval
	}
	s.model, s.effort = opts.Model, normalizeEffort(opts.Effort)
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusStarting})

	s.opMu.Lock()
	err := s.connect(ctx, opts.AgentSessionID)
	s.opMu.Unlock()
	if err != nil {
		s.fail(err.chatErr)
		return nil, err
	}
	go s.watchdog()
	s.emitReady()
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusIdle})
	return s, nil
}

type acpOpenError struct{ chatErr *protocol.AgentChatError }

func (e *acpOpenError) Error() string { return e.chatErr.Kind + ": " + e.chatErr.Message }

func (s *acpSession) ecFor(conn *acpConn, phase string) errContext {
	ec := errContext{ad: s.ad, acct: s.acct, phase: phase}
	if conn != nil {
		ec.proc = conn.proc
		ec.init = &conn.init
	}
	return ec
}

// abort detaches conn, classifies err and kills conn.
func (s *acpSession) abort(conn *acpConn, phase string, err error) *acpOpenError {
	s.mu.Lock()
	if s.conn == conn {
		s.conn = nil
	}
	s.mu.Unlock()
	ce := s.ecFor(conn, phase).classify(err)
	conn.kill()
	return &acpOpenError{ce}
}

// connect launches the process, initializes, creates or resumes the agent session and
// applies model/effort/approval. Requires opMu. On failure the process is gone.
func (s *acpSession) connect(ctx context.Context, resumeID string) *acpOpenError {
	s.mu.Lock()
	s.launch = acpLaunch{Approval: s.launchApprovalLocked()}
	if s.ad.LaunchModel {
		s.launch.Model, s.launch.Effort = s.model, s.effort
	}
	launch := s.launch
	// An agent may answer a call with an error (e.g. auth required) and exit at
	// once; monitor must leave reporting to connect, which classifies the reply.
	s.connecting = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.connecting = false
		s.mu.Unlock()
	}()

	conn, err := s.eng.start(ctx, s.acct, s.opts.Cwd, launch, s.handle)
	if err != nil {
		if conn == nil {
			return &acpOpenError{s.ecFor(nil, "launch").classify(err)}
		}
		return s.abort(conn, "initialize", err)
	}
	s.mu.Lock()
	s.conn = conn
	s.dead = false
	s.mu.Unlock()
	go s.monitor(conn)

	init := &conn.init
	mcp := mcpServers(init, s.opts.MCPServers)
	if len(s.opts.MCPServers) > 0 && len(mcp) == 0 {
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusStarting,
			Text: s.ad.Label + " could not be given Konsert's tools (ask_user, fleet) in this chat."})
	}
	resumed := false
	if resumeID != "" {
		resumed, err = s.resume(ctx, conn, resumeID, mcp)
		if err != nil {
			ce := s.ecFor(conn, "session/load").classify(err)
			if ce.Kind == protocol.ErrKindAuth || ce.Kind == protocol.ErrKindExit {
				return s.abort(conn, "session/load", err)
			}
			s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusStarting,
				Text: "Could not resume the previous " + s.ad.Label + " session (" + ce.Message + "); starting a new one."})
		}
	}
	if !resumed {
		params := map[string]any{"cwd": s.opts.Cwd, "mcpServers": mcp}
		meta := s.sessionMeta()
		sysPending := ""
		if sp := strings.TrimSpace(s.opts.SystemPrompt); sp != "" {
			if init.systemPromptAppend() {
				meta["antigravity"] = map[string]any{"systemPrompt": map[string]any{"append": sp}}
			} else {
				sysPending = sp
			}
		}
		if len(meta) > 0 {
			params["_meta"] = meta
		}
		sctx, cancel := context.WithTimeout(ctx, acpSessionTimeout)
		var res acpSessionResult
		err = conn.rpc.Call(sctx, "session/new", params, &res)
		cancel()
		if err == nil && res.SessionID == "" {
			err = &protocolError{msg: "session/new returned no sessionId"}
		}
		if err != nil {
			return s.abort(conn, "session/new", err)
		}
		s.mu.Lock()
		s.sess, s.sessionID, s.sysPending = res, res.SessionID, sysPending
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.catalog = buildCatalog(&s.sess)
	s.mu.Unlock()
	if err := s.applyModel(ctx); err != nil {
		return s.abort(conn, "set model", err)
	}
	if err := s.applyApproval(ctx); err != nil {
		return s.abort(conn, "set mode", err)
	}
	// Every call succeeded, but the process may have exited since (monitor
	// stays quiet while connecting).
	select {
	case <-conn.proc.Done():
		return s.abort(conn, "startup", errRPCClosed)
	default:
	}
	return nil
}

// resume tries session/resume (no history replay) then session/load (replayed updates
// are dropped).
func (s *acpSession) resume(ctx context.Context, conn *acpConn, id string, mcp []any) (bool, error) {
	caps := conn.init.AgentCapabilities
	params := map[string]any{"sessionId": id, "cwd": s.opts.Cwd, "mcpServers": mcp}
	if meta := s.sessionMeta(); len(meta) > 0 {
		params["_meta"] = meta
	}
	var res acpSessionResult
	sctx, cancel := context.WithTimeout(ctx, acpSessionTimeout)
	defer cancel()
	var err error
	switch {
	case caps.SessionCapabilities.Resume.set:
		err = conn.rpc.Call(sctx, "session/resume", params, &res)
	case caps.LoadSession || caps.SessionCapabilities.Load.set:
		s.mu.Lock()
		s.loading = true
		s.mu.Unlock()
		err = conn.rpc.Call(sctx, "session/load", params, &res)
		s.mu.Lock()
		s.loading = false
		s.mu.Unlock()
	default:
		return false, errors.New("agent cannot resume sessions")
	}
	if err != nil {
		return false, err
	}
	res.SessionID = id
	s.mu.Lock()
	// Keep what the replay reported (modes, config) when resume/load omits it.
	if res.Models == nil {
		res.Models = s.sess.Models
	}
	if res.Modes == nil {
		res.Modes = s.sess.Modes
	}
	if res.ConfigOptions == nil {
		res.ConfigOptions = s.sess.ConfigOptions
	}
	s.sess, s.sessionID, s.sysPending = res, id, ""
	s.mu.Unlock()
	return true, nil
}

// sessionMeta is the adapter's _meta for the current approval mode.
func (s *acpSession) sessionMeta() map[string]any {
	meta := map[string]any{}
	if s.ad.sessionMeta != nil {
		s.mu.Lock()
		approval := s.approval
		s.mu.Unlock()
		for k, v := range s.ad.sessionMeta(approval) {
			meta[k] = v
		}
	}
	return meta
}

// launchApprovalLocked is the approval id whose launch flags the process runs with.
func (s *acpSession) launchApprovalLocked() string {
	if ap, ok := s.ad.approval(s.approval); ok && ap.Launch {
		return ap.Mode.ID
	}
	return ""
}

// call sends a session-scoped request (without holding mu).
func (s *acpSession) call(ctx context.Context, method string, params map[string]any, result any) error {
	s.mu.Lock()
	conn, sid := s.conn, s.sessionID
	s.mu.Unlock()
	if conn == nil {
		return errRPCClosed
	}
	params["sessionId"] = sid
	cctx, cancel := context.WithTimeout(ctx, acpControlTimeout)
	defer cancel()
	return conn.rpc.Call(cctx, method, params, result)
}

// applyModel applies the wanted model and effort to the live session. Requires opMu.
func (s *acpSession) applyModel(ctx context.Context) error {
	s.mu.Lock()
	if s.ad.LaunchModel {
		// copilot: model/effort were passed at launch.
		if s.model == "" {
			s.model = "auto"
		}
		s.mu.Unlock()
		return nil
	}
	cat, model, effort := s.catalog, s.model, s.effort
	hasModels := s.sess.Models != nil
	curID := ""
	if hasModels {
		curID = s.sess.Models.CurrentModelID
	}
	cfgs := s.sess.ConfigOptions
	s.mu.Unlock()
	if len(cat.Entries) == 0 {
		return nil
	}
	sel, ok := cat.resolve(model, effort)
	if !ok {
		return &rpcError{Code: rpcInvalidParams, Message: "Invalid model", Data: jsontext.Value(strconv.Quote("unknown model " + model))}
	}
	if sel.ModelID != "" && sel.ModelID != curID {
		var err error
		if hasModels {
			err = s.call(ctx, "session/set_model", map[string]any{"modelId": sel.ModelID}, nil)
		} else {
			_, err = s.setConfig(ctx, "model", sel.ModelID)
		}
		if err != nil {
			return err
		}
		s.mu.Lock()
		if s.sess.Models != nil {
			s.sess.Models.CurrentModelID = sel.ModelID
		}
		cfgs = s.sess.ConfigOptions
		s.mu.Unlock()
	}
	if sel.EffortConfig != "" {
		if o, ok := findConfig(cfgs, sel.EffortConfig); !ok || o.current() != sel.Effort {
			if _, err := s.setConfig(ctx, sel.EffortConfig, sel.Effort); err != nil {
				return err
			}
		}
	}
	s.mu.Lock()
	s.catalog.CurrentModel, s.catalog.CurrentEffort = sel.Model, sel.Effort
	s.model, s.effort = sel.Model, sel.Effort
	s.mu.Unlock()
	if sel.Note != "" {
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: s.currentStatus(), Text: sel.Note})
	}
	return nil
}

func (s *acpSession) setConfig(ctx context.Context, id, value string) ([]acpConfigOption, error) {
	var res struct {
		ConfigOptions []acpConfigOption `json:"configOptions"`
	}
	if err := s.call(ctx, "session/set_config_option", map[string]any{"configId": id, "value": value}, &res); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(res.ConfigOptions) > 0 {
		s.sess.ConfigOptions = res.ConfigOptions
	}
	return s.sess.ConfigOptions, nil
}

// applyApproval applies the wanted approval mode. Requires opMu.
func (s *acpSession) applyApproval(ctx context.Context) error {
	s.mu.Lock()
	ap, _ := s.ad.approval(s.approval)
	cur, hasModes := "", s.sess.Modes != nil
	if hasModes {
		cur = s.sess.Modes.CurrentModeID
	}
	_, modeCfg := findConfig(s.sess.ConfigOptions, "mode")
	s.mu.Unlock()
	if ap.ACPMode != "" && cur != ap.ACPMode {
		var err error
		if !hasModes && modeCfg {
			_, err = s.setConfig(ctx, "mode", ap.ACPMode)
		} else {
			err = s.call(ctx, "session/set_mode", map[string]any{"modeId": ap.ACPMode}, nil)
		}
		if err != nil {
			return err
		}
		s.mu.Lock()
		if s.sess.Modes == nil {
			s.sess.Modes = &acpModeState{}
		}
		s.sess.Modes.CurrentModeID = ap.ACPMode
		s.mu.Unlock()
	}
	for id, v := range ap.Config {
		s.mu.Lock()
		o, ok := findConfig(s.sess.ConfigOptions, id)
		s.mu.Unlock()
		if !ok || o.current() == v || !o.has(v) {
			continue
		}
		if _, err := s.setConfig(ctx, id, v); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.approval = ap.Mode.ID
	s.mu.Unlock()
	return nil
}

func (s *acpSession) emitEv(ev protocol.AgentChatEvent) {
	if s.emit != nil {
		s.emit(ev)
	}
}

func (s *acpSession) emitReady() {
	s.mu.Lock()
	ev := protocol.AgentChatEvent{Kind: protocol.EventSessionReady, AgentSessionID: s.sessionID,
		Model: s.model, Effort: s.effort, ApprovalMode: s.approval}
	s.mu.Unlock()
	s.emitEv(ev)
}

func (s *acpSession) fail(ce *protocol.AgentChatError) {
	ev := protocol.AgentChatEvent{Kind: protocol.EventError, Error: ce}
	if ce.Kind == protocol.ErrKindAuth && ce.Relogin != nil {
		s.emitEv(ev)
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventElicitation, Elicitation: &protocol.AgentElicitation{
			ID: "auth", Kind: protocol.ElicitAuth, Title: ce.Message, Detail: "Run: " + ce.Relogin.Command,
		}})
	} else {
		s.emitEv(ev)
	}
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusFailed})
}

// monitor notices a process that exits while no turn is running.
func (s *acpSession) monitor(c *acpConn) {
	<-c.proc.Done()
	s.mu.Lock()
	if s.closed || s.conn != c {
		s.mu.Unlock()
		return
	}
	s.dead = true
	busy := s.turn != nil || s.connecting
	ec := s.ecFor(c, "idle")
	s.mu.Unlock()
	if busy {
		return // the turn's pending session/prompt (or connect's call) fails and reports it
	}
	ce := ec.classify(errRPCClosed)
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventError, Error: ce})
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusFailed,
		Text: s.ad.Label + " stopped; the next message restarts it and resumes the session."})
}

// AgentSessionID is the agent's session id.
func (s *acpSession) AgentSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// --- agent → client traffic -------------------------------------------------------------

func (s *acpSession) handle(method string, id jsontext.Value, params jsontext.Value) {
	switch method {
	case "session/update":
		var p acpUpdateParams
		if unmarshal(params, &p) != nil {
			return
		}
		var u acpUpdate
		if unmarshal(p.Update, &u) != nil {
			return
		}
		s.onUpdate(&u)
	case "session/request_permission":
		s.onPermission(id, params)
	default:
		if id == nil {
			return // unknown notification (_x.ai/..., github.com/copilot/...)
		}
		s.mu.Lock()
		c := s.conn
		s.mu.Unlock()
		if c == nil {
			return
		}
		// fs/* and terminal/* are not advertised; the agent uses its own tools.
		_ = c.rpc.ReplyError(id, rpcMethodNotFound, "Method not found: "+method)
	}
}

func (s *acpSession) onUpdate(u *acpUpdate) {
	s.mu.Lock()
	switch u.SessionUpdate {
	case "current_mode_update":
		if s.sess.Modes == nil {
			s.sess.Modes = &acpModeState{}
		}
		s.sess.Modes.CurrentModeID = u.CurrentModeID
		if a := s.ad.approvalForACPMode(u.CurrentModeID, s.approval); a != "" {
			s.approval = a
		}
		s.mu.Unlock()
		return
	case "config_option_update":
		if len(u.ConfigOptions) > 0 {
			s.sess.ConfigOptions = u.ConfigOptions
		}
		s.mu.Unlock()
		return
	case "available_commands_update":
		s.commands = s.commands[:0]
		for _, c := range u.AvailableCommands {
			s.commands = append(s.commands, c.Name)
		}
		s.mu.Unlock()
		return
	}
	t := s.turn
	if s.loading || t == nil {
		s.mu.Unlock()
		return
	}
	t.lastEvent = time.Now()
	turnID := t.id
	var evs []protocol.AgentChatEvent
	switch u.SessionUpdate {
	case "agent_message_chunk":
		text := blockText(u.Content)
		if text == "" {
			break
		}
		if t.msgID == "" {
			t.msgN++
			t.msgID = fmt.Sprintf("%s-m%d", t.id, t.msgN)
		}
		t.text.WriteString(text)
		evs = append(evs, protocol.AgentChatEvent{Kind: protocol.EventMessageDelta, MessageID: t.msgID, Text: text})
	case "agent_thought_chunk":
		if text := blockText(u.Content); text != "" {
			evs = append(evs, protocol.AgentChatEvent{Kind: protocol.EventThoughtDelta, Text: text})
		}
	case "tool_call", "tool_call_update":
		if ev, ok := t.flushMessage(); ok {
			evs = append(evs, ev)
		}
		tc := t.toolCall(u)
		cp := *tc
		evs = append(evs, protocol.AgentChatEvent{Kind: protocol.EventToolCall, ToolCall: &cp})
	case "plan":
		plan := make([]protocol.AgentPlanItem, 0, len(u.Entries))
		for _, e := range u.Entries {
			plan = append(plan, protocol.AgentPlanItem{Text: e.Content, Status: e.Status})
		}
		evs = append(evs, protocol.AgentChatEvent{Kind: protocol.EventPlan, Plan: plan})
	}
	s.mu.Unlock()
	for _, ev := range evs {
		ev.TurnID = turnID
		s.emitEv(ev)
	}
}

// flushMessage closes the current assistant message (coalesced deltas → EventMessage).
func (t *acpTurn) flushMessage() (protocol.AgentChatEvent, bool) {
	if t.msgID == "" {
		return protocol.AgentChatEvent{}, false
	}
	ev := protocol.AgentChatEvent{Kind: protocol.EventMessage, TurnID: t.id, MessageID: t.msgID, Text: t.text.String()}
	t.msgID = ""
	t.text.Reset()
	return ev, true
}

func (t *acpTurn) toolCall(u *acpUpdate) *protocol.AgentToolCall {
	if t.tools == nil {
		t.tools = map[string]*protocol.AgentToolCall{}
	}
	tc := t.tools[u.ToolCallID]
	if tc == nil {
		tc = &protocol.AgentToolCall{ID: u.ToolCallID, Status: "pending"}
		t.tools[u.ToolCallID] = tc
	}
	mergeToolCall(tc, u)
	return tc
}

// mergeToolCall applies a tool_call / tool_call_update (or a permission's toolCall).
func mergeToolCall(tc *protocol.AgentToolCall, u *acpUpdate) {
	if u.Title != nil && *u.Title != "" {
		tc.Title = *u.Title
	}
	if u.Kind != nil && *u.Kind != "" {
		tc.Kind = *u.Kind
	}
	if u.Status != nil {
		switch *u.Status {
		case "in_progress":
			tc.Status = "running"
		case "":
		default:
			tc.Status = *u.Status
		}
	}
	if tc.Name == "" {
		for _, k := range []string{"toolName", "tool_name", "name"} {
			var n string
			if raw, ok := u.Meta[k]; ok && unmarshal(raw, &n) == nil && n != "" {
				tc.Name = n
				break
			}
		}
		if tc.Name == "" {
			var cc struct {
				ToolName string `json:"toolName"`
			}
			if raw, ok := u.Meta["claudeCode"]; ok && unmarshal(raw, &cc) == nil {
				tc.Name = cc.ToolName
			}
		}
		if tc.Name == "" {
			tc.Name = tc.Kind
		}
	}
	if len(u.RawInput) > 0 && string(u.RawInput) != "null" {
		tc.Input = truncate(compactJSON(u.RawInput), acpToolTextMax)
	}
	if out := toolContentText(u.Content); out != "" {
		tc.Output = truncate(out, acpToolTextMax)
	} else if len(u.RawOutput) > 0 && string(u.RawOutput) != "null" {
		tc.Output = truncate(rawOutputText(u.RawOutput), acpToolTextMax)
	}
	for _, l := range u.Locations {
		if l.Path != "" && !contains(tc.Paths, l.Path) {
			tc.Paths = append(tc.Paths, l.Path)
		}
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func compactJSON(raw jsontext.Value) string {
	var s string
	if unmarshal(raw, &s) == nil {
		return s
	}
	// Compact in place of a decode/re-encode round trip: keeps the agent's member
	// order and does not HTML-escape.
	v := raw.Clone()
	if v.Compact(agentDecodeOpts) == nil {
		return string(v)
	}
	return string(raw)
}

func rawOutputText(raw jsontext.Value) string {
	var o map[string]any
	if unmarshal(raw, &o) == nil {
		for _, k := range []string{"output", "stdout", "content", "text", "result"} {
			if v, ok := o[k].(string); ok && v != "" {
				return v
			}
		}
	}
	return compactJSON(raw)
}

// blockText extracts text from one ContentBlock.
func blockText(raw jsontext.Value) string {
	var b acpContentBlock
	if unmarshal(raw, &b) != nil {
		return ""
	}
	if b.Type == "text" || b.Type == "" {
		return b.Text
	}
	return ""
}

// toolContentText renders ToolCallContent[] (content blocks, diffs, terminals) as text.
func toolContentText(raw jsontext.Value) string {
	if len(raw) == 0 || raw[0] != '[' {
		return ""
	}
	var items []acpContentBlock
	if unmarshal(raw, &items) != nil {
		return ""
	}
	var parts []string
	for _, it := range items {
		switch it.Type {
		case "content":
			if it.Content != nil && it.Content.Text != "" {
				parts = append(parts, it.Content.Text)
			} else if it.Content != nil && it.Content.URI != "" {
				parts = append(parts, it.Content.URI)
			}
		case "diff":
			parts = append(parts, diffSummary(it))
		case "terminal":
			parts = append(parts, "[terminal output]")
		case "text":
			parts = append(parts, it.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func diffSummary(d acpContentBlock) string {
	if d.OldText == nil || *d.OldText == "" {
		return fmt.Sprintf("created %s (%d lines)", d.Path, strings.Count(d.NewText, "\n")+1)
	}
	return fmt.Sprintf("edited %s (-%d +%d lines)", d.Path, strings.Count(*d.OldText, "\n")+1, strings.Count(d.NewText, "\n")+1)
}

func (s *acpSession) onPermission(id jsontext.Value, params jsontext.Value) {
	var p acpPermissionParams
	if unmarshal(params, &p) != nil {
		s.mu.Lock()
		c := s.conn
		s.mu.Unlock()
		if c != nil {
			_ = c.rpc.ReplyError(id, rpcInvalidParams, "invalid request_permission params")
		}
		return
	}
	var u acpUpdate
	_ = unmarshal(p.ToolCall, &u)
	tc := &protocol.AgentToolCall{ID: u.ToolCallID, Status: "pending"}
	mergeToolCall(tc, &u)

	s.mu.Lock()
	s.permN++
	eid := fmt.Sprintf("perm-%d", s.permN)
	perm := &acpPerm{rpcID: append(jsontext.Value{}, id...), tool: tc}
	for _, o := range p.Options {
		perm.options = append(perm.options, protocol.AgentElicitationOption{ID: o.OptionID, Label: o.Name, Kind: o.Kind})
	}
	s.perms[eid] = perm
	turnID := ""
	var flush []protocol.AgentChatEvent
	if s.turn != nil {
		turnID = s.turn.id
		s.turn.lastEvent = time.Now()
		if ev, ok := s.turn.flushMessage(); ok {
			flush = append(flush, ev)
		}
		if known := s.turn.tools[u.ToolCallID]; known != nil {
			mergeToolCall(known, &u)
			*tc = *known
		}
	}
	// Konsert's own MCP tools (ask_user, fleet tools) are trusted: answer the agent's
	// permission request ourselves instead of asking the user for permission to ask them.
	// Checked after merging the earlier tool_call update, which carries the identifying
	// rawInput (cursor sends only the id and title in the permission request itself).
	if allow := s.trustedToolAllowOption(tc, p.ToolCall, perm.options); allow != "" {
		delete(s.perms, eid)
		c := s.conn
		s.mu.Unlock()
		for _, ev := range flush {
			s.emitEv(ev)
		}
		if c != nil {
			_ = c.rpc.Reply(id, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": allow}})
		}
		return
	}
	s.mu.Unlock()

	title := tc.Title
	if title == "" {
		title = s.ad.Label + " asks for permission"
	}
	detail := tc.Input
	if len(tc.Paths) > 0 {
		detail = strings.TrimSpace(strings.Join(tc.Paths, ", ") + "\n" + detail)
	}
	for _, ev := range flush {
		s.emitEv(ev)
	}
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventElicitation, TurnID: turnID, Elicitation: &protocol.AgentElicitation{
		ID: eid, Kind: protocol.ElicitPermission, Title: title, Detail: truncate(detail, acpToolTextMax), ToolCall: tc, Options: perm.options,
	}})
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventStatus, TurnID: turnID, Status: protocol.ChatStatusNeedsYou, Text: title})
}

// trustedToolAllowOption returns the allow option to pick automatically when the permission
// request is for a tool of a Konsert-provided MCP server, or "" to ask the user. Agents
// identify MCP tools differently (cursor: rawInput providerIdentifier; others: names like
// "mcp__konsert__ask_user" or titles like "konsert: ask_user"). Caller holds s.mu.
func (s *acpSession) trustedToolAllowOption(tc *protocol.AgentToolCall, raw jsontext.Value, opts []protocol.AgentElicitationOption) string {
	if len(s.opts.MCPServers) == 0 {
		return ""
	}
	var probe struct {
		RawInput struct {
			ProviderIdentifier string `json:"providerIdentifier"`
			ToolName           string `json:"tool_name"`
		} `json:"rawInput"`
	}
	_ = unmarshal(raw, &probe)
	// The merged tool call's input is the agent's rawInput (e.g. cursor:
	// {"args":…,"providerIdentifier":"konsert","toolName":"ask_user"}).
	var merged struct {
		ProviderIdentifier string `json:"providerIdentifier"`
		ToolName           string `json:"toolName"`
	}
	_ = unmarshal([]byte(tc.Input), &merged)
	trusted := false
	for _, srv := range s.opts.MCPServers {
		name := strings.ToLower(strings.TrimSpace(srv.Name))
		if name == "" {
			continue
		}
		if strings.EqualFold(probe.RawInput.ProviderIdentifier, name) || strings.EqualFold(merged.ProviderIdentifier, name) {
			trusted = true
		}
		for _, v := range []string{tc.Name, tc.Title, probe.RawInput.ToolName} {
			v = strings.ToLower(strings.TrimSpace(v))
			// agy titles MCP calls "Run mcp_konsert_ask_user"; others use mcp__konsert__x.
			if strings.Contains(v, "mcp__"+name+"__") || strings.Contains(v, "mcp_"+name+"_") ||
				strings.HasPrefix(v, name+": ") || strings.HasPrefix(v, name+"__") || strings.HasPrefix(v, name+".") || strings.HasPrefix(v, name+"-") {
				trusted = true
			}
		}
	}
	if !trusted {
		return ""
	}
	for _, kind := range []string{"allow_once", "allow_always"} {
		for _, o := range opts {
			if o.Kind == kind {
				return o.ID
			}
		}
	}
	return ""
}

// --- Session API --------------------------------------------------------------------------

// Prompt starts a turn.
func (s *acpSession) Prompt(ctx context.Context, p protocol.AgentChatPrompt) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("session closed")
	}
	if s.turn != nil {
		s.mu.Unlock()
		return errors.New("a turn is already running")
	}
	restart, why := s.dead || s.needRestart || s.conn == nil, s.restartWhy
	s.mu.Unlock()
	if restart {
		if err := s.restart(ctx); err != nil {
			s.fail(err.chatErr)
			return err
		}
		s.emitReady()
		if why != "" {
			s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: protocol.ChatStatusIdle, Text: why})
		}
	}
	s.mu.Lock()
	if s.closed || s.turn != nil || s.conn == nil {
		s.mu.Unlock()
		return errors.New("session is not ready")
	}
	blocks, err := s.promptBlocksLocked(p)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	turnID := p.TurnID
	if turnID == "" {
		turnID = fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	t := &acpTurn{id: turnID, lastEvent: time.Now()}
	s.turn = t
	conn, sid := s.conn, s.sessionID
	s.sysPending = ""
	s.mu.Unlock()

	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventStatus, TurnID: turnID, Status: protocol.ChatStatusWorking})
	go s.runTurn(conn, t, sid, blocks)
	return nil
}

func (s *acpSession) runTurn(conn *acpConn, t *acpTurn, sid string, blocks []any) {
	var res acpPromptResult
	err := conn.rpc.Call(context.Background(), "session/prompt", map[string]any{"sessionId": sid, "prompt": blocks}, &res)

	s.mu.Lock()
	if s.turn != t {
		s.mu.Unlock()
		return
	}
	s.turn = nil
	var evs []protocol.AgentChatEvent
	if ev, ok := t.flushMessage(); ok {
		evs = append(evs, ev)
	}
	// Permission requests can't outlive their turn.
	for id, pr := range s.perms {
		_ = conn.rpc.Reply(pr.rpcID, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
		delete(s.perms, id)
		evs = append(evs, protocol.AgentChatEvent{Kind: protocol.EventElicitationOK, Elicitation: &protocol.AgentElicitation{ID: id, Kind: protocol.ElicitPermission}, Text: "cancelled"})
	}
	closed := s.closed
	ec := s.ecFor(conn, "prompt")
	cancelled := t.cancelled
	s.mu.Unlock()

	if closed {
		return
	}
	for _, ev := range evs {
		ev.TurnID = t.id
		s.emitEv(ev)
	}
	if err != nil {
		ce := ec.classify(err)
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: t.id, Error: ce})
		if ce.Kind == protocol.ErrKindAuth && ce.Relogin != nil {
			s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventElicitation, TurnID: t.id, Elicitation: &protocol.AgentElicitation{
				ID: "auth", Kind: protocol.ElicitAuth, Title: ce.Message, Detail: "Run: " + ce.Relogin.Command}})
		}
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: t.id, StopReason: "error"})
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventStatus, TurnID: t.id, Status: protocol.ChatStatusFailed})
		return
	}
	if res.Usage != nil {
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventUsage, TurnID: t.id, Usage: &protocol.AgentTokenUsage{
			InputTokens: res.Usage.InputTokens, OutputTokens: res.Usage.OutputTokens,
			CacheReadTokens: res.Usage.CachedReadTokens, CacheCreateTokens: res.Usage.CachedWriteTokens}})
	}
	stop := res.StopReason
	if stop == "" {
		stop = "end_turn"
	}
	if cancelled && stop != "cancelled" && stop != "end_turn" {
		stop = "cancelled"
	}
	status := protocol.ChatStatusDone
	if stop == "cancelled" || cancelled {
		status = protocol.ChatStatusCancelled
	}
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventTurnEnd, TurnID: t.id, StopReason: stop})
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventStatus, TurnID: t.id, Status: status})
}

// promptBlocksLocked builds the ACP prompt content: text (with a pending system prompt
// prepended) and attachments as image / embedded resource / resource_link blocks.
func (s *acpSession) promptBlocksLocked(p protocol.AgentChatPrompt) ([]any, error) {
	text := p.Text
	if s.sysPending != "" {
		text = "<system-instructions>\n" + s.sysPending + "\n</system-instructions>\n\n" + text
	}
	blocks := []any{map[string]any{"type": "text", "text": text}}
	caps := s.conn.init.AgentCapabilities.PromptCapabilities
	for _, path := range p.Attachments {
		abs := path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(s.opts.Cwd, path)
		}
		st, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("attachment %s: %w", path, err)
		}
		mt := mime.TypeByExtension(strings.ToLower(filepath.Ext(abs)))
		uri := "file://" + filepath.ToSlash(abs)
		if !strings.HasPrefix(uri, "file:///") {
			uri = "file:///" + strings.TrimPrefix(uri, "file://")
		}
		switch {
		case strings.HasPrefix(mt, "image/") && caps.Image && st.Size() <= 10<<20:
			b, err := os.ReadFile(abs)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, map[string]any{"type": "image", "mimeType": strings.Split(mt, ";")[0], "data": base64.StdEncoding.EncodeToString(b), "uri": uri})
		case caps.EmbeddedContext && st.Size() <= acpAttachMax && isTextFile(abs):
			b, err := os.ReadFile(abs)
			if err != nil {
				return nil, err
			}
			if mt == "" {
				mt = "text/plain"
			}
			blocks = append(blocks, map[string]any{"type": "resource", "resource": map[string]any{"uri": uri, "mimeType": strings.Split(mt, ";")[0], "text": string(b)}})
		default:
			blocks = append(blocks, map[string]any{"type": "resource_link", "uri": uri, "name": filepath.Base(abs), "size": st.Size()})
		}
	}
	return blocks, nil
}

func isTextFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	for _, c := range buf[:n] {
		if c == 0 {
			return false
		}
	}
	return true
}

// Respond answers a permission elicitation.
func (s *acpSession) Respond(ctx context.Context, r protocol.AgentChatRespond) error {
	s.mu.Lock()
	pr, ok := s.perms[r.ElicitationID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("no pending elicitation %q", r.ElicitationID)
	}
	outcome := map[string]any{"outcome": "cancelled"}
	chosen := ""
	switch r.OptionID {
	case "", "cancel", "cancelled":
	default:
		for _, o := range pr.options {
			if o.ID == r.OptionID {
				chosen = o.ID
			}
		}
		if chosen == "" { // a generic kind such as "allow_once"
			for _, o := range pr.options {
				if o.Kind == r.OptionID {
					chosen = o.ID
					break
				}
			}
		}
		if chosen == "" {
			s.mu.Unlock()
			return fmt.Errorf("elicitation %s has no option %q", r.ElicitationID, r.OptionID)
		}
		outcome = map[string]any{"outcome": "selected", "optionId": chosen}
	}
	delete(s.perms, r.ElicitationID)
	conn := s.conn
	turnID := ""
	if s.turn != nil {
		turnID = s.turn.id
		s.turn.lastEvent = time.Now()
	}
	pending := len(s.perms)
	s.mu.Unlock()
	if conn == nil {
		return errors.New("session not running")
	}
	// Report the resolution before replying: once the agent has the answer it may
	// end the turn at once, and neither event may follow turn_end.
	text := chosen
	if text == "" {
		text = "cancelled"
	}
	s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventElicitationOK, TurnID: turnID, Text: text,
		Elicitation: &protocol.AgentElicitation{ID: r.ElicitationID, Kind: protocol.ElicitPermission}})
	if pending == 0 && turnID != "" {
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventStatus, TurnID: turnID, Status: protocol.ChatStatusWorking})
	}
	return conn.rpc.Reply(pr.rpcID, map[string]any{"outcome": outcome})
}

// Control cancels the turn or changes model / effort / approval mode. Changes that the
// agent can apply live (set_model, set_config_option, set_mode) take effect at once and
// re-emit EventSessionReady with the new values; launch-flag changes restart the process
// on the next prompt and resume the agent session.
func (s *acpSession) Control(ctx context.Context, c protocol.AgentChatControl) error {
	if c.Cancel {
		s.cancelTurn()
	}
	if c.Model == "" && c.Effort == "" && c.ApprovalMode == "" {
		return nil
	}
	if c.ApprovalMode != "" {
		if _, ok := s.ad.approval(c.ApprovalMode); !ok {
			return fmt.Errorf("%s has no approval mode %q", s.ad.Label, c.ApprovalMode)
		}
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("session closed")
	}
	prevModel, prevEffort, prevApproval := s.model, s.effort, s.approval
	if c.ApprovalMode != "" {
		s.approval = c.ApprovalMode
	}
	if c.Model != "" {
		if c.Effort == "" && c.Model != prevModel {
			s.effort = "" // the new model's default
		}
		s.model = c.Model
	}
	if c.Effort != "" {
		s.effort = normalizeEffort(c.Effort)
	}
	want := acpLaunch{Approval: s.launchApprovalLocked()}
	if s.ad.LaunchModel {
		want.Model, want.Effort = s.model, s.effort
	}
	if want != s.launch || s.dead || s.conn == nil {
		s.needRestart = true
		s.restartWhy = "Restarted " + s.ad.Label + " with the new settings and resumed the session."
		s.mu.Unlock()
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventStatus, Status: s.currentStatus(),
			Text: s.ad.Label + " restarts with the new settings on your next message."})
		return nil
	}
	s.mu.Unlock()
	var err error
	if c.Model != "" || c.Effort != "" {
		err = s.applyModel(ctx)
	}
	if err == nil && c.ApprovalMode != "" {
		err = s.applyApproval(ctx)
	}
	if err != nil {
		s.mu.Lock()
		s.model, s.effort, s.approval = prevModel, prevEffort, prevApproval
		s.mu.Unlock()
		return fmt.Errorf("%s: %w", s.ad.Label, err)
	}
	s.emitReady()
	return nil
}

func (s *acpSession) currentStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn != nil {
		return protocol.ChatStatusWorking
	}
	return protocol.ChatStatusIdle
}

// cancelTurn sends session/cancel and cancels pending permission requests (as the ACP
// spec requires); the agent then answers session/prompt with stopReason "cancelled".
func (s *acpSession) cancelTurn() {
	s.mu.Lock()
	t := s.turn
	if t == nil || s.conn == nil {
		s.mu.Unlock()
		return
	}
	t.cancelled = true
	conn, sid := s.conn, s.sessionID
	var resolved []string
	for id, pr := range s.perms {
		_ = conn.rpc.Reply(pr.rpcID, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
		delete(s.perms, id)
		resolved = append(resolved, id)
	}
	s.mu.Unlock()
	_ = conn.rpc.Notify("session/cancel", map[string]any{"sessionId": sid})
	for _, id := range resolved {
		s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventElicitationOK, TurnID: t.id, Text: "cancelled",
			Elicitation: &protocol.AgentElicitation{ID: id, Kind: protocol.ElicitPermission}})
	}
}

// restart replaces the process and resumes the agent session. Requires opMu.
func (s *acpSession) restart(ctx context.Context) *acpOpenError {
	s.mu.Lock()
	old, sid := s.conn, s.sessionID
	s.conn = nil
	s.needRestart, s.restartWhy = false, ""
	s.mu.Unlock()
	if old != nil {
		old.kill()
	}
	return s.connect(ctx, sid)
}

// watchdog reports a turn with no agent activity for acpWatchdog (once per turn).
func (s *acpSession) watchdog() {
	tick := time.NewTicker(s.wdTick)
	defer tick.Stop()
	for {
		select {
		case <-s.stopWatch:
			return
		case <-tick.C:
		}
		s.mu.Lock()
		t := s.turn
		fire := t != nil && !t.timedOut && !s.suspended && len(s.perms) == 0 && time.Since(t.lastEvent) > s.wdAfter
		if fire {
			t.timedOut = true
		}
		s.mu.Unlock()
		if fire {
			s.emitEv(protocol.AgentChatEvent{Kind: protocol.EventError, TurnID: t.id, Error: &protocol.AgentChatError{
				Kind: protocol.ErrKindTimeout, Retryable: true,
				Message: fmt.Sprintf("%s has shown no progress for %s. Cancel the turn or keep waiting.", s.ad.Label, s.wdAfter.Round(time.Second)),
			}})
		}
	}
}

// Close kills the process group and waits for it. The agent session stays resumable.
func (s *acpSession) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conn := s.conn
	close(s.stopWatch)
	s.mu.Unlock()
	if conn != nil {
		conn.kill()
		<-conn.rpc.Done()
	}
	return nil
}

func (s *acpSession) pidLocked() int {
	if s.conn == nil || s.conn.proc == nil || s.conn.proc.Cmd == nil || s.conn.proc.Cmd.Process == nil {
		return 0
	}
	return s.conn.proc.Cmd.Process.Pid
}

// Suspend freezes the agent's process tree (Suspender).
func (s *acpSession) Suspend() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.suspended {
		return nil
	}
	if pid := s.pidLocked(); pid > 0 {
		if err := proctree.Suspend(pid); err != nil {
			return err
		}
	}
	s.suspended = true
	return nil
}

// Resume thaws the agent's process tree; the watchdog restarts from now (Suspender).
func (s *acpSession) Resume() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.suspended {
		return nil
	}
	if pid := s.pidLocked(); pid > 0 {
		if err := proctree.Resume(pid); err != nil && !errors.Is(err, proctree.ErrNoProcess) {
			return err
		}
	}
	s.suspended = false
	if s.turn != nil {
		s.turn.lastEvent = time.Now()
	}
	return nil
}
