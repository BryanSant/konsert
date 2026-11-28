package controller

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BryanSant/syre/internal/auth"
	"github.com/BryanSant/syre/internal/qrcode"
	"github.com/BryanSant/syre/internal/store"
)

// Device sign-in (SYR-W0P8B, SYR-V5JZB, SYR-A29CT): pairing links, the CLI's device-code
// login, Settings → Devices, MCP credentials and recovery-key rotation.

// Audit actions for device sign-in.
const (
	AuditDevicePairCode     = "device.pair_code"
	AuditDevicePaired       = "device.paired"
	AuditDeviceLoginApprove = "device.login_approve"
	AuditDeviceLoginDeny    = "device.login_deny"
	AuditDeviceLogin        = "device.login"
	AuditDeviceRename       = "device.rename"
	AuditDeviceRevoke       = "device.revoke"
	AuditDeviceMCP          = "device.mcp_create"
	AuditRecoveryLogin      = "auth.recovery_login"
	AuditRecoveryRotate     = "auth.recovery_rotate"
)

// deviceIdleSetting is the sliding expiry of device credentials, in days.
const deviceIdleSetting = "device_idle_days"

const (
	defaultDeviceIdleDays = 90
	maxDeviceIdleDays     = 3650
	maxDeviceNameLen      = 60
	// loginPollInterval is how often `syre login` should poll, in seconds.
	loginPollInterval = 2
)

type identityKey struct{}

func withIdentity(ctx context.Context, id auth.Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// identityFrom returns the authenticated caller attached by requireBrowserAuth.
func identityFrom(ctx context.Context) auth.Identity {
	id, _ := ctx.Value(identityKey{}).(auth.Identity)
	return id
}

// deviceIdleDays parses the saved setting (default 90).
func deviceIdleDays(saved map[string]string) int {
	if n, err := strconv.Atoi(strings.TrimSpace(saved[deviceIdleSetting])); err == nil && n >= 1 && n <= maxDeviceIdleDays {
		return n
	}
	return defaultDeviceIdleDays
}

func idleDuration(days int) time.Duration { return time.Duration(days) * 24 * time.Hour }

// cleanDeviceName trims a requested name to something printable and short.
func cleanDeviceName(name, fallback string) string {
	name = strings.Join(strings.Fields(name), " ")
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	for utf8.RuneCountInString(name) > maxDeviceNameLen {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "" {
		name = fallback
	}
	if name == "" {
		name = "Device"
	}
	return name
}

// deviceView is a device as Settings → Devices shows it.
type deviceView struct {
	store.Device `json:",inline"`
	Current      bool `json:"current"`
	Expired      bool `json:"expired"`
}

func (s *Server) viewDevice(d store.Device, current string) deviceView {
	d.ExpiresAt = s.auth.ExpiresAt(d)
	return deviceView{Device: d, Current: d.ID == current, Expired: s.auth.Expired(d, s.auth.Now())}
}

// publicBaseURL is the base URL to put in links shown to other devices: the public URL
// when configured, else the URL this request used, unless that is loopback (useless on
// a phone), in which case the best LAN URL of the listener.
func (s *Server) publicBaseURL(r *http.Request) string {
	s.cfgMu.RLock()
	public, bound := s.cfg.PublicURL, s.boundAddr
	s.cfgMu.RUnlock()
	if strings.TrimSpace(public) != "" {
		return strings.TrimRight(strings.TrimSpace(public), "/")
	}
	if r != nil && r.Host != "" {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			scheme := "http"
			if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
				scheme = "https"
			}
			return scheme + "://" + r.Host
		}
	}
	base, _ := BestBaseURL(bound, "")
	return base
}

func pairURL(base, code string) string  { return base + "/pair/" + code }
func loginURL(base, code string) string { return base + "/approve/" + code }

// PairingCode is a newly minted sign-in code. Code is shown once and never stored.
type PairingCode struct {
	Code      string     `json:"code"`
	SID       string     `json:"sid"`
	URL       string     `json:"url"`
	ExpiresAt time.Time  `json:"expires_at"`
	QR        qrcode.SVG `json:"qr"`
}

// MintPairingCode creates a single-use pairing code for a browser or phone and audits
// it by its short id (never the code). base is the hub URL for the link.
func (s *Server) MintPairingCode(ctx context.Context, base, createdBy, ip string) (PairingCode, error) {
	return s.mintCode(ctx, base, createdBy, ip, store.PairingPurposePair, auth.PairingCodeTTL)
}

func (s *Server) mintCode(ctx context.Context, base, createdBy, ip, purpose string, ttl time.Duration) (PairingCode, error) {
	code, hash, err := auth.NewPairingCode()
	if err != nil {
		return PairingCode{}, err
	}
	now := s.auth.Now()
	_ = s.store.PruneAuth(ctx, now.Add(-24*time.Hour))
	rec, err := s.store.CreatePairingCode(ctx, store.PairingCode{
		Purpose: purpose, CodeHash: hash, CreatedBy: createdBy, CreatedIP: ip,
		CreatedAt: now, ExpiresAt: now.Add(ttl),
	})
	if err != nil {
		return PairingCode{}, err
	}
	link := pairURL(strings.TrimRight(base, "/"), code)
	qr, _ := qrcode.EncodeSVG(link)
	msg := "Created pairing code " + rec.SID
	if purpose == store.PairingPurposeLaunch {
		msg = "Created local launcher sign-in code " + rec.SID + " (syre open)"
	}
	s.hub.recordAudit(withDefaultAuditActor(ctx, cmpOr(createdBy, "system"), auditSourceREST), store.AuditEvent{
		Action:   AuditDevicePairCode,
		Message:  msg,
		MetaJSON: auditMeta(map[string]any{"code_sid": rec.SID, "expires_at": rec.ExpiresAt}),
	})
	return PairingCode{Code: code, SID: rec.SID, URL: link, ExpiresAt: rec.ExpiresAt, QR: qr}, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// handleCreatePairingCode serves POST /api/pairing-codes: "Add a device" and `syre pair`.
func (s *Server) handleCreatePairingCode(w http.ResponseWriter, r *http.Request) {
	if s.auth.NoAuth() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "authentication is off (--no-auth): there is nothing to sign in to"})
		return
	}
	var body struct {
		// Launcher asks for a `syre open` code: 60 s, redeemable only over loopback, and
		// only issued to a request from this machine itself.
		Launcher bool `json:"launcher"`
	}
	if requestHasBody(r) && !decodeJSON(w, r, &body) {
		return
	}
	id := identityFrom(r.Context())
	purpose, ttl, base := store.PairingPurposePair, auth.PairingCodeTTL, s.publicBaseURL(r)
	if body.Launcher {
		if !auth.IsLocalRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "a launcher sign-in code is only issued to the hub's own machine over loopback"})
			return
		}
		purpose, ttl, base = store.PairingPurposeLaunch, auth.LaunchCodeTTL, "http://"+r.Host
	}
	pc, err := s.mintCode(r.Context(), base, id.User, auth.ClientIP(r), purpose, ttl)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, pc)
}

// issueDevice creates a credential for a new device through redeeming a code.
func (s *Server) redeemCode(ctx context.Context, rec store.PairingCode, name, kind, ip, ua string) (store.Device, string, bool, error) {
	secret, hash, err := auth.NewDeviceSecret()
	if err != nil {
		return store.Device{}, "", false, err
	}
	now := s.auth.Now()
	d, ok, err := s.store.RedeemPairingCode(ctx, rec.ID, store.Device{
		Name: name, Kind: kind, SecretHash: hash, CreatedAt: now, CreatedBy: "pairing code " + rec.SID,
		LastUsedAt: now, LastIP: ip, LastUserAgent: ua, ExpiresAt: now.Add(s.auth.IdleExpiry()),
	}, now)
	return d, secret, ok, err
}

// pairFailure records a failed redemption against the peer and the global budget.
func (s *Server) pairFailure(ip string) {
	s.pairLimiter.Failure(ip)
	s.pairGlobal.Failure("*")
}

func (s *Server) pairAllowed(w http.ResponseWriter, ip string) bool {
	ok, wait := s.pairLimiter.Allow(ip)
	if ok {
		ok, wait = s.pairGlobal.Allow("*")
	}
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many attempts; try again later"})
	}
	return ok
}

// handlePair serves POST /api/auth/pair {code, name}: the /pair/{code} page redeems a
// pairing code, and this browser becomes a signed-in device.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	ip := auth.ClientIP(r)
	if !s.pairAllowed(w, ip) {
		return
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	invalid := func() {
		s.pairFailure(ip)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "this sign-in code is invalid, expired or already used"})
	}
	canon, err := auth.NormalizePairingCode(body.Code)
	if err != nil {
		invalid()
		return
	}
	rec, err := s.store.GetPairingCodeByHash(r.Context(), auth.HashPairingCode(canon))
	if err != nil || (rec.Purpose != store.PairingPurposePair && rec.Purpose != store.PairingPurposeLaunch) {
		invalid()
		return
	}
	launch := rec.Purpose == store.PairingPurposeLaunch
	if launch && !auth.IsLocalRequest(r) {
		invalid() // a launcher code only signs in a browser on the hub's own machine
		return
	}
	uaRaw := r.UserAgent()
	ua := auth.UserAgentSummary(uaRaw)
	d, secret, ok, err := s.redeemCode(r.Context(), rec, cleanDeviceName(body.Name, ua), auth.DeviceKindForUA(uaRaw), ip, ua)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		invalid()
		return
	}
	s.pairLimiter.Success(ip)
	auth.SetSessionCookie(w, r, secret)
	ctx := withAuditActor(r.Context(), auth.DeviceUser(d), auditSourceREST)
	s.hub.recordAudit(ctx, store.AuditEvent{
		Action:   AuditDevicePaired,
		Message:  "Signed in " + d.Name + " (" + d.SID + ") with pairing code " + rec.SID + launchNote(launch),
		MetaJSON: auditMeta(map[string]any{"device_sid": d.SID, "code_sid": rec.SID, "kind": d.Kind, "ip": ip}),
	})
	view := s.viewDevice(d, d.ID)
	s.hub.broadcastFleetUpdate("device", s.viewDevice(d, ""))
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "user": auth.DeviceUser(d), "device": view})
}

// handleLogin serves POST /api/auth/login {token, name}: the login page's "Use recovery
// key" fallback. A correct recovery key signs this browser in as a new device.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
		Name  string `json:"name"`
	}
	ip := auth.ClientIP(r)
	if ok, wait := s.loginLimiter.Allow(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed attempts; try again later"})
		return
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if s.auth.NoAuth() {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "user": "local"})
		return
	}
	if !s.auth.CheckRecoveryKey(strings.TrimSpace(body.Token)) {
		s.loginLimiter.Failure(ip)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid recovery key"})
		return
	}
	s.loginLimiter.Success(ip)
	uaRaw := r.UserAgent()
	ua := auth.UserAgentSummary(uaRaw)
	secret, hash, err := auth.NewDeviceSecret()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	now := s.auth.Now()
	d, err := s.store.CreateDevice(r.Context(), store.Device{
		Name: cleanDeviceName(body.Name, ua), Kind: auth.DeviceKindForUA(uaRaw), SecretHash: hash,
		CreatedAt: now, CreatedBy: "recovery key", LastUsedAt: now, LastIP: ip, LastUserAgent: ua,
		ExpiresAt: now.Add(s.auth.IdleExpiry()),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	auth.SetSessionCookie(w, r, secret)
	s.hub.recordAudit(withAuditActor(r.Context(), auth.DeviceUser(d), auditSourceREST), store.AuditEvent{
		Action:   AuditRecoveryLogin,
		Message:  "Signed in " + d.Name + " (" + d.SID + ") with the recovery key",
		MetaJSON: auditMeta(map[string]any{"device_sid": d.SID, "ip": ip}),
	})
	s.hub.broadcastFleetUpdate("device", s.viewDevice(d, ""))
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "user": auth.DeviceUser(d), "device": s.viewDevice(d, d.ID)})
}

// handleLogout serves POST /api/auth/logout: signing out revokes this device.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if id, ok := s.auth.Authenticate(r); ok && id.Device != nil {
		s.revokeDevice(withAuditActor(r.Context(), id.User, restAuditSource(r)), *id.Device, "Signed out")
	}
	auth.ClearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// revokeDevice revokes a device, drops it from the auth cache and closes its sockets.
func (s *Server) revokeDevice(ctx context.Context, d store.Device, verb string) (bool, error) {
	ok, err := s.store.RevokeDevice(ctx, d.ID, s.auth.Now())
	s.auth.Invalidate(d.ID)
	if err != nil || !ok {
		return ok, err
	}
	closed := s.hub.DisconnectDevice(d.ID)
	s.hub.recordAudit(ctx, store.AuditEvent{
		Action:   AuditDeviceRevoke,
		Message:  verb + " " + d.Name + " (" + d.SID + ")",
		MetaJSON: auditMeta(map[string]any{"device_sid": d.SID, "kind": d.Kind, "sockets_closed": closed}),
	})
	s.hub.broadcastFleetUpdate("device_revoked", d.ID)
	return true, nil
}

// handleListDevices serves GET /api/devices.
func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListDevices(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	cur := identityFrom(r.Context()).DeviceID()
	out := make([]deviceView, 0, len(list))
	for _, d := range list {
		out = append(out, s.viewDevice(d, cur))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"devices":           out,
		"current_device_id": cur,
		"idle_days":         int(s.auth.IdleExpiry() / (24 * time.Hour)),
		"auth_required":     s.auth.AuthRequired(),
	})
}

func (s *Server) deviceFromPath(w http.ResponseWriter, r *http.Request) (store.Device, bool) {
	d, err := s.store.GetDevice(r.Context(), r.PathValue("id"))
	if err != nil || d.RevokedAt != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return store.Device{}, false
	}
	return d, true
}

// handleRenameDevice serves PATCH /api/devices/{id} {name}.
func (s *Server) handleRenameDevice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	d, ok := s.deviceFromPath(w, r)
	if !ok {
		return
	}
	name := cleanDeviceName(body.Name, "")
	if strings.TrimSpace(body.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if err := s.store.RenameDevice(r.Context(), d.ID, name); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}
	s.auth.Invalidate(d.ID)
	old := d.Name
	d.Name = name
	s.hub.recordAudit(r.Context(), store.AuditEvent{
		Action:   AuditDeviceRename,
		Message:  "Renamed device " + d.SID + " from " + old + " to " + name,
		MetaJSON: auditMeta(map[string]any{"device_sid": d.SID}),
	})
	s.hub.broadcastFleetUpdate("device", s.viewDevice(d, ""))
	writeJSON(w, http.StatusOK, s.viewDevice(d, identityFrom(r.Context()).DeviceID()))
}

// handleRevokeDevice serves DELETE /api/devices/{id}. Revoking takes effect at once:
// the credential stops working and the device's live connections close.
func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	d, ok := s.deviceFromPath(w, r)
	if !ok {
		return
	}
	if _, err := s.revokeDevice(r.Context(), d, "Revoked"); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if d.ID == identityFrom(r.Context()).DeviceID() {
		auth.ClearSessionCookie(w, r)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": d.ID})
}

// handleCreateMCPDevice serves POST /api/devices/mcp {name}: "Connect an MCP client"
// mints an MCP-only credential and returns it once, with ready-to-paste config.
func (s *Server) handleCreateMCPDevice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decodeJSONBody(w, r, &body, true) {
		return
	}
	if s.auth.NoAuth() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "authentication is off (--no-auth): MCP clients need no credential"})
		return
	}
	s.cfgMu.RLock()
	mcpOn := s.cfg.EnableMCPHTTP
	s.cfgMu.RUnlock()
	if !mcpOn {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "the /mcp endpoint is disabled (--enable-mcp-http=false)"})
		return
	}
	secret, hash, err := auth.NewDeviceSecret()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	now := s.auth.Now()
	id := identityFrom(r.Context())
	d, err := s.store.CreateDevice(r.Context(), store.Device{
		Name: cleanDeviceName(body.Name, "MCP client"), Kind: store.DeviceKindMCP, SecretHash: hash,
		CreatedAt: now, CreatedBy: id.User, LastUsedAt: now, ExpiresAt: now.Add(s.auth.IdleExpiry()),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.hub.recordAudit(r.Context(), store.AuditEvent{
		Action:   AuditDeviceMCP,
		Message:  "Created MCP credential " + d.Name + " (" + d.SID + ")",
		MetaJSON: auditMeta(map[string]any{"device_sid": d.SID}),
	})
	s.hub.broadcastFleetUpdate("device", s.viewDevice(d, ""))
	writeJSON(w, http.StatusCreated, map[string]any{
		"device":  s.viewDevice(d, id.DeviceID()),
		"token":   secret,
		"mcp_url": s.publicBaseURL(r) + "/mcp",
	})
}

// --- CLI device-code login (syre login) ---

// handleStartLogin serves POST /api/auth/device-code {name, kind}: `syre login`
// asks for a code to show the user, plus a secret it polls with.
func (s *Server) handleStartLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	ip := auth.ClientIP(r)
	if !s.loginCodeLimiter.Allow(ip) || !s.loginCodeGlobal.Allow("*") {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many sign-in requests; try again later"})
		return
	}
	if !decodeJSONBody(w, r, &body, true) {
		return
	}
	if s.auth.NoAuth() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "authentication is off (--no-auth): no sign-in needed"})
		return
	}
	kind := cmpOr(body.Kind, store.DeviceKindCLI)
	if kind != store.DeviceKindCLI && kind != store.DeviceKindMCP {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind must be cli or mcp"})
		return
	}
	code, hash, err := auth.NewPairingCode()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	poll, err := auth.GenerateToken(32)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	now := s.auth.Now()
	_ = s.store.PruneAuth(r.Context(), now.Add(-24*time.Hour))
	rec, err := s.store.CreatePairingCode(r.Context(), store.PairingCode{
		Purpose: store.PairingPurposeLogin, CodeHash: hash, PollHash: auth.HashSecret(poll), Kind: kind,
		DeviceName: cleanDeviceName(body.Name, auth.UserAgentSummary(r.UserAgent())), CreatedIP: ip,
		CreatedAt: now, ExpiresAt: now.Add(auth.PairingCodeTTL),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"code":       code,
		"sid":        rec.SID,
		"verify_url": loginURL(s.publicBaseURL(r), code),
		"poll_token": poll,
		"expires_at": rec.ExpiresAt,
		"interval":   loginPollInterval,
	})
}

func loginStatus(p store.PairingCode, now time.Time) string {
	switch {
	case p.RedeemedAt != nil:
		return "redeemed"
	case p.DeniedAt != nil:
		return "denied"
	case !now.Before(p.ExpiresAt):
		return "expired"
	case p.ApprovedAt != nil:
		return "approved"
	}
	return "pending"
}

func (s *Server) loginCodeFromPath(w http.ResponseWriter, r *http.Request) (store.PairingCode, bool) {
	canon, err := auth.NormalizePairingCode(r.PathValue("code"))
	var rec store.PairingCode
	if err == nil {
		rec, err = s.store.GetPairingCodeByHash(r.Context(), auth.HashPairingCode(canon))
	}
	if err != nil || rec.Purpose != store.PairingPurposeLogin {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown sign-in code"})
		return rec, false
	}
	return rec, true
}

// handleGetLogin serves GET /api/auth/device-code/{code}: the approval page shows what
// is asking to sign in.
func (s *Server) handleGetLogin(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.loginCodeFromPath(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sid":        rec.SID,
		"name":       rec.DeviceName,
		"kind":       rec.Kind,
		"created_at": rec.CreatedAt,
		"created_ip": rec.CreatedIP,
		"expires_at": rec.ExpiresAt,
		"status":     loginStatus(rec, s.auth.Now()),
	})
}

// handleDecideLogin serves POST /api/auth/device-code/{code}/approve|deny.
func (s *Server) handleDecideLogin(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec, ok := s.loginCodeFromPath(w, r)
		if !ok {
			return
		}
		user := identityFrom(r.Context()).User
		done, err := s.store.DecidePairingCode(r.Context(), rec.ID, approve, user, s.auth.Now())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if !done {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "this sign-in request is no longer pending", "status": loginStatus(rec, s.auth.Now())})
			return
		}
		action, verb := AuditDeviceLoginDeny, "Denied"
		if approve {
			action, verb = AuditDeviceLoginApprove, "Approved"
		}
		s.hub.recordAudit(r.Context(), store.AuditEvent{
			Action:   action,
			Message:  verb + " sign-in of " + rec.DeviceName + " (" + rec.Kind + ") with code " + rec.SID,
			MetaJSON: auditMeta(map[string]any{"code_sid": rec.SID, "ip": rec.CreatedIP}),
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "approved": approve})
	}
}

// handlePollLogin serves POST /api/auth/device-code/poll {poll_token}: once a browser
// approved the code, the first poll receives the new credential (and only the first).
func (s *Server) handlePollLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PollToken string `json:"poll_token"`
	}
	ip := auth.ClientIP(r)
	if !s.pollLimiter.Allow(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "polling too fast", "status": "slow_down"})
		return
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	rec, err := s.store.GetPairingCodeByPollHash(r.Context(), auth.HashSecret(strings.TrimSpace(body.PollToken)))
	if err != nil || rec.Purpose != store.PairingPurposeLogin || strings.TrimSpace(body.PollToken) == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown sign-in request", "status": "unknown"})
		return
	}
	status := loginStatus(rec, s.auth.Now())
	if status != "approved" {
		writeJSON(w, http.StatusOK, map[string]any{"status": status})
		return
	}
	d, secret, ok, err := s.redeemCode(r.Context(), rec, rec.DeviceName, rec.Kind, ip, auth.UserAgentSummary(r.UserAgent()))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"status": "redeemed"})
		return
	}
	s.hub.recordAudit(withAuditActor(r.Context(), cmpOr(rec.ApprovedBy, "system"), auditSourceCLI), store.AuditEvent{
		Action:   AuditDeviceLogin,
		Message:  "Signed in " + d.Name + " (" + d.SID + ", " + d.Kind + ") with code " + rec.SID,
		MetaJSON: auditMeta(map[string]any{"device_sid": d.SID, "code_sid": rec.SID, "ip": ip}),
	})
	s.hub.broadcastFleetUpdate("device", s.viewDevice(d, ""))
	writeJSON(w, http.StatusOK, map[string]any{"status": "approved", "token": secret, "device": s.viewDevice(d, d.ID)})
}

// --- Recovery key rotation ---

// RecoveryKeyRotator persists a new recovery key (to the tokens file). Set by `syre
// serve`; nil when the key comes from a flag, the environment or a config file.
type RecoveryKeyRotator func(newKey string) error

// SetRecoveryKeyRotator installs the persistence hook used by recovery key rotation.
func (s *Server) SetRecoveryKeyRotator(f RecoveryKeyRotator) {
	s.cfgMu.Lock()
	s.rotateRecovery = f
	s.cfgMu.Unlock()
}

// errRecoveryKeyExternal means the recovery key is not managed by the tokens file.
var errRecoveryKeyExternal = errors.New("the recovery key is set by --recovery-key, SYRE_RECOVERY_KEY or the config file; change it there")

// RotateRecoveryKey generates and persists a new recovery key; the old one stops
// working immediately. The key is never returned or logged.
func (s *Server) RotateRecoveryKey(ctx context.Context) error {
	s.cfgMu.RLock()
	persist := s.rotateRecovery
	s.cfgMu.RUnlock()
	if persist == nil {
		return errRecoveryKeyExternal
	}
	k, err := auth.GenerateToken(32)
	if err != nil {
		return err
	}
	if err := persist(k); err != nil {
		return err
	}
	s.auth.SetRecoveryKey(k)
	s.hub.recordAudit(ctx, store.AuditEvent{Action: AuditRecoveryRotate, Message: "Rotated the recovery key"})
	return nil
}

// handleRotateRecoveryKey serves POST /api/auth/recovery-key/rotate.
func (s *Server) handleRotateRecoveryKey(w http.ResponseWriter, r *http.Request) {
	if err := s.RotateRecoveryKey(r.Context()); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errRecoveryKeyExternal) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func launchNote(launch bool) string {
	if launch {
		return " via the local launcher (syre open)"
	}
	return ""
}
