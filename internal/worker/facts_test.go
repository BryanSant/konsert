package worker

import (
	"context"
	"testing"

	"github.com/BryanSant/konsert/internal/protocol"
)

func TestCollectMachineFactsAndClassification(t *testing.T) {
	facts := CollectMachineFacts(context.Background())
	if facts.OSName == "" {
		t.Fatal("expected non-empty OSName")
	}
	if facts.CPUCores <= 0 {
		t.Fatalf("expected CPUCores > 0, got %d", facts.CPUCores)
	}
	if facts.Virtualization == "" {
		t.Fatal("expected non-empty Virtualization")
	}
	if facts.CollectedAt.IsZero() {
		t.Fatal("expected CollectedAt timestamp")
	}

	// Verify classification precedence
	if got := classifyVirtualization(&protocol.MachineFacts{IsWSL: true, IsVM: true}); got != "wsl" {
		t.Fatalf("expected wsl, got %s", got)
	}
	if got := classifyVirtualization(&protocol.MachineFacts{IsMacContainer: true, IsContainer: true}); got != "mac_container" {
		t.Fatalf("expected mac_container, got %s", got)
	}
	if got := classifyVirtualization(&protocol.MachineFacts{IsContainer: true}); got != "container" {
		t.Fatalf("expected container, got %s", got)
	}
	if got := classifyVirtualization(&protocol.MachineFacts{IsVM: true}); got != "vm" {
		t.Fatalf("expected vm, got %s", got)
	}
	if got := classifyVirtualization(&protocol.MachineFacts{}); got != "bare_metal" {
		t.Fatalf("expected bare_metal, got %s", got)
	}

	// Verify version extractor
	if v := extractVersion("postgres (PostgreSQL) 18.1"); v != "18.1" {
		t.Fatalf("expected 18.1, got %q", v)
	}
	if v := extractVersion("go version go1.27.2 darwin/arm64"); v != "1.27.2" {
		t.Fatalf("expected 1.27.2, got %q", v)
	}
}
