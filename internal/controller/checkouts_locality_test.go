package controller

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
)

// Locality is preferred until the machine with the checkout is saturated: then work
// spills to the other machines, least loaded first (SYR-WCH0N).
func TestLocalityPreferredUntilSaturated(t *testing.T) {
	local := store.Worker{ID: "w-local", Name: "local", Status: "online", MaxConcurrent: 2, Agents: []string{"mock"}}
	remote := store.Worker{ID: "w-remote", Name: "remote", Status: "online", MaxConcurrent: 2, Agents: []string{"mock"}}
	far := store.Worker{ID: "w-far", Name: "far", Status: "online", MaxConcurrent: 2, Agents: []string{"mock"}}
	workers := []store.Worker{remote, far, local}
	locality := func(w store.Worker) (int, bool, bool) {
		if w.ID == "w-local" {
			return 1, true, true
		}
		return 0, false, true // the repo has a remote: others can clone it
	}
	counts := map[string]int{}
	var got []string
	for range 6 {
		w, ok := SelectWorkerWithLocality(workers, counts, WorkerSelector{}, "mock", "", locality)
		if !ok {
			got = append(got, "-")
			continue
		}
		counts[w.ID]++
		got = append(got, w.Name)
	}
	if got[0] != "local" || got[1] != "local" || counts["w-local"] != 2 || counts["w-far"] != 2 || counts["w-remote"] != 2 {
		t.Fatalf("placements %v (%v): want local twice, then the others", got, counts)
	}
}

// A machine with the checkout but no memory headroom for the work's expected peak is
// saturated too: the ticket goes to the machine that has room.
func TestTicketLocalityYieldsToHeadroom(t *testing.T) {
	th := newTestHub(t)
	ctx := context.Background()
	if err := th.st.CreateProject(ctx, store.Project{Name: "Mem"}); err != nil {
		t.Fatal(err)
	}
	projs, _ := th.st.ListProjects(ctx)
	proj := projs[len(projs)-1]
	if err := th.st.CreateRepo(ctx, store.Repo{ProjectID: proj.ID, Name: "api", RemoteURL: "https://github.com/acme/api.git", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	repos, _ := th.st.ListRepos(ctx, proj.ID)
	tk, err := th.st.CreateTicket(ctx, store.Ticket{ProjectID: proj.ID, Kind: store.TicketKindFeature, Title: "Big build",
		Priority: "P1", Status: store.TicketStatusReady, Hints: store.TicketHints{Agent: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = th.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{TicketID: tk.ID, RepoID: repos[0].ID, TargetBranch: "feat/x"})
	// This work peaked at 8 GB before.
	key := th.h.workKeyFor(ctx, store.Attempt{TicketID: tk.ID}, store.Task{ProjectID: proj.ID, RepoID: repos[0].ID})
	if err := th.st.UpsertWorkProfile(ctx, store.WorkProfile{WorkKey: key, MachineClass: "linux/amd64/16g", Runs: 3, PeakRSSMaxMB: 8000}); err != nil {
		t.Fatal(err)
	}

	localInfo := testWorkerInfo("w-local", "local-box")
	localInfo.Facts = &protocol.MachineFacts{RAMTotalMB: 16384, SurvivalReserveMB: 1000,
		Checkouts: []protocol.ReportedCheckout{{Path: "/work/api", RemoteURL: "github.com/acme/api"}}}
	fwLocal := th.connectWorker(t, localInfo, nil, "w-local")
	remoteInfo := testWorkerInfo("w-remote", "remote-box")
	remoteInfo.Facts = &protocol.MachineFacts{RAMTotalMB: 32768, SurvivalReserveMB: 1000}
	fwRemote := th.connectWorker(t, remoteInfo, nil, "w-remote")
	th.h.mu.Lock()
	th.h.workerMetrics["w-local"] = &protocol.WorkerMetrics{MemTotalMB: 16384, MemUsedMB: 11384} // 5 GB free: no room for 8
	th.h.workerMetrics["w-remote"] = &protocol.WorkerMetrics{MemTotalMB: 32768, MemUsedMB: 8000} // plenty
	th.h.mu.Unlock()

	if _, err := th.h.SetProjectAutopilot(ctx, proj.ID, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f := <-fwRemote.frames:
			if f.Type == protocol.MsgAgentChatOpen {
				if got := fwLocal.drain(protocol.MsgAgentChatOpen, 100*time.Millisecond); len(got) != 0 {
					t.Fatal("both machines got the ticket")
				}
				return
			}
		case f := <-fwLocal.frames:
			if f.Type == protocol.MsgAgentChatOpen {
				t.Fatal("the machine without headroom kept the ticket for its checkout")
			}
		case <-deadline:
			t.Fatal("the ticket was not placed")
		}
	}
}

func TestCheckoutsVisiblePerRepoAcrossMachines(t *testing.T) {
	cs := newChatServer(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for _, m := range []struct {
		id, name string
		cos      []protocol.ReportedCheckout
	}{
		{"w1", "alpha", []protocol.ReportedCheckout{{Path: "/src/api", RemoteURL: "git@github.com:acme/api.git", Branch: "main", Head: "abc", LastFetch: &now}}},
		{"w2", "beta", []protocol.ReportedCheckout{{Path: "/work/api", RemoteURL: "https://github.com/acme/api"}, {Path: "/work/web", RemoteURL: "https://github.com/acme/web"}}},
	} {
		upsertBox(t, cs.st, m.id, m.name)
		w, _ := cs.st.GetWorker(ctx, m.id)
		facts, _ := json.Marshal(protocol.MachineFacts{Checkouts: m.cos})
		w.FactsJSON = string(facts)
		if err := cs.st.UpsertWorker(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	code, raw := cs.api(t, "GET", "/api/checkouts?remote=https://github.com/acme/api.git", nil)
	var res struct {
		Repos []RepoCheckouts `json:"repos"`
	}
	if code != 200 || json.Unmarshal(raw, &res) != nil || len(res.Repos) != 1 || len(res.Repos[0].Checkouts) != 2 {
		t.Fatalf("one repo: %d %s", code, raw)
	}
	a := res.Repos[0].Checkouts[0]
	if a.WorkerName != "alpha" || a.Path != "/src/api" || a.Branch != "main" || a.LastFetch == nil {
		t.Fatalf("alpha's checkout: %+v", a)
	}
	if _, raw = cs.api(t, "GET", "/api/checkouts", nil); json.Unmarshal(raw, &res) != nil || len(res.Repos) != 2 {
		t.Fatalf("all repos: %s", raw)
	}

	// A project's repos list their checkouts.
	if err := cs.st.CreateRepo(ctx, store.Repo{ProjectID: store.DefaultProjectID, Name: "api", RemoteURL: "https://github.com/acme/api.git"}); err != nil {
		t.Fatal(err)
	}
	_, raw = cs.api(t, "GET", "/api/projects/"+store.DefaultProjectID+"/repos", nil)
	var repos struct {
		Repos []struct {
			Name      string            `json:"name"`
			Checkouts []MachineCheckout `json:"checkouts"`
		} `json:"repos"`
	}
	if json.Unmarshal(raw, &repos) != nil || len(repos.Repos) != 1 || repos.Repos[0].Name != "api" || len(repos.Repos[0].Checkouts) != 2 {
		t.Fatalf("project repos: %s", raw)
	}
}
