package controller

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// The fleet workspace folder (SYR-W3SVZ): one home-relative folder that means the same
// on every machine, so a Commander chat sending work to several machines has one place
// to start it. Stored as parts joined by "/" ("src", "source/repos").

const workspaceFolderSetting = "workspace_folder"

// defaultWorkspaceFolder is used when no machine reports a code folder.
const defaultWorkspaceFolder = "src"

var driveLetter = regexp.MustCompile(`^[A-Za-z]:`)

// normalizeWorkspaceFolder turns "~/src", "src/", "~\\source\\repos" into home-relative
// parts joined by "/". Absolute paths, drive letters, "." and ".." are refused; "" (or
// "~") means the default.
func normalizeWorkspaceFolder(in string) (string, error) {
	s := strings.TrimSpace(in)
	if s == "" || s == "~" || s == "~/" || s == `~\` {
		return "", nil
	}
	if strings.HasPrefix(s, "~/") || strings.HasPrefix(s, `~\`) {
		s = s[2:]
	} else if strings.HasPrefix(s, "/") || strings.HasPrefix(s, `\`) {
		return "", errors.New("the workspace folder is relative to each machine's home: use ~/src, not an absolute path")
	}
	if driveLetter.MatchString(s) {
		return "", errors.New("the workspace folder is relative to each machine's home: no drive letters")
	}
	var parts []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == '\\' }) {
		p = strings.TrimSpace(p)
		switch {
		case p == "" || p == ".":
			continue
		case p == ".." || strings.HasPrefix(p, "~"):
			return "", errors.New("the workspace folder must stay inside each machine's home (no .. or ~)")
		case strings.ContainsAny(p, `:*?"<>|`):
			return "", errors.New("the workspace folder has characters Windows does not allow in a folder name")
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, "/"), nil
}

// workspaceSuggestion is a folder found under home on some machines.
type workspaceSuggestion struct {
	Path string `json:"path"`
	// MachineIDs have the folder; Count is len(MachineIDs) of Total machines reporting.
	MachineIDs []string `json:"machine_ids"`
	Count      int      `json:"count"`
	Total      int      `json:"total"`
	Repos      int      `json:"repos"`
}

// workspaceSuggestions ranks the folders the machines' home surveys report: on more
// machines first, then more git repos. Spellings that differ only in case are one folder.
func (h *Hub) workspaceSuggestions(ctx context.Context) ([]workspaceSuggestion, int) {
	workers, _ := h.store.ListWorkers(ctx)
	byKey := map[string]*workspaceSuggestion{}
	var order []string
	total := 0
	for _, w := range workers {
		var facts protocol.MachineFacts
		if w.FactsJSON == "" || json.Unmarshal([]byte(w.FactsJSON), &facts, protocol.JSONOptions) != nil || len(facts.HomeFolders) == 0 {
			continue
		}
		total++
		for _, f := range facts.HomeFolders {
			key := strings.ToLower(f.Path)
			sg, ok := byKey[key]
			if !ok {
				sg = &workspaceSuggestion{Path: f.Path, MachineIDs: []string{}}
				byKey[key] = sg
				order = append(order, key)
			}
			if !slices.Contains(sg.MachineIDs, w.ID) {
				sg.MachineIDs = append(sg.MachineIDs, w.ID)
			}
			sg.Repos += f.Repos
		}
	}
	out := make([]workspaceSuggestion, 0, len(order))
	for _, k := range order {
		sg := byKey[k]
		sg.Count, sg.Total = len(sg.MachineIDs), total
		out = append(out, *sg)
	}
	slices.SortStableFunc(out, func(a, b workspaceSuggestion) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(b.Repos, a.Repos), cmp.Compare(a.Path, b.Path))
	})
	return out, total
}

// workspaceFolder is the fleet workspace folder: the user's choice, else the top
// suggestion, else ~/src.
func (h *Hub) workspaceFolder(ctx context.Context) string {
	saved, _ := h.store.GetAllSettings(ctx)
	if f := saved[workspaceFolderSetting]; f != "" {
		return f
	}
	if sg, _ := h.workspaceSuggestions(ctx); len(sg) > 0 {
		return sg[0].Path
	}
	return defaultWorkspaceFolder
}

// chatWorkFolder is a chat's own folder, else the fleet workspace folder.
func (h *Hub) chatWorkFolder(ctx context.Context, chatID string) string {
	if chatID != "" {
		if f, err := h.store.GetChatWorkFolder(ctx, chatID); err == nil && f != "" {
			return f
		}
	}
	return h.workspaceFolder(ctx)
}

// defaultDispatchFolder is where Commander work without a repo starts: with machines to
// choose from, the chat's workspace folder (created where the work lands); for a chat
// limited to one machine, and for everyone else, home as before.
func (h *Hub) defaultDispatchFolder(ctx context.Context) (string, bool) {
	o, ok := chatOriginFrom(ctx)
	if !ok || !o.commander {
		return "~", false
	}
	if sc := h.scopeOfChat(ctx, o.id); sc.target.Kind == store.TargetMachine {
		return "~", false
	}
	return "~/" + h.chatWorkFolder(ctx, o.id), true
}

// --- REST ----------------------------------------------------------------------------

func (s *Server) registerWorkspaceFolderRoutes() {
	s.mux.Handle("GET /api/workspace-folders", s.requireBrowserAuth(s.handleWorkspaceFolders))
	s.mux.Handle("GET /api/chats/{id}/work-folder", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleGetChatWorkFolder)))
	s.mux.Handle("PUT /api/chats/{id}/work-folder", s.requireBrowserAuth(s.byRef(ids.Chat, s.handleSetChatWorkFolder)))
}

func (s *Server) handleWorkspaceFolders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sg, total := s.hub.workspaceSuggestions(ctx)
	saved, _ := s.store.GetAllSettings(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"suggestions": sg,
		"total":       total,
		"saved":       saved[workspaceFolderSetting],
		"effective":   s.hub.workspaceFolder(ctx),
	})
}

type chatWorkFolderView struct {
	ChatID string `json:"chat_id"`
	// Folder is the chat's own choice ("" = the fleet default); Effective is what it uses.
	Folder    string `json:"folder"`
	Effective string `json:"effective"`
}

func (s *Server) handleGetChatWorkFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if _, err := s.store.GetChatSession(ctx, id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "chat not found"})
		return
	}
	f, _ := s.store.GetChatWorkFolder(ctx, id)
	writeJSON(w, http.StatusOK, chatWorkFolderView{ChatID: id, Folder: f, Effective: s.hub.chatWorkFolder(ctx, id)})
}

func (s *Server) handleSetChatWorkFolder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if _, err := s.store.GetChatSession(ctx, id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "chat not found"})
		return
	}
	var body struct {
		Folder string `json:"folder"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	f, err := normalizeWorkspaceFolder(body.Folder)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.store.SetChatWorkFolder(ctx, id, f); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, chatWorkFolderView{ChatID: id, Folder: f, Effective: s.hub.chatWorkFolder(ctx, id)})
}
