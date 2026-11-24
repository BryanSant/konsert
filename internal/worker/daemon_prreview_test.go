package worker

import (
	"context"
	"os/exec"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
)

// git.pr.review runs gh in the checkout on this machine (an injected runner: never the
// real gh) and answers with the outcome.
func TestDaemonReviewsThePullRequestWithGH(t *testing.T) {
	repo := initGitRepo(t)
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "git@github.com:acme/wc.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}
	var mu sync.Mutex
	var calls [][]string
	var dirs []string
	hub := newFakeHub(t)
	startTestDaemonWith(t, hub, nil, func(d *Daemon) {
		d.ghRun = func(_ context.Context, dir string, args ...string) ([]byte, error) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, args)
			dirs = append(dirs, dir)
			if args[1] == "view" {
				return []byte(`{"number":5,"url":"https://github.com/acme/wc/pull/5","state":"OPEN"}`), nil
			}
			return nil, nil
		}
	})
	c := hub.accept(t)
	c.waitFor(t, 10*time.Second, "worker.hello", isType(protocol.MsgWorkerHello))
	c.send(t, protocol.MsgGitPRReview, "rv-1", protocol.GitPRReviewRequest{Workdir: repo, Branch: "konsert/t1", Action: protocol.PRReviewApprove})
	env := c.waitFor(t, 10*time.Second, "git.pr.review.result", func(e protocol.Envelope) bool {
		return e.Type == protocol.MsgGitPRReviewResult && e.ID == "rv-1"
	})
	res, err := protocol.DecodePayload[protocol.GitPRReviewResult](env)
	if err != nil || res.Status != protocol.PRReviewSent || res.PRNumber != 5 {
		t.Fatalf("result %+v, %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || !slices.Equal(calls[0], []string{"pr", "view", "konsert/t1", "--json", "number,url,state"}) ||
		!slices.Equal(calls[1], []string{"pr", "review", "5", "--approve"}) {
		t.Fatalf("gh calls %q", calls)
	}
	if dirs[0] == "" {
		t.Fatal("gh ran without the checkout as its directory")
	}
}
