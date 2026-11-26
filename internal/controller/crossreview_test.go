package controller

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/store"
)

func TestParseReviewVerdict(t *testing.T) {
	for _, tc := range []struct {
		in      string
		approve bool
		notes   string
	}{
		{"APPROVE\nLooks good.", true, ""},
		{"**APPROVE**", true, ""},
		{"CHANGES: handle nil\n- and empty input", false, "handle nil\n- and empty input"},
		{"I think it's mostly fine", false, "I think it's mostly fine"},
	} {
		approve, notes := parseReviewVerdict(tc.in)
		if approve != tc.approve || notes != tc.notes {
			t.Errorf("parseReviewVerdict(%q) = %v %q, want %v %q", tc.in, approve, notes, tc.approve, tc.notes)
		}
	}
}

// With cross_review on, a passing run goes to a reviewer; changes requested go back to
// the author, and the second review approves, so the ticket ends in review.
func TestCrossReviewChangesThenApproveEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh verify commands")
	}
	cs := newChatServer(t)
	ctx := context.Background()
	workdirRoot := t.TempDir()
	repoOrigin := filepath.Join(t.TempDir(), "app-origin")
	initTestGitRepo(t, repoOrigin, "main")
	_, stop := startTicketTestDaemon(t, cs, workdirRoot)
	defer stop()

	on := true
	if err := cs.st.CreateProject(ctx, store.Project{Name: "Reviewed", Autopilot: true, Autonomy: store.AutonomyPolicy{CrossReview: &on}}); err != nil {
		t.Fatal(err)
	}
	projs, _ := cs.st.ListProjects(ctx)
	proj := projs[len(projs)-1]
	if err := cs.st.CreateRepo(ctx, store.Repo{ProjectID: proj.ID, Name: "app", RemoteURL: repoOrigin, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	repos, _ := cs.st.ListRepos(ctx, proj.ID)
	tk, err := cs.st.CreateTicket(ctx, store.Ticket{
		ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: "Reviewed change mock_review_changes",
		Status: store.TicketStatusReady, Hints: store.TicketHints{Agent: "mock"},
		Acceptance: []store.AcceptanceItem{{Text: "The reviewer's marker file exists"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repos[0].ID, BaseBranch: "main", TargetBranch: "feat/reviewed"}); err != nil {
		t.Fatal(err)
	}
	cs.h.SchedulePending(ctx)

	deadline := time.Now().Add(20 * time.Second)
	for {
		cur, _ := cs.st.GetTicket(ctx, tk.ID)
		if cur.Status == store.TicketStatusReview {
			break
		}
		if time.Now().After(deadline) {
			comments, _ := cs.st.ListTicketComments(ctx, tk.ID)
			t.Fatalf("ticket status %s, want review; comments: %+v", cur.Status, comments)
		}
		time.Sleep(50 * time.Millisecond)
	}
	comments, _ := cs.st.ListTicketComments(ctx, tk.ID)
	var verdicts []string
	for _, c := range comments {
		if strings.HasPrefix(c.Author, "review:") {
			verdicts = append(verdicts, c.Body)
		}
	}
	if len(verdicts) != 2 || !strings.Contains(verdicts[0], "requested changes") || !strings.Contains(verdicts[1], "approved") {
		t.Fatalf("review verdicts = %q, want changes then approval", verdicts)
	}
}
