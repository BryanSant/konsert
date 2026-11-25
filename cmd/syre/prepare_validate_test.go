package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/config"
)

func TestPrepareRejectsInvalidConfigBeforeGeneratingTokens(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultControllerConfig()
	cfg.DBPath = filepath.Join(dir, "syre.db")
	cfg.Commander.Provider = "antropic"
	err := prepareServeConfig(&cfg, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "antropic") {
		t.Fatalf("expected provider validation error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, tokensFileName)); !os.IsNotExist(statErr) {
		t.Fatal("no tokens should be generated for an invalid config")
	}
}
