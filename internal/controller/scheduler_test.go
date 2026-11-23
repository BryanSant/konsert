package controller

import (
	"testing"
	"time"

	"github.com/BryanSant/konsert/internal/store"
)

func TestSelectWorkerCapabilitiesAndIsolation(t *testing.T) {
	now := time.Now().UTC()
	workers := []store.Worker{
		{
			ID:             "w-sandbox",
			Name:           "mac-sandbox",
			HostGroup:      "mac-studio",
			OS:             "darwin",
			PermissionMode: store.PermissionSandboxed,
			Agents:         []string{"claude", "agy"},
			Tags:           map[string]string{"env": "host"},
			MaxConcurrent:  4,
			Status:         "online",
			LastSeenAt:     now,
		},
		{
			ID:             "w-podman-1",
			Name:           "podman-1",
			HostGroup:      "fedora-main",
			OS:             "linux",
			PermissionMode: store.PermissionFullAccess,
			Agents:         []string{"claude", "grok"},
			Tags:           map[string]string{"env": "podman", "gpu": "rtx4090"},
			MaxConcurrent:  4,
			Status:         "online",
			LastSeenAt:     now.Add(-5 * time.Second),
		},
		{
			ID:             "w-podman-2",
			Name:           "podman-2",
			HostGroup:      "fedora-main",
			OS:             "linux",
			PermissionMode: store.PermissionFullAccess,
			Agents:         []string{"claude", "grok"},
			Tags:           map[string]string{"env": "podman"},
			MaxConcurrent:  2,
			Status:         "online",
			LastSeenAt:     now,
		},
		{
			ID:             "w-offline",
			Name:           "offline-node",
			HostGroup:      "fedora-main",
			OS:             "linux",
			PermissionMode: store.PermissionFullAccess,
			Agents:         []string{"claude"},
			MaxConcurrent:  4,
			Status:         "offline",
			LastSeenAt:     now,
		},
	}

	// Sandboxed worker must NOT be chosen for full_access tasks.
	if _, ok := SelectWorker(workers, nil, WorkerSelector{WorkerID: "w-sandbox"}, "claude", store.PermissionFullAccess); ok {
		t.Fatal("sandboxed worker must not accept full_access task")
	}

	// Sandboxed worker CAN accept sandboxed task.
	w, ok := SelectWorker(workers, nil, WorkerSelector{WorkerID: "w-sandbox"}, "agy", store.PermissionSandboxed)
	if !ok || w.ID != "w-sandbox" {
		t.Fatalf("expected w-sandbox for sandboxed task, got %+v ok=%v", w, ok)
	}

	// Full-access worker can also downgrade to run a sandboxed task.
	w, ok = SelectWorker(workers, nil, WorkerSelector{HostGroup: "fedora-main", Tags: map[string]string{"gpu": "rtx4090"}}, "claude", store.PermissionSandboxed)
	if !ok || w.ID != "w-podman-1" {
		t.Fatalf("expected w-podman-1, got %+v ok=%v", w, ok)
	}

	// Utilization ranking: w-podman-1 has 1/4 = 0.25, w-podman-2 has 1/2 = 0.50 -> pick w-podman-1.
	counts := map[string]int{"w-podman-1": 1, "w-podman-2": 1}
	w, ok = SelectWorker(workers, counts, WorkerSelector{OS: "linux"}, "claude", store.PermissionFullAccess)
	if !ok || w.ID != "w-podman-1" {
		t.Fatalf("expected lower utilization w-podman-1, got %+v ok=%v", w, ok)
	}

	// Capacity full -> returns false (task should queue).
	countsFull := map[string]int{"w-podman-1": 4, "w-podman-2": 2}
	if _, ok := SelectWorker(workers, countsFull, WorkerSelector{OS: "linux"}, "claude", store.PermissionFullAccess); ok {
		t.Fatal("expected no worker when all matching workers are at max_concurrent")
	}
}

func TestSelectWorkerFactAwareRouting(t *testing.T) {
	now := time.Now().UTC()
	workers := []store.Worker{
		{
			ID:             "w-mac",
			Name:           "mac-studio",
			HostGroup:      "mac-studio",
			OS:             "darwin",
			PermissionMode: store.PermissionFullAccess,
			Agents:         []string{"claude", "agy"},
			MaxConcurrent:  4,
			Status:         "online",
			LastSeenAt:     now,
			FactsJSON:      `{"os_name":"macOS","cpu_cores":16,"ram_total_mb":65536,"disk_free_gb":400,"virtualization":"bare_metal","tools":{"go":"1.27.2","node":"26.11.1"},"auth_ready":{"gh":true}}`,
		},
		{
			ID:             "w-pg",
			Name:           "bocabox",
			HostGroup:      "homelab",
			OS:             "linux",
			PermissionMode: store.PermissionFullAccess,
			Agents:         []string{"claude", "agy"},
			MaxConcurrent:  4,
			Status:         "online",
			LastSeenAt:     now,
			FactsJSON:      `{"os_name":"Fedora Linux","cpu_cores":32,"ram_total_mb":131072,"disk_free_gb":900,"tmp_is_tmpfs":true,"virtualization":"bare_metal","tools":{"postgres":"18.1","podman":"5.4.0"},"auth_ready":{"podman":true}}`,
		},
		{
			ID:             "w-gcp",
			Name:           "foo1",
			HostGroup:      "cloud-ops",
			OS:             "linux",
			PermissionMode: store.PermissionFullAccess,
			Agents:         []string{"claude", "agy"},
			MaxConcurrent:  4,
			Status:         "online",
			LastSeenAt:     now,
			FactsJSON:      `{"os_name":"Ubuntu","cpu_cores":8,"ram_total_mb":16384,"disk_free_gb":120,"virtualization":"vm","tools":{"gcloud":"512.0.0","kubectl":"1.32.0"},"auth_ready":{"gcloud":true,"kubectl":true}}`,
		},
	}

	// 1. macOS-only job ("macos" alias -> darwin) routes to mac-studio
	w, ok := SelectWorker(workers, nil, WorkerSelector{OS: "macos"}, "agy", store.PermissionFullAccess)
	if !ok || w.ID != "w-mac" {
		t.Fatalf("expected w-mac for macOS job, got %+v ok=%v", w, ok)
	}

	// 2. Schema migration needing Postgres 18 routes to bocabox
	w, ok = SelectWorker(workers, nil, WorkerSelector{RequiredTools: map[string]string{"postgres": "18"}}, "claude", store.PermissionFullAccess)
	if !ok || w.ID != "w-pg" {
		t.Fatalf("expected w-pg for Postgres 18 job, got %+v ok=%v", w, ok)
	}

	// 3. Named host / gcloud auth_ready routes to foo1
	w, ok = SelectWorker(workers, nil, WorkerSelector{Name: "foo1", RequiredAuth: []string{"gcloud"}}, "claude", store.PermissionFullAccess)
	if !ok || w.ID != "w-gcp" {
		t.Fatalf("expected w-gcp (foo1) for gcloud deploy, got %+v ok=%v", w, ok)
	}

	// 4. Impossible Postgres 19 requirement returns false
	if _, ok := SelectWorker(workers, nil, WorkerSelector{RequiredTools: map[string]string{"postgres": "19"}}, "claude", store.PermissionFullAccess); ok {
		t.Fatal("expected no worker matching postgres 19")
	}
}
