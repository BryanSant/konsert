package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/forge"
)

func TestUpdatePRBase(t *testing.T) {
	var method, path string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	c := New(forge.Repo{Kind: forge.GitHub, Host: "forge.example", Owner: "org", Name: "app", APIBase: srv.URL}, "tok", srv.Client())
	if err := c.UpdatePRBase(context.Background(), forge.Repo{Kind: forge.GitHub, Host: "forge.example", Owner: "org", Name: "app", APIBase: srv.URL}, 7, "main"); err != nil {
		t.Fatal(err)
	}
	if method != "PATCH" || !strings.HasSuffix(path, "/pulls/7") || body["base"] != "main" {
		t.Fatalf("request %s %s %v", method, path, body)
	}
}
