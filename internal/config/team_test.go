package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadTeam(t *testing.T) {
	t.Setenv("HIVE_HOME", t.TempDir())
	if err := os.MkdirAll(TeamsDir(), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(TeamsDir(), "zlt.yaml")
	if err := os.WriteFile(path, []byte(`
name: zlt
host: debian-dev
members:
  - name: zlt-lead
    profile: zlt-lead
  - name: zlt-codex
    profile: zlt-codex
    persist: true
`), 0600); err != nil {
		t.Fatal(err)
	}
	team, gotPath, err := LoadTeam("zlt")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != path || team.Host != "debian-dev" || len(team.Members) != 2 || !team.Members[1].Persist {
		t.Fatalf("team=%+v path=%q", team, gotPath)
	}
}

func TestLoadTeamRejectsDuplicateMembers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	os.WriteFile(path, []byte(`
members:
  - {name: worker, profile: codex}
  - {name: worker, profile: claude}
`), 0600)
	_, _, err := LoadTeam(path)
	if err == nil || !strings.Contains(err.Error(), "duplicate member") {
		t.Fatalf("err=%v", err)
	}
}
