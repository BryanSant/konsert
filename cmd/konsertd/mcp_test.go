package main

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/controller"
	"github.com/BryanSant/konsert/internal/store/sqlite"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveMCPBridge(t *testing.T) {
	cfg := config.WorkerConfig{ControllerURL: "wss://hub.example/ws/worker", FleetToken: "fleet"}

	base, tok := resolveMCPBridge(cfg, nil, nil, envMap(nil))
	if base != "https://hub.example" || tok != "" {
		t.Fatalf("defaults: base=%q tok=%q (fleet token must never be used)", base, tok)
	}
	base, tok = resolveMCPBridge(cfg, nil, nil, envMap(map[string]string{"KONSERT_URL": "http://other:1", "KONSERT_TOKEN": "adm"}))
	if base != "http://other:1" || tok != "adm" {
		t.Fatalf("env: base=%q tok=%q", base, tok)
	}
	u, f := "http://flag:2/", "flagtok"
	base, tok = resolveMCPBridge(cfg, &u, &f, envMap(map[string]string{"KONSERT_URL": "http://other:1", "KONSERT_TOKEN": "adm"}))
	if base != "http://flag:2" || tok != "flagtok" {
		t.Fatalf("flags: base=%q tok=%q", base, tok)
	}
}

// konsertd mcp against an auth-enabled hub whose fleet token and recovery key differ.
func TestMCPBridgeAgainstAuthEnabledHub(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "konsert.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hubCfg := config.DefaultControllerConfig()
	hubCfg.FleetToken = "fleet-tok"
	hubCfg.RecoveryKey = "admin-tok"
	ts := httptest.NewServer(controller.NewServer(hubCfg, st, nil, "test"))
	defer ts.Close()

	workerCfg := config.WorkerConfig{ControllerURL: "ws" + ts.URL[len("http"):] + "/ws/worker", FleetToken: "fleet-tok"}

	base, tok := resolveMCPBridge(workerCfg, nil, nil, envMap(map[string]string{"KONSERT_TOKEN": "admin-tok"}))
	if _, err := controller.NewRemoteFleetClient(base, tok).FleetStatus(ctx); err != nil {
		t.Fatalf("FleetStatus with the credential from KONSERT_TOKEN failed: %v", err)
	}

	// The old default (the fleet token) is rejected by /api, which is why it is no longer used.
	if _, err := controller.NewRemoteFleetClient(base, workerCfg.FleetToken).FleetStatus(ctx); err == nil {
		t.Fatal("expected the fleet token to be rejected by /api")
	}
	// Without any admin token the bridge does not silently fall back to the fleet token.
	if _, tok := resolveMCPBridge(workerCfg, nil, nil, envMap(nil)); tok != "" {
		t.Fatalf("expected no token, got %q", tok)
	}
}
