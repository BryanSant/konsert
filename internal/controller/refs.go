package controller

import (
	"context"
	"net/http"
	"strings"

	"github.com/BryanSant/syre/internal/ids"
	"github.com/BryanSant/syre/internal/store"
)

// Every entity has a UUIDv7 id (used internally and by the worker protocol) and a short sid
// (shown to users). API paths, query parameters, request bodies and MCP tool arguments
// accept either; they are resolved to the id here, once, at the boundary.

// resolveRef returns the id of the kind's record named by ref (its id or its sid). A ref
// that matches nothing is returned unchanged, so the caller's own lookup reports it as not
// found (and tests may use arbitrary id strings).
func resolveRef(ctx context.Context, st store.Store, kind ids.Kind, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || st == nil {
		return ref
	}
	if id, err := st.ResolveID(ctx, kind, ref); err == nil {
		return id
	}
	return ref
}

// resolveRefs resolves each pointed-to ref in place.
func resolveRefs(ctx context.Context, st store.Store, refs map[ids.Kind][]*string) {
	for kind, ptrs := range refs {
		for _, p := range ptrs {
			if p != nil {
				*p = resolveRef(ctx, st, kind, *p)
			}
		}
	}
}

// byRef resolves the {id} path value of a route naming a kind before calling next.
func (s *Server) byRef(kind ids.Kind, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ref := r.PathValue("id"); ref != "" {
			r.SetPathValue("id", resolveRef(r.Context(), s.store, kind, ref))
		}
		next(w, r)
	}
}

// queryRefKinds are the query parameters that name an entity.
var queryRefKinds = map[string]ids.Kind{
	"worker_id":  ids.Machine,
	"task_id":    ids.Task,
	"chat_id":    ids.Chat,
	"session_id": ids.Chat,
	"project_id": ids.Project,
	"repo_id":    ids.Repo,
	"ticket_id":  ids.Ticket,
	"parent_id":  ids.Ticket,
}

// withQueryRefs resolves entity-naming query parameters (worker_id=m-…) to ids.
func (s *Server) withQueryRefs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			q := r.URL.Query()
			changed := false
			for key, kind := range queryRefKinds {
				if v := q.Get(key); v != "" {
					if id := resolveRef(r.Context(), s.store, kind, v); id != v {
						q.Set(key, id)
						changed = true
					}
				}
			}
			if changed {
				r.URL.RawQuery = q.Encode()
			}
		}
		next.ServeHTTP(w, r)
	})
}
