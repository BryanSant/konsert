package controller

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Reference enrichment (PRD §5.8b): ka ticket ids, PR numbers, file paths and Syre
// sids in a question become cards (title, status, short excerpt). Resolution runs after
// the question is shown and never delays it; results are cached briefly.

// refCacheTTL is how long a resolved reference is reused.
var refCacheTTL = 2 * time.Minute

// refResolveTimeout bounds resolving one reference.
var refResolveTimeout = 5 * time.Second

type refCacheEntry struct {
	ref store.QuestionRef
	at  time.Time
}

var (
	// ka ids: an upper-case project prefix and five Crockford base32 characters (SYR-2Q0CH).
	ticketRe = regexp.MustCompile(`\b([A-Z][A-Z0-9]{1,9}-[0-9A-HJKMNP-TV-Z]{5})\b`)
	// PR numbers: "#123", "PR 123", "PR #123", "pull request 123".
	prRe = regexp.MustCompile(`(?i)(?:\bPR\s*#?|\bpull request\s*#?|(?:^|[\s(])#)(\d{1,6})\b`)
	// Syre sids: t-, c-, m-, q-, h-, rq-, ... followed by 5-12 lower-case Crockford chars.
	sidRe = regexp.MustCompile(`\b((?:t|c|m|p|r|s|ws|wt|h|rq|q)-[0-9a-hjkmnp-tv-z]{5,12})\b`)
	// File paths: something/with/slashes.ext or name.ext with a known code extension.
	fileRe = regexp.MustCompile("(?:^|[\\s`'\"(])((?:\\.{0,2}/)?(?:[\\w.@-]+/)+[\\w.@-]+\\.[A-Za-z0-9]{1,8}|[\\w-]+\\.(?:go|ts|tsx|js|py|rs|md|sql|yaml|yml|json|toml|css|html|sh))(?::\\d+)?\\b")
)

// detectRefs finds references in question texts (unresolved, de-duplicated, in order).
func detectRefs(texts ...string) []store.QuestionRef {
	var out []store.QuestionRef
	seen := map[string]bool{}
	add := func(kind, ref string) {
		key := kind + ":" + ref
		if seen[key] || len(out) >= 12 {
			return
		}
		seen[key] = true
		out = append(out, store.QuestionRef{Kind: kind, Ref: ref})
	}
	all := strings.Join(texts, "\n")
	for _, m := range ticketRe.FindAllStringSubmatch(all, -1) {
		add("ticket", m[1])
	}
	for _, m := range prRe.FindAllStringSubmatch(all, -1) {
		add("pr", "#"+m[1])
	}
	for _, m := range sidRe.FindAllStringSubmatch(all, -1) {
		kind, _, ok := ids.Parse(m[1])
		if !ok {
			continue
		}
		switch kind {
		case ids.Task:
			add("task", m[1])
		case ids.Chat:
			add("chat", m[1])
		case ids.Machine:
			add("machine", m[1])
		case ids.Question:
			add("question", m[1])
		}
	}
	for _, m := range fileRe.FindAllStringSubmatch(all, -1) {
		p := strings.TrimSpace(m[1])
		if strings.Contains(p, "://") || strings.HasPrefix(p, "www.") {
			continue
		}
		add("file", p)
	}
	return out
}

// kaShow runs `ka show <id> --json --offline` on the hub host (replaceable in tests).
func (h *Hub) kaShow(ctx context.Context, id string) ([]byte, error) {
	if h.kaShowFn != nil {
		return h.kaShowFn(ctx, id)
	}
	bin := h.KaPath
	if bin == "" {
		bin = "ka"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, path, "show", id, "--json", "--offline").Output()
}

// ghPRView runs `gh pr view` for a repository (replaceable in tests).
func (h *Hub) ghPRView(ctx context.Context, repo, number string) ([]byte, error) {
	if h.ghPRFn != nil {
		return h.ghPRFn(ctx, repo, number)
	}
	path, err := exec.LookPath("gh")
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, path, "pr", "view", number, "-R", repo, "--json", "title,state,url,body").Output()
}

// enrichQuestion resolves a question's references and stores the cards.
func (h *Hub) enrichQuestion(ctx context.Context, id string) {
	q, err := h.store.GetQuestion(ctx, id)
	if err != nil || len(q.Refs) == 0 {
		return
	}
	resolved := make([]store.QuestionRef, len(q.Refs))
	changed := false
	for i, r := range q.Refs {
		resolved[i] = h.resolveRefCached(ctx, q, r)
		if resolved[i] != r {
			changed = true
		}
	}
	if !changed {
		return
	}
	h.questions.mu.Lock()
	cur, err := h.store.GetQuestion(ctx, id)
	if err == nil {
		cur.Refs = resolved
		// A ticket title makes a better group label; the key stays the ticket id.
		err = h.store.UpdateQuestion(ctx, cur)
	}
	h.questions.mu.Unlock()
	if err == nil {
		h.broadcastQuestion(ctx, cur)
	}
}

func (h *Hub) resolveRefCached(ctx context.Context, q store.Question, r store.QuestionRef) store.QuestionRef {
	key := r.Kind + ":" + r.Ref
	if r.Kind == "file" || r.Kind == "pr" {
		key += "@" + q.ChatID // relative to the asking session's repository
	}
	h.questions.mu.Lock()
	h.questions.init()
	if e, ok := h.questions.refCache[key]; ok && time.Since(e.at) < refCacheTTL {
		h.questions.mu.Unlock()
		return e.ref
	}
	h.questions.mu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, refResolveTimeout)
	out := h.resolveRef(rctx, q, r)
	cancel()
	h.questions.mu.Lock()
	h.questions.refCache[key] = refCacheEntry{ref: out, at: time.Now()}
	h.questions.mu.Unlock()
	return out
}

// resolveRef resolves one reference; unresolvable ones are returned as they are.
func (h *Hub) resolveRef(ctx context.Context, q store.Question, r store.QuestionRef) store.QuestionRef {
	switch r.Kind {
	case "ticket":
		raw, err := h.kaShow(ctx, r.Ref)
		if err != nil {
			return r
		}
		var t struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Status string `json:"status"`
			Body   string `json:"body"`
		}
		if json.Unmarshal(raw, &t) != nil || t.Title == "" {
			return r
		}
		r.Title, r.Status, r.Resolved = t.Title, t.Status, true
		r.Excerpt = ticketExcerpt(t.Body)
	case "pr":
		repo := h.repoSlugFor(ctx, q)
		if repo == "" {
			return r
		}
		raw, err := h.ghPRView(ctx, repo, strings.TrimPrefix(r.Ref, "#"))
		if err != nil {
			return r
		}
		var pr struct {
			Title string `json:"title"`
			State string `json:"state"`
			URL   string `json:"url"`
			Body  string `json:"body"`
		}
		if json.Unmarshal(raw, &pr) != nil || pr.Title == "" {
			return r
		}
		r.Title, r.Status, r.URL, r.Resolved = pr.Title, strings.ToLower(pr.State), pr.URL, true
		r.Excerpt = ticketExcerpt(pr.Body)
	case "task":
		if t, err := h.store.GetTask(ctx, resolveRef(ctx, h.store, ids.Task, r.Ref)); err == nil {
			r.Title, r.Status, r.Resolved = oneLine(firstNonEmpty(t.Summary, t.Prompt), 120), t.Status, true
			r.Excerpt = oneLine(t.Prompt, 240)
		}
	case "chat":
		if cs, err := h.store.GetChatSession(ctx, resolveRef(ctx, h.store, ids.Chat, r.Ref)); err == nil {
			r.Title, r.Status, r.Resolved = cs.Title, cs.Status, true
			r.Excerpt = agentLabel(cs) + " on " + h.workerLabel(ctx, cs.WorkerID)
		}
	case "machine":
		if w, err := h.store.GetWorker(ctx, resolveRef(ctx, h.store, ids.Machine, r.Ref)); err == nil {
			r.Title, r.Status, r.Resolved = w.Name, w.Status, true
			r.Excerpt = strings.TrimSpace(w.OS + " " + w.Arch)
		}
	case "question":
		if other, err := h.store.GetQuestion(ctx, resolveRef(ctx, h.store, ids.Question, r.Ref)); err == nil {
			r.Title, r.Status, r.Resolved = oneLine(other.Prompt, 120), other.Status, true
		}
	case "file":
		if desc := h.fileActivity(ctx, q.ChatID, r.Ref); desc != "" {
			r.Excerpt, r.Resolved = desc, true
			r.Title = r.Ref
		}
	}
	return r
}

// ticketExcerpt takes the first meaningful paragraph of a ticket or PR body.
func ticketExcerpt(body string) string {
	for _, para := range strings.Split(body, "\n\n") {
		p := strings.TrimSpace(para)
		if p == "" || strings.HasPrefix(p, "#") || strings.HasPrefix(p, "<!--") {
			continue
		}
		return oneLine(p, 280)
	}
	return ""
}

// repoSlugFor returns owner/repo of the GitHub repository the asking session works in
// (the registered repo whose path contains its working directory), or "".
func (h *Hub) repoSlugFor(ctx context.Context, q store.Question) string {
	cs, err := h.store.GetChatSession(ctx, q.ChatID)
	if err != nil {
		return ""
	}
	repos, err := h.store.ListRepos(ctx, "", cs.WorkerID)
	if err != nil {
		return ""
	}
	best := ""
	bestLen := 0
	for _, r := range repos {
		if r.Path == "" || r.RemoteURL == "" || !(cs.Cwd == r.Path || strings.HasPrefix(cs.Cwd, strings.TrimRight(r.Path, "/")+"/")) {
			continue
		}
		if slug := githubSlug(r.RemoteURL); slug != "" && len(r.Path) > bestLen {
			best, bestLen = slug, len(r.Path)
		}
	}
	return best
}

var githubRe = regexp.MustCompile(`github\.com[:/]([\w.-]+)/([\w.-]+?)(?:\.git)?/?$`)

func githubSlug(remote string) string {
	m := githubRe.FindStringSubmatch(strings.TrimSpace(remote))
	if m == nil {
		return ""
	}
	return m[1] + "/" + m[2]
}

// fileActivity describes what the asking session did with a file ("edited 2× in this
// session"), from its tool calls.
func (h *Hub) fileActivity(ctx context.Context, chatID, path string) string {
	evs, err := h.store.ListChatEvents(ctx, chatID, 0, 5000)
	if err != nil {
		return ""
	}
	clean := strings.TrimPrefix(path, "./")
	if i := strings.LastIndex(clean, ":"); i > 0 && strings.Trim(clean[i+1:], "0123456789") == "" {
		clean = clean[:i]
	}
	counts := map[string]int{}
	seen := map[string]bool{}
	for _, e := range evs {
		if e.Kind != protocol.EventToolCall {
			continue
		}
		var ev protocol.AgentChatEvent
		if json.Unmarshal([]byte(e.JSON), &ev) != nil || ev.ToolCall == nil {
			continue
		}
		tc := ev.ToolCall
		if seen[tc.ID] {
			continue
		}
		match := false
		for _, p := range tc.Paths {
			if p == clean || strings.HasSuffix(p, "/"+clean) {
				match = true
			}
		}
		if !match && !strings.Contains(tc.Input, clean) {
			continue
		}
		seen[tc.ID] = true
		kind := tc.Kind
		if kind == "" {
			kind = "other"
		}
		counts[kind]++
	}
	var parts []string
	for _, k := range []string{"edit", "read", "execute", "search", "other"} {
		if n := counts[k]; n > 0 {
			verb := map[string]string{"edit": "edited", "read": "read", "execute": "used in commands", "search": "searched", "other": "touched"}[k]
			parts = append(parts, fmt.Sprintf("%s %d×", verb, n))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return capitalize(strings.Join(parts, ", ")) + " in this session"
}
