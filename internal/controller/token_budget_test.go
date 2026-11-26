package controller

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/BryanSant/syre/internal/tokenbudget"
)

func TestControllerTokenBudgets(t *testing.T) {
	cases := []struct {
		name   string
		budget int
		render func(t *testing.T) string
	}{
		{
			name:   "largest_multi_repo_compiled_packet_with_contracts",
			budget: tokenbudget.Packet,
			render: func(t *testing.T) string {
				th := newTestHub(t)
				ctx := context.Background()

				if err := th.st.CreateProject(ctx, store.Project{
					Name:      "billing-platform",
					VerifyCmd: "make test",
				}); err != nil {
					t.Fatal(err)
				}
				projs, _ := th.st.ListProjects(ctx)
				proj := projs[len(projs)-1]

				repoDefs := []struct {
					name   string
					verify string
				}{
					{"api", "go test ./pkg/auth/..."},
					{"web", "npm --prefix web test"},
					{"sdk", "go test ./sdk/..."},
					{"worker", "go test ./worker/..."},
				}
				var repos []store.Repo
				for _, rd := range repoDefs {
					if err := th.st.CreateRepo(ctx, store.Repo{
						ProjectID: proj.ID,
						Name:      rd.name,
						RemoteURL: "https://github.com/acme/" + rd.name + ".git",
						VerifyCmd: rd.verify,
					}); err != nil {
						t.Fatal(err)
					}
				}
				repos, _ = th.st.ListRepos(ctx, proj.ID)

				// Create 5 done blocker tickets with rich interface digests to populate contracts.
				var blockerIDs []string
				for i := 1; i <= 5; i++ {
					bt, err := th.st.CreateTicket(ctx, store.Ticket{
						ProjectID: proj.ID,
						Title:     fmt.Sprintf("Prerequisite contract %d", i),
						Status:    store.TicketStatusDone,
					})
					if err != nil {
						t.Fatal(err)
					}
					digRaw, _ := json.Marshal(protocol.RunDigest{
						TicketSID: bt.SID,
						Status:    protocol.DigestStatusReview,
						Interface: fmt.Sprintf("POST /v2/auth/step%d returns {access_token, refresh_token, family_id, expires_in}; api/pkg/auth/step%d.go", i, i),
					}, protocol.JSONOptions)
					task := store.Task{
						ID:         fmt.Sprintf("task-contract-%d", i),
						SID:        fmt.Sprintf("ts-c%04d", i),
						ProjectID:  proj.ID,
						TicketID:   bt.ID,
						Status:     protocol.TaskStatusCompleted,
						DigestJSON: string(digRaw),
					}
					if err := th.st.CreateTask(ctx, task); err != nil {
						t.Fatal(err)
					}
					blockerIDs = append(blockerIDs, bt.ID)
				}

				tk, err := th.st.CreateTicket(ctx, store.Ticket{
					ProjectID: proj.ID,
					Kind:      store.TicketKindFeature,
					Title:     "Rotate refresh tokens across all services",
					Body:      "Rotate refresh tokens every 15 minutes across api, web, sdk, and background worker; revoke entire token family on reuse detection and retry once on 401.",
					Acceptance: []store.AcceptanceItem{
						{Text: "Rotate on /v2/auth/refresh and invalidate the entire token family on reuse."},
						{Text: "Web fetch wrapper retries once on 401 after refreshing credentials."},
						{Text: "SDK client surfaces structured AuthExpiredError after retry exhaustion."},
						{Text: "Worker background refresh avoids stampede with jittered singleflight."},
					},
					Status: store.TicketStatusReady,
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, bid := range blockerIDs {
					if err := th.st.AddTicketEdge(ctx, tk.ID, bid); err != nil {
						t.Fatal(err)
					}
				}
				for _, r := range repos {
					if err := th.st.PutTicketRepoBinding(ctx, store.TicketRepoBinding{
						TicketID:     tk.ID,
						RepoID:       r.ID,
						BaseBranch:   "main",
						TargetBranch: "feat/jwt-refresh-family-rotation",
					}); err != nil {
						t.Fatal(err)
					}
				}

				pkt, err := th.h.CompileDispatchPacket(ctx, tk.ID, PacketOptions{WorkdirRoot: "/var/lib/syred/work"})
				if err != nil {
					t.Fatal(err)
				}
				return pkt.Text()
			},
		},
		{
			name:   "full_commander_tool_schema",
			budget: tokenbudget.ToolSchemas,
			render: func(t *testing.T) string {
				cs := newChatServer(t)
				w := cs.connectAgentWorker(t, "w-budget", "box", claudeAccount("claude", true))
				b := cs.dialBrowser(t)
				_, open, _ := openChat(t, cs, w, b, map[string]any{"fleet_tools": true})
				tok := strings.TrimPrefix(open.MCPServers[0].Headers["Authorization"], "Bearer ")
				sess, err := connectChatMCP(t, w.mcpURL(open.MCPServers[0]), tok)
				if err != nil {
					t.Fatalf("connect commander MCP: %v", err)
				}
				defer sess.Close()
				listed, err := sess.ListTools(context.Background(), nil)
				if err != nil {
					t.Fatalf("ListTools: %v", err)
				}
				raw, err := json.Marshal(listed.Tools, protocol.JSONOptions)
				if err != nil {
					t.Fatalf("marshal tools: %v", err)
				}
				return string(raw)
			},
		},
		{
			name:   "fleet_status_20_machines_with_directives",
			budget: tokenbudget.FleetStatus20,
			render: func(t *testing.T) string {
				th := newTestHub(t)
				ctx := context.Background()
				now := time.Now().UTC()

				for i := 1; i <= 20; i++ {
					osName, arch, ramMB := "linux", "amd64", uint64(64*1024)
					tags := map[string]string{}
					switch {
					case i <= 5:
						tags["gpu"] = "true"
					case i <= 12:
						osName, arch, ramMB = "darwin", "arm64", uint64(32*1024)
					case i <= 17:
						osName, arch, ramMB = "windows", "amd64", uint64(16*1024)
					default:
						osName, arch, ramMB = "linux", "arm64", uint64(128*1024)
					}
					factsRaw, _ := json.Marshal(protocol.MachineFacts{
						CPUCores:   16,
						RAMTotalMB: ramMB,
						DiskFreeGB: 250,
					}, protocol.JSONOptions)
					if err := th.st.UpsertWorker(ctx, store.Worker{
						ID:            fmt.Sprintf("w-%02d", i),
						SID:           fmt.Sprintf("m-%02d", i),
						Name:          fmt.Sprintf("node-%02d", i),
						OS:            osName,
						Arch:          arch,
						Agents:        []string{"claude", "codex", "gemini"},
						Tags:          tags,
						MaxConcurrent: 4,
						Status:        "offline",
						Drained:       i >= 19,
						FactsJSON:     string(factsRaw),
						LastSeenAt:    now,
					}); err != nil {
						t.Fatal(err)
					}
				}
				until := now.Add(3 * time.Hour)
				maxOne := 1
				directives := []DirectiveInput{
					{Kind: store.DirectiveProviderDisable, Target: "codex", Until: &until, Reason: "quota", Params: store.DirectiveParams{Fallback: "claude"}},
					{Kind: store.DirectiveMachineThrottle, Target: "node-06", Until: &until, Reason: "quiet hours", Params: store.DirectiveParams{MaxConcurrency: &maxOne}},
					{Kind: store.DirectiveMachinePause, Target: "node-13", Until: &until, Reason: "gaming"},
				}
				for _, d := range directives {
					if _, err := th.h.CreateDirective(ctx, d); err != nil {
						t.Fatal(err)
					}
				}

				res, err := th.svc.FleetStatusView(ctx, FleetStatusInput{})
				if err != nil {
					t.Fatalf("FleetStatusView: %v", err)
				}
				raw, err := json.Marshal(res, protocol.JSONOptions)
				if err != nil {
					t.Fatal(err)
				}
				return string(raw)
			},
		},
		{
			name:   "busiest_hub_pulse",
			budget: tokenbudget.Pulse,
			render: func(t *testing.T) string {
				th := newTestHub(t)
				ctx := context.Background()
				now := time.Now().UTC()

				for i := 1; i <= 10; i++ {
					if err := th.st.UpsertWorker(ctx, store.Worker{
						ID:            fmt.Sprintf("w-p%02d", i),
						SID:           fmt.Sprintf("m-p%02d", i),
						Name:          fmt.Sprintf("worker-%02d", i),
						Status:        "online",
						Drained:       i <= 4,
						MaxConcurrent: 2,
						LastSeenAt:    now,
					}); err != nil {
						t.Fatal(err)
					}
				}
				var events []store.FleetEvent
				for i := 1; i <= 8; i++ {
					meta, _ := json.Marshal(pulseTriggerMeta{
						Kind:          protocol.PulseTriggerReview,
						TicketSID:     fmt.Sprintf("SYR-P%04d", i),
						UnblockedSIDs: []string{fmt.Sprintf("SYR-U%04d", i), fmt.Sprintf("SYR-V%04d", i)},
						Followups:     2,
					}, protocol.JSONOptions)
					events = append(events, store.FleetEvent{Kind: store.EventKindPulse, MetaJSON: string(meta)})
				}
				for _, m := range []pulseTriggerMeta{
					{Kind: protocol.PulseTriggerFailed, TicketSID: "SYR-F9999", Category: protocol.ErrKindVerify},
					{Kind: protocol.PulseTriggerNeedsYou, TicketSID: "SYR-N9999", Category: "question"},
				} {
					meta, _ := json.Marshal(m, protocol.JSONOptions)
					events = append(events, store.FleetEvent{Kind: store.EventKindPulse, MetaJSON: string(meta)})
				}
				pulse := th.h.buildBoardPulse(ctx, events)
				return pulse.Text()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.render(t)
			if strings.TrimSpace(got) == "" {
				t.Fatalf("%s rendered empty output", tc.name)
			}
			tokenbudget.Assert(t, tc.name, got, tc.budget)
		})
	}
}
