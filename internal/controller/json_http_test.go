package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSONBody(t *testing.T) {
	type body struct {
		Title string `json:"title"`
	}
	cases := []struct {
		name       string
		in         string
		allowEmpty bool
		ok         bool
		status     int
		title      string
	}{
		{"object", `{"title":"a"}`, false, true, 0, "a"},
		{"empty rejected", ``, false, false, http.StatusBadRequest, ""},
		{"empty allowed", "  \n", true, true, 0, ""},
		{"truncated", `{"title":`, true, false, http.StatusBadRequest, ""},
		// v1's Decoder ignored data after the first value; the whole body must be one value now.
		{"trailing data", `{"title":"a"} {"title":"b"}`, false, false, http.StatusBadRequest, ""},
		// Names are matched exactly (v1 matched "Title" case-insensitively).
		{"case sensitive", `{"Title":"a"}`, false, true, 0, ""},
		{"duplicate name", `{"title":"a","title":"b"}`, false, false, http.StatusBadRequest, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.in))
			w := httptest.NewRecorder()
			var b body
			ok := decodeJSONBody(w, r, &b, tc.allowEmpty)
			if ok != tc.ok || (ok && b.Title != tc.title) {
				t.Fatalf("ok=%v title=%q, want ok=%v title=%q", ok, b.Title, tc.ok, tc.title)
			}
			if !ok && w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
		})
	}
	// Oversized bodies are reported as 413.
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"title":"`+strings.Repeat("x", 100)+`"}`))
	w := httptest.NewRecorder()
	r.Body = http.MaxBytesReader(w, r.Body, 10)
	var b body
	if decodeJSON(w, r, &b) || w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status %d", w.Code)
	}
}

func TestWriteJSONWireFormat(t *testing.T) {
	type item struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	w := httptest.NewRecorder()
	// Invalid UTF-8 (e.g. a file name) is replaced rather than producing an empty
	// response; nil slices are [] (v1: null).
	writeJSON(w, http.StatusOK, []item{{Name: "bad\xffname"}})
	if got, want := w.Body.String(), `[{"name":"bad`+"�"+`name","tags":[]}]`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Fatal("content type")
	}
	w = httptest.NewRecorder()
	var none []item
	writeJSON(w, http.StatusOK, none)
	if w.Body.String() != `[]` {
		t.Fatalf("nil list = %s, want []", w.Body.String())
	}
}
