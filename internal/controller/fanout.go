package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/store"
)

// FanOutRequest controls a fleet-wide parallel operation with deterministic daemon pre-checking
// so machines without relevant work are short-circuited at zero agent LLM token cost.
type FanOutRequest struct {
	Kind           string         `json:"kind"` // "worktree_cleanup" | "agent_sweep"
	WorkerSelector WorkerSelector `json:"worker_selector,omitzero"`
	Query          string         `json:"query,omitempty"`     // substring match (e.g. "epic-123")
	RootPath       string         `json:"root_path,omitempty"` // optional root to inspect per host
	Apply          bool           `json:"apply,omitzero"`      // for worktree_cleanup: actually delete removable matches
	Agent          string         `json:"agent,omitempty"`     // for agent_sweep
	Prompt         string         `json:"prompt,omitempty"`    // for agent_sweep
	Precheck       bool           `json:"precheck,omitzero"`   // if true for agent_sweep, skip machines with 0 deterministic matches
	PermissionMode string         `json:"permission_mode,omitempty"`
}

type FanOutMachineOutcome struct {
	WorkerID      string                       `json:"worker_id"`
	WorkerName    string                       `json:"worker_name"`
	Status        string                       `json:"status"` // "no_op" | "matched" | "cleaned" | "dispatched" | "error"
	Matches       []protocol.WorktreeScanMatch `json:"matches,omitempty"`
	CleanedCount  int                          `json:"cleaned_count,omitzero"`
	DispatchedIDs []string                     `json:"dispatched_task_ids,omitempty"`
	Error         string                       `json:"error,omitempty"`
}

type FanOutResult struct {
	Kind              string                 `json:"kind"`
	TotalMachines     int                    `json:"total_machines"`
	NoOpMachines      int                    `json:"no_op_machines"`
	ActedMachines     int                    `json:"acted_machines"`
	TotalMatches      int                    `json:"total_matches"`
	TotalCleaned      int                    `json:"total_cleaned"`
	DispatchedTaskIDs []string               `json:"dispatched_task_ids,omitempty"`
	Machines          []FanOutMachineOutcome `json:"machines"`
}

// FanOutFleet executes a parallel operation across all online machines matching selector.
// Machines with no work are short-circuited deterministically by konsertd without invoking any coding agent.
func (s *FleetService) FanOutFleet(ctx context.Context, req FanOutRequest) (FanOutResult, error) {
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if kind == "" {
		kind = "worktree_cleanup"
	}
	permMode := store.PermissionMode(req.PermissionMode)
	if permMode == "" {
		permMode = store.PermissionFullAccess
	}

	workers, err := s.store.ListWorkers(ctx)
	if err != nil {
		return FanOutResult{}, err
	}
	counts := s.hub.ActiveTaskCounts()
	var targets []store.Worker
	for _, w := range workers {
		if !s.hub.IsWorkerConnected(w.ID) {
			continue
		}
		agentReq := ""
		if kind == "agent_sweep" {
			agentReq = req.Agent
		}
		if workerMatchesSelector(w, counts, req.WorkerSelector, agentReq, permMode) {
			targets = append(targets, w)
		}
	}

	out := FanOutResult{
		Kind:              kind,
		TotalMachines:     len(targets),
		DispatchedTaskIDs: []string{},
		Machines:          make([]FanOutMachineOutcome, len(targets)),
	}
	if len(targets) == 0 {
		return out, nil
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, wk := range targets {
		wg.Add(1)
		go func(idx int, w store.Worker) {
			defer wg.Done()
			outcome := FanOutMachineOutcome{
				WorkerID:   w.ID,
				WorkerName: w.Name,
			}

			// Step 1: Deterministic daemon pre-scan (zero LLM tokens) when cleanup or precheck requested.
			if kind == "worktree_cleanup" || req.Precheck {
				rpcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				env, rErr := s.hub.RequestWorker(rpcCtx, w.ID, protocol.MsgWorktreeScan, protocol.WorktreeScanRequest{
					RootPath: req.RootPath,
					Query:    req.Query,
				})
				cancel()
				if rErr != nil {
					outcome.Status = "error"
					outcome.Error = rErr.Error()
					out.Machines[idx] = outcome
					return
				}
				scanRes, dErr := protocol.DecodePayload[protocol.WorktreeScanResult](env)
				if dErr != nil {
					outcome.Status = "error"
					outcome.Error = dErr.Error()
					out.Machines[idx] = outcome
					return
				}
				if scanRes.Error != "" {
					outcome.Status = "error"
					outcome.Error = scanRes.Error
					out.Machines[idx] = outcome
					return
				}
				outcome.Matches = scanRes.Matches
				if len(scanRes.Matches) == 0 {
					outcome.Status = "no_op"
					mu.Lock()
					out.NoOpMachines++
					mu.Unlock()
					out.Machines[idx] = outcome
					return
				}

				if kind == "worktree_cleanup" {
					cleaned := 0
					if req.Apply {
						for _, m := range scanRes.Matches {
							if !m.Removable {
								continue
							}
							cCtx, cCancel := context.WithTimeout(ctx, 12*time.Second)
							cEnv, cErr := s.hub.RequestWorker(cCtx, w.ID, protocol.MsgGitWorktreeCleanup, protocol.GitWorktreeCleanupRequest{
								Workdir: m.Path,
							})
							cCancel()
							if cErr == nil {
								if cRes, err := protocol.DecodePayload[protocol.GitWorktreeCleanupResult](cEnv); err == nil && cRes.OK {
									cleaned++
								}
							}
						}
						outcome.CleanedCount = cleaned
						outcome.Status = "cleaned"
					} else {
						outcome.Status = "matched"
					}
					mu.Lock()
					out.ActedMachines++
					out.TotalMatches += len(scanRes.Matches)
					out.TotalCleaned += cleaned
					mu.Unlock()
					out.Machines[idx] = outcome
					return
				}
			}

			// Step 2: Agent sweep (only reaches here if kind == "agent_sweep" and either !Precheck or matches > 0).
			if kind == "agent_sweep" {
				if strings.TrimSpace(req.Agent) == "" || strings.TrimSpace(req.Prompt) == "" {
					outcome.Status = "error"
					outcome.Error = "agent and prompt are required for agent_sweep"
					out.Machines[idx] = outcome
					return
				}
				repoPath := req.RootPath
				if repoPath == "" && len(outcome.Matches) > 0 {
					repoPath = outcome.Matches[0].Path
				}
				if repoPath == "" {
					repoPath = "~"
				}
				disp, dErr := s.DispatchTask(ctx, DispatchRequest{
					WorkerSelector: WorkerSelector{WorkerID: w.ID},
					Agent:          req.Agent,
					RepoPath:       repoPath,
					Prompt:         req.Prompt,
					Mode:           protocol.TaskModeOneshot,
					Origin:         protocol.TaskOriginOrchestrator,
					PermissionMode: string(permMode),
				})
				if dErr != nil {
					outcome.Status = "error"
					outcome.Error = fmt.Sprintf("dispatch failed: %v", dErr)
					out.Machines[idx] = outcome
					return
				}
				outcome.Status = "dispatched"
				outcome.DispatchedIDs = []string{disp.TaskID}
				mu.Lock()
				out.ActedMachines++
				out.TotalMatches += len(outcome.Matches)
				out.DispatchedTaskIDs = append(out.DispatchedTaskIDs, disp.TaskID)
				mu.Unlock()
				out.Machines[idx] = outcome
				return
			}

			outcome.Status = "no_op"
			mu.Lock()
			out.NoOpMachines++
			mu.Unlock()
			out.Machines[idx] = outcome
		}(i, wk)
	}
	wg.Wait()
	return out, nil
}
