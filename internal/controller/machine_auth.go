package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/konsert/internal/auth"
	"github.com/BryanSant/konsert/internal/ids"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
	"github.com/google/uuid"
)

// Per-machine credentials (KON-RK9YD) and their rotation (KON-W1YRP).
//
// Every konsertd authenticates /ws/worker with its own credential, bound to its machine:
// the hub maps the credential to a worker id and refuses a worker.hello for any other
// identity, so one machine cannot impersonate another. Credentials reach machines
// without anyone copying them:
//
//   - a manual machine trades a short-lived, single-use join code for one
//     (`konsertd join <hub> <code>`, POST /api/machines/join);
//   - the hub mints one when it installs konsertd itself (its own machine, or over SSH)
//     and hands it to `konsertd install --credential-stdin`. (SSH machines move to an
//     SSH transport later, which bypasses authenticateMachine entirely.)
//
// Revoking a machine's credential, or rotating (which revokes manual machines'
// credentials so they re-join with a new code), disconnects it immediately.
// The credential check lives in one place, authenticateMachine.
//
// Only SHA-256 hashes of credential secrets and join codes are stored.

// Audit actions for machine credentials.
const (
	AuditMachineJoinCode   = "machine.join_code"
	AuditMachineJoin       = "machine.join"
	AuditMachineJoinFailed = "machine.join_failed"
	AuditMachineCredMint   = "machine.credential_mint"
	AuditMachineCredRevoke = "machine.credential_revoke"
	AuditMachineCredRotate = "machine.credential_rotate"
)

const (
	// joinCodeTTL is how long a join code can be redeemed.
	joinCodeTTL = 15 * time.Minute
	// maxActiveJoinCodes caps outstanding (unused, unexpired) join codes.
	maxActiveJoinCodes = 20
)

// machineAuth holds the hub's machine-credential state; tests replace the clock.
type machineAuth struct {
	now func() time.Time
	// joinIP throttles failed joins per client IP; joinAll across all clients, so a
	// distributed guess at the 40-bit codes is throttled too.
	joinIP  *auth.LoginLimiter
	joinAll *auth.LoginLimiter
}

func newMachineAuth() *machineAuth {
	m := &machineAuth{now: time.Now}
	clock := func() time.Time { return m.now() }
	m.joinIP = auth.NewLoginLimiter()
	m.joinIP.Now = clock
	m.joinAll = auth.NewLoginLimiter()
	m.joinAll.FreeAttempts = 50
	m.joinAll.MaxDelay = time.Minute
	m.joinAll.Now = clock
	return m
}

func (m *machineAuth) clock() time.Time { return m.now().UTC() }

// boundMachine is the machine a /ws/worker connection's credential belongs to.
type boundMachine struct {
	workerID string
	credID   string
	credSID  string
}

type boundMachineKey struct{}

func withBoundMachine(ctx context.Context, b boundMachine) context.Context {
	return context.WithValue(ctx, boundMachineKey{}, b)
}

func boundMachineFrom(ctx context.Context) (boundMachine, bool) {
	b, ok := ctx.Value(boundMachineKey{}).(boundMachine)
	return b, ok
}

// authenticateMachine checks a /ws/worker upgrade's credential. With --no-auth every
// connection is accepted unbound; otherwise the bearer must be an active credential,
// and the connection is bound to its machine.
func (s *Server) authenticateMachine(r *http.Request) (boundMachine, bool) {
	if s.auth.NoAuth() {
		return boundMachine{}, true
	}
	credID, secret, ok := auth.ParseMachineCredential(auth.BearerToken(r))
	if !ok {
		return boundMachine{}, false
	}
	cred, hash, err := s.store.GetMachineCredential(r.Context(), credID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.logger.Warn("machine credential lookup failed", "err", err)
		}
		return boundMachine{}, false
	}
	if !auth.VerifyMachineSecret(secret, hash) || cred.RevokedAt != nil {
		return boundMachine{}, false
	}
	if err := s.store.TouchMachineCredential(r.Context(), cred.ID, s.machines.clock()); err != nil {
		s.logger.Warn("record credential use failed", "credential", cred.SID, "err", err)
	}
	return boundMachine{workerID: cred.WorkerID, credID: cred.ID, credSID: cred.SID}, true
}

// mintMachineCredential creates a credential for workerID and returns the bearer string
// to hand to that machine (it is never stored or logged).
func (s *Server) mintMachineCredential(ctx context.Context, workerID, source string) (string, store.MachineCredential, error) {
	sec, err := auth.NewMachineSecret()
	if err != nil {
		return "", store.MachineCredential{}, err
	}
	cred, err := s.store.CreateMachineCredential(ctx, store.MachineCredential{
		WorkerID: workerID, Source: source, CreatedAt: s.machines.clock(),
	}, sec.Hash)
	if err != nil {
		return "", store.MachineCredential{}, err
	}
	s.hub.recordAudit(ctx, store.AuditEvent{
		Action: AuditMachineCredMint, WorkerID: workerID, WorkerSID: s.workerSID(ctx, workerID),
		Message:  "Issued machine credential " + cred.SID + " (" + source + ")",
		MetaJSON: auditMeta(map[string]any{"credential": cred.SID, "source": source}),
	})
	return auth.FormatMachineCredential(cred.ID, sec.Secret), cred, nil
}

func (s *Server) workerSID(ctx context.Context, workerID string) string {
	if w, err := s.store.GetWorker(ctx, workerID); err == nil {
		return w.SID
	}
	return ""
}

// revokeAndDisconnect revokes every credential of workerID and closes its connection
// with WSCloseCredentialRevoked, so it stays out until it re-joins.
func (s *Server) revokeAndDisconnect(ctx context.Context, workerID, reason string) (int, error) {
	n, err := s.store.RevokeMachineCredentials(ctx, workerID, "", reason, s.machines.clock())
	if err != nil {
		return 0, err
	}
	s.hub.disconnectWorkerWith(workerID, protocol.WSCloseCredentialRevoked, "machine credential revoked")
	return n, nil
}

// provisionCred mints at most one credential during one provisioning run (local
// install or SSH bootstrap) and settles the machine's credentials afterwards.
type provisionCred struct {
	s      *Server
	source string
	cred   store.MachineCredential
	minted bool
}

func (s *Server) newProvisionCred(source string) *provisionCred {
	return &provisionCred{s: s, source: source}
}

// mint returns a new credential for workerID (BootstrapOptions.Credential).
func (p *provisionCred) mint(ctx context.Context, workerID string) (string, error) {
	if p.minted {
		return "", errors.New("credential already issued for this run")
	}
	tok, cred, err := p.s.mintMachineCredential(ctx, workerID, p.source)
	if err != nil {
		return "", fmt.Errorf("issue machine credential: %w", err)
	}
	p.cred, p.minted = cred, true
	return tok, nil
}

// finish revokes the machine's older credentials once the new one is installed (ok),
// or the unused new one when provisioning failed.
func (p *provisionCred) finish(ctx context.Context, ok bool) {
	if !p.minted {
		return
	}
	ctx = context.WithoutCancel(ctx)
	now := p.s.machines.clock()
	if ok {
		if _, err := p.s.store.RevokeMachineCredentials(ctx, p.cred.WorkerID, p.cred.ID, "replaced by "+p.cred.SID, now); err != nil {
			p.s.logger.Warn("revoke replaced credentials failed", "worker_id", p.cred.WorkerID, "err", err)
		}
		return
	}
	if err := p.s.store.RevokeMachineCredential(ctx, p.cred.ID, "provisioning failed", now); err != nil {
		p.s.logger.Warn("revoke unused credential failed", "credential", p.cred.SID, "err", err)
	}
}

// CredentialRotation reports what a rotation did for one machine.
type CredentialRotation struct {
	WorkerID  string `json:"worker_id"`
	WorkerSID string `json:"worker_sid,omitempty"`
	Name      string `json:"name"`
	// Action: "revoked" (it needs a new join code) or "failed".
	Action    string `json:"action"`
	NeedsJoin bool   `json:"needs_join,omitzero"`
	Error     string `json:"error,omitempty"`
}

// Rotation actions.
const (
	rotationRevoked = "revoked"
	rotationFailed  = "failed"
)

// rotateMachine retires the credentials of a machine that joined by hand: they are
// revoked, it is disconnected, and it needs a new join code. Machines the hub installs
// itself (launch local/ssh) are skipped, as are machines with no credential that are
// not connected (ok false).
func (s *Server) rotateMachine(ctx context.Context, w store.Worker) (CredentialRotation, bool) {
	if w.Launch == store.LaunchLocal || w.Launch == store.LaunchSSH {
		return CredentialRotation{}, false
	}
	res := CredentialRotation{WorkerID: w.ID, WorkerSID: w.SID, Name: w.Name, Action: rotationRevoked, NeedsJoin: true}
	creds, err := s.store.ListMachineCredentials(ctx, w.ID)
	if err != nil {
		res.Action, res.NeedsJoin, res.Error = rotationFailed, false, err.Error()
		return res, true
	}
	active := slices.ContainsFunc(creds, func(c store.MachineCredential) bool { return c.RevokedAt == nil })
	if !active && !s.hub.IsWorkerConnected(w.ID) {
		return CredentialRotation{}, false
	}
	if _, err := s.revokeAndDisconnect(ctx, w.ID, "rotated"); err != nil {
		res.Action, res.NeedsJoin, res.Error = rotationFailed, false, err.Error()
	}
	return res, true
}

// RotationResponse is POST /api/machines/credentials/rotate.
type RotationResponse struct {
	Results   []CredentialRotation `json:"results"`
	NeedsJoin int                  `json:"needs_join"`
	Failed    int                  `json:"failed"`
}

// rotateMachines rotates the credentials of every manually joined machine (or only of
// workerID).
func (s *Server) rotateMachines(ctx context.Context, workerID string) (RotationResponse, error) {
	var workers []store.Worker
	if workerID != "" {
		w, err := s.store.GetWorker(ctx, workerID)
		if err != nil {
			return RotationResponse{}, err
		}
		workers = []store.Worker{w}
	} else {
		all, err := s.store.ListWorkers(ctx)
		if err != nil {
			return RotationResponse{}, err
		}
		workers = all
	}
	out := RotationResponse{Results: []CredentialRotation{}}
	for _, w := range workers {
		res, ok := s.rotateMachine(ctx, w)
		if !ok {
			continue
		}
		s.hub.recordAudit(ctx, store.AuditEvent{
			Action: AuditMachineCredRotate, WorkerID: w.ID, WorkerSID: w.SID,
			Message:  rotationMessage(res),
			MetaJSON: auditMeta(map[string]any{"action": res.Action, "error": res.Error}),
		})
		out.Results = append(out.Results, res)
		if res.Action == rotationFailed {
			out.Failed++
		} else {
			out.NeedsJoin++
		}
	}
	slices.SortFunc(out.Results, func(a, b CredentialRotation) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out, nil
}

func rotationMessage(r CredentialRotation) string {
	name := cmp.Or(r.Name, r.WorkerID)
	switch r.Action {
	case rotationRevoked:
		return "Revoked the credential of " + name + "; it needs a new join code"
	default:
		return "Rotating the credential of " + name + " failed: " + r.Error
	}
}

// --- HTTP handlers ---------------------------------------------------------------

// JoinCodeResponse is POST /api/machines/join-codes.
type JoinCodeResponse struct {
	ID        string    `json:"id"`
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	HubURL    string    `json:"hub_url"`
	Command   string    `json:"command"`
}

func (s *Server) handleCreateJoinCode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.machines.clock()
	if n, err := s.store.CountActiveMachineJoinCodes(ctx, now); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	} else if n >= maxActiveJoinCodes {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many unused join codes; wait for them to expire"})
		return
	}
	code, err := auth.NewJoinCode()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	norm, _ := auth.NormalizeJoinCode(code)
	jc, err := s.store.CreateMachineJoinCode(ctx, store.MachineJoinCode{
		CreatedBy: auditActorFrom(ctx).User, CreatedAt: now, ExpiresAt: now.Add(joinCodeTTL),
	}, auth.JoinCodeHash(norm))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	hubURL := s.joinHubURL(r)
	s.hub.recordAudit(ctx, store.AuditEvent{
		Action:   AuditMachineJoinCode,
		Message:  "Created a machine join code (expires in 15 min)",
		MetaJSON: auditMeta(map[string]any{"join_code_id": jc.ID, "expires_at": jc.ExpiresAt}),
	})
	writeJSON(w, http.StatusCreated, JoinCodeResponse{
		ID: jc.ID, Code: code, ExpiresAt: jc.ExpiresAt, HubURL: hubURL,
		Command: "konsertd join " + hubURL + " " + code,
	})
}

// JoinCodeStatus is GET /api/machines/join-codes/{id}.
type JoinCodeStatus struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"` // pending | used | expired
	ExpiresAt  time.Time  `json:"expires_at"`
	UsedAt     *time.Time `json:"used_at,omitzero"`
	WorkerID   string     `json:"worker_id,omitempty"`
	WorkerSID  string     `json:"worker_sid,omitempty"`
	WorkerName string     `json:"worker_name,omitempty"`
}

func (s *Server) handleGetJoinCode(w http.ResponseWriter, r *http.Request) {
	jc, err := s.store.GetMachineJoinCode(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "join code not found"})
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	st := JoinCodeStatus{ID: jc.ID, ExpiresAt: jc.ExpiresAt, UsedAt: jc.UsedAt, WorkerID: jc.WorkerID}
	switch {
	case jc.UsedAt != nil:
		st.Status = "used"
		if wk, err := s.store.GetWorker(r.Context(), jc.WorkerID); err == nil {
			st.WorkerSID, st.WorkerName = wk.SID, wk.Name
		}
	case !s.machines.clock().Before(jc.ExpiresAt):
		st.Status = "expired"
	default:
		st.Status = "pending"
	}
	writeJSON(w, http.StatusOK, st)
}

// JoinRequest is POST /api/machines/join, sent by `konsertd join`.
type JoinRequest struct {
	Code      string `json:"code"`
	WorkerID  string `json:"worker_id"`
	Name      string `json:"name"`
	HostGroup string `json:"host_group"`
	Hostname  string `json:"hostname"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// JoinResponse carries the machine's new credential (the only time it is sent).
type JoinResponse struct {
	Credential string `json:"credential"`
	WorkerID   string `json:"worker_id"`
	WorkerSID  string `json:"worker_sid"`
	Name       string `json:"name"`
}

const errBadJoinCode = "invalid or expired join code"

// handleMachineJoin trades a join code for a credential. It is unauthenticated (the
// code is the credential), so failures are rate-limited per client and fleet-wide.
func (s *Server) handleMachineJoin(w http.ResponseWriter, r *http.Request) {
	ctx := withAuditActor(r.Context(), "konsertd join", auditSourceREST)
	ip := auth.ClientIP(r)
	for _, l := range []struct {
		lim *auth.LoginLimiter
		key string
	}{{s.machines.joinIP, ip}, {s.machines.joinAll, "*"}} {
		if ok, wait := l.lim.Allow(l.key); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed join attempts; try again later"})
			return
		}
	}
	var req JoinRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	fail := func(status int, msg, why string) {
		s.machines.joinIP.Failure(ip)
		s.machines.joinAll.Failure("*")
		s.logger.Warn("machine join refused", "ip", ip, "why", why)
		s.hub.recordAudit(ctx, store.AuditEvent{
			Action: AuditMachineJoinFailed, WorkerID: req.WorkerID,
			Message:  "Refused a machine join: " + why,
			MetaJSON: auditMeta(map[string]any{"ip": ip, "name": req.Name}),
		})
		writeJSON(w, status, map[string]string{"error": msg})
	}
	norm, ok := auth.NormalizeJoinCode(req.Code)
	if !ok {
		fail(http.StatusUnauthorized, errBadJoinCode, "malformed code")
		return
	}
	if u, err := uuid.Parse(req.WorkerID); err != nil || u.String() != req.WorkerID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker_id must be konsertd's UUID identity"})
		return
	}
	name := cleanLabel(req.Name, cleanLabel(req.Hostname, "machine"))
	hostGroup := cleanLabel(req.HostGroup, name)

	// A machine that holds an active credential keeps it: a join code cannot take over
	// an existing identity (revoke it first).
	creds, err := s.store.ListMachineCredentials(ctx, req.WorkerID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	for _, c := range creds {
		if c.RevokedAt == nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "this machine already has a credential; revoke it in Fleet first"})
			return
		}
	}
	now := s.machines.clock()
	jc, err := s.store.RedeemMachineJoinCode(ctx, auth.JoinCodeHash(norm), req.WorkerID, now)
	if errors.Is(err, store.ErrNotFound) {
		fail(http.StatusUnauthorized, errBadJoinCode, "unknown, used or expired code")
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.machines.joinIP.Success(ip)

	row, err := s.store.GetWorker(ctx, req.WorkerID)
	if errors.Is(err, store.ErrNotFound) {
		row, err = s.store.EnsureWorker(ctx, store.Worker{
			ID: req.WorkerID, Name: name, HostGroup: hostGroup,
			OS: cleanLabel(req.OS, ""), Arch: cleanLabel(req.Arch, ""), Launch: store.LaunchManual,
		})
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	tok, cred, err := s.mintMachineCredential(ctx, row.ID, store.MachineCredJoin)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.markSetupCompleted(ctx)
	s.hub.recordAudit(ctx, store.AuditEvent{
		Action: AuditMachineJoin, WorkerID: row.ID, WorkerSID: row.SID,
		Message:  "Machine " + row.Name + " joined with a code",
		MetaJSON: auditMeta(map[string]any{"ip": ip, "credential": cred.SID, "join_code_id": jc.ID}),
	})
	s.logger.Info("machine joined", "worker", row.SID, "name", row.Name)
	s.hub.broadcastWorkerView(ctx, row.ID)
	writeJSON(w, http.StatusOK, JoinResponse{Credential: tok, WorkerID: row.ID, WorkerSID: row.SID, Name: row.Name})
}

// cleanLabel trims a self-reported label to 64 printable characters, or def if empty.
func cleanLabel(v, def string) string {
	v = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(v))
	if len(v) > 64 {
		v = v[:64]
	}
	if v == "" {
		return def
	}
	return v
}

// joinHubURL is the hub URL a manual machine should join: the configured public URL,
// else the URL the browser used unless that is loopback, else this host's name and
// port.
func (s *Server) joinHubURL(r *http.Request) string {
	s.cfgMu.RLock()
	public := strings.TrimSpace(s.cfg.PublicURL)
	s.cfgMu.RUnlock()
	if public != "" {
		if u, err := url.Parse(public); err == nil && u.Host != "" {
			return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")
		}
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, ""
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); host == "" || strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback()) {
		if h, _ := os.Hostname(); h != "" {
			host = h
		}
		if _, p, err := net.SplitHostPort(s.listenAddr()); err == nil && p != "" && p != "0" {
			port = p
		}
	}
	if port != "" {
		return scheme + "://" + net.JoinHostPort(strings.Trim(host, "[]"), port)
	}
	return scheme + "://" + host
}

// MachineCredentialInfo is GET /api/workers/{id}/credential.
type MachineCredentialInfo struct {
	WorkerID string                    `json:"worker_id"`
	Launch   string                    `json:"launch,omitempty"`
	Active   []store.MachineCredential `json:"active"`
	// Revoked counts the machine's revoked credentials.
	Revoked int `json:"revoked"`
}

func (s *Server) handleGetMachineCredential(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	row, ok := s.workerOr404(w, r)
	if !ok {
		return
	}
	creds, err := s.store.ListMachineCredentials(ctx, row.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	info := MachineCredentialInfo{WorkerID: row.ID, Launch: row.Launch, Active: []store.MachineCredential{}}
	for _, c := range creds {
		if c.RevokedAt == nil {
			info.Active = append(info.Active, c)
		} else {
			info.Revoked++
		}
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) workerOr404(w http.ResponseWriter, r *http.Request) (store.Worker, bool) {
	row, err := s.store.GetWorker(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "worker not found"})
		return row, false
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return row, false
	}
	return row, true
}

func (s *Server) handleRevokeMachineCredential(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	row, ok := s.workerOr404(w, r)
	if !ok {
		return
	}
	n, err := s.revokeAndDisconnect(ctx, row.ID, "revoked by "+auditActorFrom(ctx).User)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.hub.recordAudit(ctx, store.AuditEvent{
		Action: AuditMachineCredRevoke, WorkerID: row.ID, WorkerSID: row.SID,
		Message:  "Revoked the credential of " + row.Name + " and disconnected it",
		MetaJSON: auditMeta(map[string]any{"revoked": n}),
	})
	s.hub.broadcastWorkerView(ctx, row.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": n})
}

func (s *Server) handleRotateMachineCredentials(w http.ResponseWriter, r *http.Request) {
	var body struct {
		// Machine limits the rotation to one machine (its id or m- sid).
		Machine string `json:"machine"`
	}
	if !decodeJSONBody(w, r, &body, true) {
		return
	}
	workerID := ""
	if ref := strings.TrimSpace(body.Machine); ref != "" {
		id, err := s.store.ResolveID(r.Context(), ids.Machine, ref)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "machine " + ref + " not found"})
			return
		}
		workerID = id
	}
	res, err := s.rotateMachines(r.Context(), workerID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// RotateMachineCredentials asks the hub to rotate the credentials of every manually
// joined machine, or only machine's (an id or m- sid) when it is not empty.
func (c *RemoteFleetClient) RotateMachineCredentials(ctx context.Context, machine string) (RotationResponse, error) {
	var res RotationResponse
	err := c.doJSON(ctx, http.MethodPost, "/api/machines/credentials/rotate", map[string]string{"machine": machine}, &res)
	return res, err
}
