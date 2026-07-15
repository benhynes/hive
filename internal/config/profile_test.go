package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HIVE_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "profiles"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := `{
  "runtime": ["claude", "--dangerously-skip-permissions"],
  "cwd": "~/work",
  "context": ["docs/AGENT-GUIDE.md"],
  "mcp": {"playwright": {"command": "npx", "args": ["-y", "@playwright/mcp"]}}
}`
	if err := os.WriteFile(filepath.Join(home, "profiles", "dev.json"), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := LoadProfile("dev")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if len(p.Runtime) != 2 || p.Runtime[0] != "claude" {
		t.Errorf("runtime = %v", p.Runtime)
	}
	if p.Cwd != "~/work" || len(p.Context) != 1 {
		t.Errorf("cwd/context = %q / %v", p.Cwd, p.Context)
	}
	if s, ok := p.MCP["playwright"]; !ok || s.Command != "npx" {
		t.Errorf("mcp = %+v", p.MCP)
	}

	// A missing profile is a plain not-found error.
	if _, err := LoadProfile("absent"); !os.IsNotExist(err) {
		t.Errorf("missing profile: got %v, want IsNotExist", err)
	}

	// Names that could escape the profiles dir are rejected before any I/O.
	for _, bad := range []string{"../evil", "a/b", "UPPER", ""} {
		if _, err := LoadProfile(bad); err == nil {
			t.Errorf("LoadProfile(%q) accepted a bad name", bad)
		}
	}
}
