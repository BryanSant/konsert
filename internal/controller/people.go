package controller

import (
	"context"
	"net/http"
	"os/user"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/syre/internal/realname"
)

// Display names for people (SYR-98VFV): the UI shows "Bryan Sant" where it used to show
// "bsant", with the login on hover. The hub detects the name of the account it runs as
// (git user.name, else the OS account's full name) in the background and refreshes it
// daily; a user can set their own in Settings, which wins. Agents keep their labels.

const displayNameSettingPrefix = "display_name:"

// detectNames is replaced in tests.
var detectNames = realname.Detect

type peopleNames struct {
	mu       sync.Mutex
	detected map[string]string // login -> detected display name
}

// hubLogin is the login of the account the hub runs as ("DOMAIN\user" -> "user").
func hubLogin() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	name := u.Username
	if i := strings.LastIndex(name, `\`); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// peopleLoop looks up the hub account's display name now and once a day.
func (h *Hub) peopleLoop(ctx context.Context) {
	refresh := func() {
		login := hubLogin()
		if login == "" {
			return
		}
		name := detectNames(ctx, login)
		h.people.mu.Lock()
		if h.people.detected == nil {
			h.people.detected = map[string]string{}
		}
		h.people.detected[login] = name
		h.people.mu.Unlock()
	}
	refresh()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}

// displayName is login's name: the user's own choice, else the detected one, else "".
// "local" (someone at the hub's own machine with no sign-in) is the owner.
func (h *Hub) displayName(ctx context.Context, login string) string {
	if login == "" {
		return ""
	}
	if login == "local" {
		login = hubLogin()
	}
	saved, _ := h.store.GetAllSettings(ctx)
	if v := strings.TrimSpace(saved[displayNameSettingPrefix+login]); v != "" {
		return v
	}
	h.people.mu.Lock()
	defer h.people.mu.Unlock()
	return h.people.detected[login]
}

type personView struct {
	Login       string `json:"login"`
	DisplayName string `json:"display_name,omitempty"`
	// Override is the name the user set themselves ("" = detected).
	Override string `json:"override,omitempty"`
}

// knownPeople lists every login with a known display name.
func (h *Hub) knownPeople(ctx context.Context) []personView {
	logins := map[string]bool{}
	h.people.mu.Lock()
	for l := range h.people.detected {
		logins[l] = true
	}
	h.people.mu.Unlock()
	saved, _ := h.store.GetAllSettings(ctx)
	for k := range saved {
		if l, ok := strings.CutPrefix(k, displayNameSettingPrefix); ok {
			logins[l] = true
		}
	}
	out := []personView{}
	for l := range logins {
		if n := h.displayName(ctx, l); n != "" {
			out = append(out, personView{Login: l, DisplayName: n, Override: strings.TrimSpace(saved[displayNameSettingPrefix+l])})
		}
	}
	return out
}

func (s *Server) registerPeopleRoutes() {
	s.mux.Handle("GET /api/people", s.requireBrowserAuth(s.handleListPeople))
	s.mux.Handle("PUT /api/people/me", s.requireBrowserAuth(s.handleSetMyName))
}

func (s *Server) handleListPeople(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"people": s.hub.knownPeople(r.Context()), "owner": hubLogin()})
}

// handleSetMyName sets (or with "" clears) the owner's display name: a hub belongs to
// one person, the account it runs as, whichever device they sign in from.
func (s *Server) handleSetMyName(w http.ResponseWriter, r *http.Request) {
	login := hubLogin()
	if login == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the hub's account has no login name"})
		return
	}
	var body struct {
		DisplayName string `json:"display_name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	name := strings.Join(strings.Fields(body.DisplayName), " ")
	if len([]rune(name)) > 80 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the name is longer than 80 characters"})
		return
	}
	if err := s.store.SetSettings(r.Context(), map[string]string{displayNameSettingPrefix + login: name}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	v := personView{Login: login, DisplayName: s.hub.displayName(r.Context(), login), Override: name}
	s.hub.broadcastFleetUpdate("person", v)
	writeJSON(w, http.StatusOK, v)
}
