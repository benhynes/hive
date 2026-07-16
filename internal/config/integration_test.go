package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverForcefieldMCPFromSpawnProfiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HIVE_HOME", filepath.Join(home, ".hive"))
	if err := os.MkdirAll(ProfilesDir(), 0700); err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(home, "runner.yaml")
	if err := os.WriteFile(runner, []byte(`
profiles:
  codex:
    forcefield_url: https://forcefield.test:7902
    token_file: ~/.config/forcefield/token
    ca_cert: ~/.config/forcefield/ca.crt
`), 0600); err != nil {
		t.Fatal(err)
	}
	profile := `{"sandbox":{"command":"/usr/local/bin/ff","profiles":"` + runner + `","profile":"codex"}}`
	if err := os.WriteFile(filepath.Join(ProfilesDir(), "codex.json"), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverForcefieldMCP()
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.URL != "https://forcefield.test:7902" ||
		got.TokenFile != filepath.Join(home, ".config/forcefield/token") {
		t.Fatalf("integration = %+v", got)
	}
}
