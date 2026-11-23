package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanSant/konsert/internal/config"
)

func TestPrepareRejectsInvalidConfigBeforeGeneratingTokens(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultControllerConfig()
	cfg.DBPath = filepath.Join(dir, "konsert.db")
	cfg.Orchestrator.Provider = "antropic"
	var out bytes.Buffer
	err := prepareServeConfig(&cfg, &out, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "antropic") {
		t.Fatalf("expected provider validation error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, tokensFileName)); !os.IsNotExist(statErr) {
		t.Fatal("no tokens should be generated for an invalid config")
	}
}
