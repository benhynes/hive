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

func TestTeamNudgeDefaultsOn(t *testing.T) {
	team := Team{}
	if !team.NudgeFor(TeamMember{}) {
		t.Fatal("managed team member should default to nudge=true")
	}
}

func TestTeamNudgeOverrides(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name   string
		team   Team
		member TeamMember
		want   bool
	}{
		{name: "team disables", team: Team{Defaults: TeamDefaults{Nudge: &no}}, want: false},
		{name: "team enables", team: Team{Defaults: TeamDefaults{Nudge: &yes}}, want: true},
		{name: "member disables", team: Team{Defaults: TeamDefaults{Nudge: &yes}}, member: TeamMember{Nudge: &no}, want: false},
		{name: "member enables", team: Team{Defaults: TeamDefaults{Nudge: &no}}, member: TeamMember{Nudge: &yes}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.team.NudgeFor(tt.member); got != tt.want {
				t.Fatalf("NudgeFor()=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadTeamPreservesExplicitNudgeFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nudge.yaml")
	if err := os.WriteFile(path, []byte(`
defaults:
  nudge: true
members:
  - name: awake
    profile: codex
  - name: quiet
    profile: codex
    nudge: false
`), 0600); err != nil {
		t.Fatal(err)
	}
	team, _, err := LoadTeam(path)
	if err != nil {
		t.Fatal(err)
	}
	if !team.NudgeFor(team.Members[0]) {
		t.Fatal("defaulted member should be nudgeable")
	}
	if team.Members[1].Nudge == nil || team.NudgeFor(team.Members[1]) {
		t.Fatal("explicit nudge=false was not preserved")
	}
}
