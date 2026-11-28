package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/BryanSant/syre/internal/policy"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Web Push (SYR-Z15J6): the hub notifies every signed-in device that subscribed, desktop
// or phone, with no Syre page open. VAPID keys are generated once and kept in the hub's
// settings. A device whose Syre page is visible and focused shows the alert itself and
// gets no push; repeats for one session collapse (one topic per session).

// Browser presence (hub-local): {"visible": bool, "focused": bool}.
const msgPresence = "presence"

type presencePayload struct {
	Visible bool `json:"visible"`
	Focused bool `json:"focused"`
}

// Push categories.
const (
	PushWaiting   = "waiting"   // a session waits for you
	PushQuestion  = "question"  // a P1 question, or one blocked too long
	PushFailed    = "failed"    // a session failed
	PushCompleted = "completed" // a session finished
)

// PushPrefs are a device's notification choices.
type PushPrefs struct {
	// Off lists the categories switched off (all are on by default).
	Off []string `json:"off,omitempty"`
	// Quiet is a daily window with no notifications, in TimeZone.
	Quiet *protocol.QuietWindow `json:"quiet,omitzero"`
	// TimeZone is an IANA name for Quiet.
	TimeZone string `json:"time_zone,omitempty"`
}

func (p PushPrefs) wants(category string, now time.Time) bool {
	for _, c := range p.Off {
		if c == category {
			return false
		}
	}
	if p.Quiet != nil && p.TimeZone != "" {
		mp := &protocol.MachinePolicy{QuietHours: []protocol.QuietWindow{*p.Quiet}, TimeZone: p.TimeZone}
		if _, quiet := policy.InQuietHours(mp, now); quiet {
			return false
		}
	}
	return true
}

// pushMessage is one notification.
type pushMessage struct {
	Category string
	Title    string
	Body     string
	// URL opens when the notification is clicked (a path in the PWA).
	URL string
	// Tag collapses repeats about the same thing ("session:<id>").
	Tag     string
	Urgency webpush.Urgency
}

// Settings keys of the VAPID key pair.
const (
	settingVAPIDPublic  = "push_vapid_public"
	settingVAPIDPrivate = "push_vapid_private"
)

type pushState struct {
	mu   sync.Mutex
	pub  string
	priv string
	// send is webpush.SendNotificationWithContext (tests replace it).
	send func(ctx context.Context, payload []byte, s *webpush.Subscription, o *webpush.Options) (*http.Response, error)
}

// vapidKeys returns the hub's VAPID keys, generating and storing them on first use.
func (h *Hub) vapidKeys(ctx context.Context) (pub, priv string, err error) {
	h.push.mu.Lock()
	defer h.push.mu.Unlock()
	if h.push.pub != "" {
		return h.push.pub, h.push.priv, nil
	}
	kv, err := h.store.GetAllSettings(ctx)
	if err != nil {
		return "", "", err
	}
	pub, priv = kv[settingVAPIDPublic], kv[settingVAPIDPrivate]
	if pub == "" || priv == "" {
		priv, pub, err = webpush.GenerateVAPIDKeys()
		if err != nil {
			return "", "", err
		}
		if err := h.store.SetSettings(ctx, map[string]string{settingVAPIDPublic: pub, settingVAPIDPrivate: priv}); err != nil {
			return "", "", err
		}
	}
	h.push.pub, h.push.priv = pub, priv
	return pub, priv, nil
}

// deviceForeground reports whether a page of the device is visible and focused.
func (h *Hub) deviceForeground(deviceID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if c.deviceID != deviceID {
			continue
		}
		c.mu.Lock()
		fg := c.foreground
		c.mu.Unlock()
		if fg {
			return true
		}
	}
	return false
}

// pushTopic makes a tag a Web Push topic: at most 32 URL-safe characters.
func pushTopic(tag string) string {
	if tag == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(tag))
	return hex.EncodeToString(sum[:16])
}

// pushPayload is the notification as Angular's service worker shows it: a click opens
// (or focuses) the PWA at the URL.
func pushPayload(m pushMessage) ([]byte, error) {
	n := map[string]any{
		"title": m.Title,
		"body":  m.Body,
		"icon":  "/logo.svg",
		"badge": "/logo.svg",
		"tag":   m.Tag,
		"data": map[string]any{
			"url": m.URL,
			"onActionClick": map[string]any{
				"default": map[string]any{"operation": "navigateLastFocusedOrOpen", "url": m.URL},
			},
		},
	}
	if m.Tag != "" {
		n["renotify"] = true
	}
	return json.Marshal(map[string]any{"notification": n})
}

// notifyDevices sends m to every subscribed device that wants it, in the background.
// Devices with a visible, focused Syre page are skipped: the page shows the alert.
func (h *Hub) notifyDevices(m pushMessage) {
	go func() {
		ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
		defer cancel()
		subs, err := h.store.ListPushSubscriptions(ctx, "")
		if err != nil || len(subs) == 0 {
			return
		}
		now := time.Now()
		for _, s := range subs {
			var prefs PushPrefs
			_ = json.Unmarshal([]byte(s.PrefsJSON), &prefs)
			if !prefs.wants(m.Category, now) || h.deviceForeground(s.DeviceID) {
				continue
			}
			h.sendPush(ctx, s, m)
		}
	}()
}

// sendPush delivers one notification to one subscription; a gone subscription
// (404/410) is deleted. It reports whether the push service accepted it.
func (h *Hub) sendPush(ctx context.Context, s store.PushSubscription, m pushMessage) bool {
	pub, priv, err := h.vapidKeys(ctx)
	if err != nil {
		h.logger.Warn("push: no VAPID keys", "err", err)
		return false
	}
	payload, err := pushPayload(m)
	if err != nil {
		return false
	}
	send := h.push.send
	if send == nil {
		send = webpush.SendNotificationWithContext
	}
	urgency := m.Urgency
	if urgency == "" {
		urgency = webpush.UrgencyNormal
	}
	resp, err := send(ctx, payload, &webpush.Subscription{Endpoint: s.Endpoint, Keys: webpush.Keys{P256dh: s.P256DH, Auth: s.Auth}},
		&webpush.Options{Subscriber: h.pushSubscriber(), VAPIDPublicKey: pub, VAPIDPrivateKey: priv, TTL: 24 * 60 * 60,
			Urgency: urgency, Topic: pushTopic(m.Tag)})
	if err != nil {
		_ = h.store.RecordPushResult(ctx, s.Endpoint, time.Now().UTC(), "send failed: "+shortErr(err))
		return false
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		_ = h.store.DeletePushSubscription(ctx, s.Endpoint)
		return false
	case resp.StatusCode/100 == 2:
		_ = h.store.RecordPushResult(ctx, s.Endpoint, time.Now().UTC(), "")
		return true
	default:
		_ = h.store.RecordPushResult(ctx, s.Endpoint, time.Now().UTC(), fmt.Sprintf("push service answered %d", resp.StatusCode))
		return false
	}
}

func shortErr(err error) string {
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

// pushSubscriber is the VAPID contact: the hub's public URL, else a neutral one (no
// personal address).
func (h *Hub) pushSubscriber() string {
	if h.publicURL != "" && strings.HasPrefix(h.publicURL, "https://") {
		return h.publicURL
	}
	return "https://syre.invalid"
}

// --- REST ------------------------------------------------------------------------------

// handlePushConfig serves GET /api/push: the VAPID public key and this device's
// subscriptions and prefs.
func (s *Server) handlePushConfig(w http.ResponseWriter, r *http.Request) {
	pub, _, err := s.hub.vapidKeys(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	subs, _ := s.store.ListPushSubscriptions(r.Context(), identityFrom(r.Context()).DeviceID())
	prefs := PushPrefs{}
	endpoints := []string{}
	for _, sub := range subs {
		endpoints = append(endpoints, sub.Endpoint)
		_ = json.Unmarshal([]byte(sub.PrefsJSON), &prefs)
	}
	writeJSON(w, http.StatusOK, map[string]any{"public_key": pub, "endpoints": endpoints, "prefs": prefs})
}

// handlePushSubscribe serves POST /api/push/subscriptions {endpoint, keys:{p256dh,auth}, prefs}.
func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256DH string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
		Prefs *PushPrefs `json:"prefs"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !strings.HasPrefix(body.Endpoint, "https://") || body.Keys.P256DH == "" || body.Keys.Auth == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a push subscription needs an https endpoint and its keys"})
		return
	}
	prefs := PushPrefs{}
	if body.Prefs != nil {
		prefs = *body.Prefs
	}
	raw, _ := json.Marshal(prefs)
	sub := store.PushSubscription{Endpoint: body.Endpoint, DeviceID: identityFrom(r.Context()).DeviceID(), P256DH: body.Keys.P256DH,
		Auth: body.Keys.Auth, PrefsJSON: string(raw), UserAgent: shortUA(r.UserAgent())}
	if err := s.store.UpsertPushSubscription(r.Context(), sub); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePushUnsubscribe serves DELETE /api/push/subscriptions {endpoint}: only this
// device's own.
func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	subs, _ := s.store.ListPushSubscriptions(r.Context(), identityFrom(r.Context()).DeviceID())
	for _, sub := range subs {
		if sub.Endpoint == body.Endpoint {
			_ = s.store.DeletePushSubscription(r.Context(), sub.Endpoint)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePushPrefs serves PUT /api/push/prefs: this device's categories and quiet hours.
func (s *Server) handlePushPrefs(w http.ResponseWriter, r *http.Request) {
	var prefs PushPrefs
	if !decodeJSON(w, r, &prefs) {
		return
	}
	if prefs.Quiet != nil {
		if _, err := time.LoadLocation(prefs.TimeZone); err != nil || prefs.TimeZone == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "quiet hours need a time zone (an IANA name)"})
			return
		}
	}
	raw, _ := json.Marshal(prefs)
	subs, _ := s.store.ListPushSubscriptions(r.Context(), identityFrom(r.Context()).DeviceID())
	for _, sub := range subs {
		sub.PrefsJSON = string(raw)
		_ = s.store.UpsertPushSubscription(r.Context(), sub)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "prefs": prefs})
}

// handlePushTest serves POST /api/push/test: a test notification to this device's
// subscriptions, whatever its prefs and presence.
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	subs, err := s.store.ListPushSubscriptions(r.Context(), identityFrom(r.Context()).DeviceID())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(subs) == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this device has not turned on notifications"})
		return
	}
	sent := 0
	for _, sub := range subs {
		if s.hub.sendPush(r.Context(), sub, pushMessage{Category: "test", Title: "Syre", Body: "Notifications work on this device.", URL: "/", Tag: "test"}) {
			sent++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": sent, "subscriptions": len(subs)})
}

func shortUA(ua string) string {
	if len(ua) > 200 {
		return ua[:200]
	}
	return ua
}
