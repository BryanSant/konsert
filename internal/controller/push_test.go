package controller

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// fakeBrowserKeys is a browser subscription's key pair (p256dh public key, auth secret).
func fakeBrowserKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	_, _ = rand.Read(secret)
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(secret)
}

// pushRecorder stands in for the push service: it records what was sent.
type pushRecorder struct {
	mu   sync.Mutex
	sent []map[string]any // payload notification + topic/urgency/endpoint
	code int
}

func (r *pushRecorder) send(_ context.Context, payload []byte, s *webpush.Subscription, o *webpush.Options) (*http.Response, error) {
	var p struct {
		Notification map[string]any `json:"notification"`
	}
	_ = json.Unmarshal(payload, &p)
	n := p.Notification
	n["_topic"], n["_urgency"], n["_endpoint"] = o.Topic, string(o.Urgency), s.Endpoint
	r.mu.Lock()
	r.sent = append(r.sent, n)
	code := r.code
	r.mu.Unlock()
	if code == 0 {
		code = http.StatusCreated
	}
	return &http.Response{StatusCode: code, Body: http.NoBody}, nil
}

func (r *pushRecorder) all() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.sent...)
}

func subscribe(t *testing.T, cs *chatServer, endpoint string, prefs map[string]any) {
	t.Helper()
	p256, auth := fakeBrowserKeys(t)
	body := map[string]any{"endpoint": endpoint, "keys": map[string]any{"p256dh": p256, "auth": auth}}
	if prefs != nil {
		body["prefs"] = prefs
	}
	if code, raw := cs.api(t, "POST", "/api/push/subscriptions", body); code != 200 {
		t.Fatalf("subscribe: %d %s", code, raw)
	}
}

// A real, encrypted push reaches the push service with VAPID auth and the session's topic.
func TestPushTestNotificationIsEncryptedWithVAPID(t *testing.T) {
	cs := newChatServer(t)
	var got *http.Request
	var gotBody []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		gotBody = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(gotBody)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	cs.h.push.send = func(ctx context.Context, payload []byte, s *webpush.Subscription, o *webpush.Options) (*http.Response, error) {
		o.HTTPClient = srv.Client()
		return webpush.SendNotificationWithContext(ctx, payload, s, o)
	}
	code, raw := cs.api(t, "GET", "/api/push", nil)
	var cfg struct {
		PublicKey string `json:"public_key"`
	}
	if code != 200 || json.Unmarshal(raw, &cfg) != nil || cfg.PublicKey == "" {
		t.Fatalf("config: %d %s", code, raw)
	}
	// The key pair is made once and kept.
	_, raw2 := cs.api(t, "GET", "/api/push", nil)
	if !strings.Contains(string(raw2), cfg.PublicKey) {
		t.Fatal("VAPID key changed between calls")
	}
	if code, _ := cs.api(t, "POST", "/api/push/test", nil); code != http.StatusConflict {
		t.Fatalf("test without a subscription: %d", code)
	}
	subscribe(t, cs, srv.URL+"/push/abc", nil)
	code, raw = cs.api(t, "POST", "/api/push/test", nil)
	if code != 200 || !strings.Contains(string(raw), `"sent":1`) {
		t.Fatalf("test: %d %s", code, raw)
	}
	if got == nil || got.Header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(got.Header.Get("Authorization"), "vapid t=") ||
		!strings.Contains(got.Header.Get("Authorization"), "k="+cfg.PublicKey) || got.Header.Get("TTL") == "" || len(gotBody) == 0 {
		t.Fatalf("push request: %+v", got.Header)
	}
	if strings.Contains(string(gotBody), "Notifications work") {
		t.Fatal("payload sent in the clear")
	}
}

func TestPushRoutingPrefsPresenceAndGoneSubscriptions(t *testing.T) {
	cs := newChatServer(t)
	rec := &pushRecorder{}
	cs.h.push.send = rec.send
	ctx := context.Background()
	subscribe(t, cs, "https://push.example/one", nil)
	// A task waits: one push, high urgency, opening the session, tagged per session.
	if err := cs.st.CreateTask(ctx, store.Task{ID: "t-1", SID: "t-push1", Agent: "claude", Title: "Fix the cache", Status: protocol.TaskStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.h.setTaskStatus(ctx, "t-1", protocol.TaskStatusWaitingInput, "", "", nil, causeScheduler); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "push", func() bool { return len(rec.all()) == 1 })
	n := rec.all()[0]
	if n["title"] != "Fix the cache waits for you" || n["_urgency"] != "high" || n["tag"] != "session:t-1" || n["_topic"] == "" ||
		!strings.Contains(asJSON(n), `"url":"/command/t-push1"`) {
		t.Fatalf("waiting push: %+v", n)
	}
	// Scratch sessions and shells never push.
	_ = cs.st.CreateTask(ctx, store.Task{ID: "t-2", SID: "t-push2", Agent: "shell", Status: protocol.TaskStatusRunning})
	_, _ = cs.h.setTaskStatus(ctx, "t-2", protocol.TaskStatusFailed, "", "", nil, causeScheduler)
	// Completed is switched off on this device.
	if code, _ := cs.api(t, "PUT", "/api/push/prefs", map[string]any{"off": []string{"completed"}}); code != 200 {
		t.Fatal("prefs")
	}
	_, _ = cs.h.setTaskStatus(ctx, "t-1", protocol.TaskStatusCompleted, "", "", nil, causeScheduler)
	time.Sleep(200 * time.Millisecond)
	if len(rec.all()) != 1 {
		t.Fatalf("pushed anyway: %+v", rec.all()[1:])
	}
	// A visible, focused page on the device shows the alert itself: no push.
	c := &browserClient{deviceID: "", foreground: true, cancel: func() {}, ctx: context.Background(), sendCh: make(chan wsFrame, 64),
		subs: map[string]bool{}, chatSubs: map[string]bool{}}
	cs.h.mu.Lock()
	cs.h.clients[c] = struct{}{}
	cs.h.mu.Unlock()
	t.Cleanup(func() {
		cs.h.mu.Lock()
		delete(cs.h.clients, c)
		cs.h.mu.Unlock()
	})
	_ = cs.st.CreateTask(ctx, store.Task{ID: "t-3", SID: "t-push3", Agent: "claude", Status: protocol.TaskStatusRunning})
	_, _ = cs.h.setTaskStatus(ctx, "t-3", protocol.TaskStatusFailed, "", "", nil, causeScheduler)
	time.Sleep(200 * time.Millisecond)
	if len(rec.all()) != 1 {
		t.Fatalf("pushed to a foreground device: %+v", rec.all()[1:])
	}
	cs.h.mu.Lock()
	delete(cs.h.clients, c)
	cs.h.mu.Unlock()
	// The push service says the subscription is gone: it is dropped.
	rec.mu.Lock()
	rec.code = http.StatusGone
	rec.mu.Unlock()
	_ = cs.st.CreateTask(ctx, store.Task{ID: "t-4", SID: "t-push4", Agent: "claude", Status: protocol.TaskStatusRunning})
	_, _ = cs.h.setTaskStatus(ctx, "t-4", protocol.TaskStatusFailed, "", "", nil, causeScheduler)
	waitFor(t, 3*time.Second, "gone subscription dropped", func() bool {
		subs, _ := cs.st.ListPushSubscriptions(ctx, "")
		return len(subs) == 0
	})
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestPushQuestionsP1AtOnceOthersAfterThreshold(t *testing.T) {
	cs := newChatServer(t)
	rec := &pushRecorder{}
	cs.h.push.send = rec.send
	ctx := context.Background()
	subscribe(t, cs, "https://push.example/q", nil)
	tk, err := cs.st.CreateTicket(ctx, store.Ticket{ProjectID: store.DefaultProjectID, Kind: store.TicketKindFeature, Title: "Urgent", Priority: "P1", Status: store.TicketStatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := cs.st.CreateChatSession(ctx, store.ChatSession{ID: "c-p1", SID: "c-p1x", Backend: store.ChatBackendAgent, TicketID: tk.ID, Status: protocol.ChatStatusWorking, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	mk := func(chatID, prompt string, since time.Time) {
		t.Helper()
		if _, err := cs.st.CreateQuestion(ctx, store.Question{ChatID: chatID, Kind: "decision", Source: store.QuestionSourceAskUser, Prompt: prompt,
			Status: store.QuestionOpen, BlockedSince: since, CreatedAt: since, UpdatedAt: since}); err != nil {
			t.Fatal(err)
		}
	}
	mk("c-p1", "Ship the hotfix now?", now)            // P1: at once
	mk("c-other", "Rename the module?", now)           // fresh, not P1: waits
	mk("c-old", "Which DB?", now.Add(-20*time.Minute)) // blocked 20 min: due
	if n := cs.h.pushQuestionSweep(ctx, now); n != 2 {
		t.Fatalf("pushed %d questions, want 2", n)
	}
	waitFor(t, 3*time.Second, "question pushes", func() bool { return len(rec.all()) == 2 })
	titles := asJSON(rec.all())
	if !strings.Contains(titles, "P1: a question waits for you") || !strings.Contains(titles, "Ship the hotfix now?") || !strings.Contains(titles, "Which DB?") {
		t.Fatalf("pushes: %s", titles)
	}
	// Once each: a second sweep pushes nothing new until the fresh one is due.
	if n := cs.h.pushQuestionSweep(ctx, now.Add(time.Minute)); n != 0 {
		t.Fatalf("re-pushed %d", n)
	}
	if n := cs.h.pushQuestionSweep(ctx, now.Add(16*time.Minute)); n != 1 {
		t.Fatalf("the fresh question after the threshold: %d", n)
	}
}

func TestRevokedDeviceLosesItsSubscriptions(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	now := time.Now().UTC()
	d, err := cs.st.CreateDevice(ctx, store.Device{Name: "phone", Kind: "phone", SecretHash: "h1", CreatedAt: now, LastUsedAt: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	p256, auth := fakeBrowserKeys(t)
	_ = cs.st.UpsertPushSubscription(ctx, store.PushSubscription{Endpoint: "https://push.example/phone", DeviceID: d.ID, P256DH: p256, Auth: auth})
	if _, err := cs.srv.revokeDevice(ctx, d, "Revoked"); err != nil {
		t.Fatal(err)
	}
	if subs, _ := cs.st.ListPushSubscriptions(ctx, d.ID); len(subs) != 0 {
		t.Fatalf("subscriptions left: %+v", subs)
	}
}
