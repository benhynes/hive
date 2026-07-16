package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhynes/hive/internal/config"
)

func TestSyncHarnessGuidancePreservesAndUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(path, []byte("# Existing\n\nKeep me.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncHarnessGuidance(path); err != nil {
		t.Fatal(err)
	}
	if err := syncHarnessGuidance(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.Contains(text, "Keep me.") || strings.Count(text, harnessGuidanceStart) != 1 ||
		!strings.Contains(text, "list_mcp_resources") {
		t.Fatalf("guidance = %s", text)
	}
}

func TestHarnessSyncStampChangesWithForcefield(t *testing.T) {
	a := harnessSyncStamp("/usr/local/bin/hive", nil)
	b := harnessSyncStamp("/usr/local/bin/hive", &config.ForcefieldMCP{URL: "https://forcefield.test"})
	if a == b {
		t.Fatal("stamp did not change with Forcefield configuration")
	}
}
