package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

func TestSetNarrationHeadlineAndNow(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	th.h.kaShowFn = func(_ context.Context, id string) ([]byte, error) {
		return []byte(`{"id":"` + id + `","title":"Questions and steering","status":"in_progress"}`), nil
	}
	proj := store.Project{WorkspaceID: store.DefaultWorkspaceID, Name: "Syre"}
	if err := th.st.CreateProject(ctx, proj); err != nil {
		t.Fatal(err)
	}
	projects, _ := th.st.ListProjects(ctx, "")
	var p store.Project
	for _, x := range projects {
		if x.Name == "Syre" {
			p = x
		}
	}
	res, err := th.svc.DispatchTask(ctx, DispatchRequest{Agent: "mock", RepoPath: "/repo", ProjectID: p.SID,
		Title: "Questions card on mobile", Prompt: "Implement SYR-2Q0CH on mobile"})
	if err != nil {
		t.Fatal(err)
	}
	// The ticket is detected and its title resolved in the background.
	waitFor(t, 3*time.Second, "ticket title", func() bool {
		tk := mustTask(t, th.st, res.TaskID)
		return tk.TicketID == "SYR-2Q0CH" && tk.TicketTitle == "Questions and steering"
	})
	if tk := mustTask(t, th.st, res.TaskID); tk.Title != "Questions card on mobile" {
		t.Fatalf("title %q", tk.Title)
	}

	// Headline by project name, now line by task sid.
	out, err := th.svc.SetNarration(ctx, SetNarrationInput{Project: "syre", Headline: "  Ship the questions\nqueue to mobile ", Task: res.TaskSID, Now: "Porting the card"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Headline != "Ship the questions queue to mobile" || out.ProjectID != p.ID || out.Now != "Porting the card" {
		t.Fatalf("result %+v", out)
	}
	got, _ := th.st.GetProject(ctx, p.ID)
	if got.Headline != out.Headline || got.HeadlineAt == nil {
		t.Fatalf("project %+v", got)
	}
	tk := mustTask(t, th.st, res.TaskID)
	if tk.Narration != "Porting the card" || tk.NarrationAt == nil {
		t.Fatalf("task %+v", tk)
	}
	// A headline with only a task goes to the task's project.
	if out, err := th.svc.SetNarration(ctx, SetNarrationInput{Task: res.TaskID, Headline: "New plan"}); err != nil || out.ProjectID != p.ID {
		t.Fatalf("headline via task: %+v %v", out, err)
	}

	for _, bad := range []SetNarrationInput{
		{},
		{Now: "orphan line"},
		{Headline: "no project"},
		{Project: "nope", Headline: "x"},
		{Task: "t-zzzzzzzz", Now: "x"},
		{Project: "syre", Headline: strings.Repeat("x", maxHeadline+1)},
		{Task: res.TaskID, Now: strings.Repeat("x", maxNarration+1)},
	} {
		if _, err := th.svc.SetNarration(ctx, bad); err == nil {
			t.Errorf("SetNarration(%+v) succeeded, want an error", bad)
		}
	}
}

func TestDetectTicket(t *testing.T) {
	if got := detectTicket("", "fix SYR-2Q0CH and SYR-AAAAA"); got != "SYR-2Q0CH" {
		t.Fatalf("got %q", got)
	}
	if got := detectTicket("no ticket here"); got != "" {
		t.Fatalf("got %q", got)
	}
}
