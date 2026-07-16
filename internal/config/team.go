package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Team is a repeatable group of agents launched from spawn profiles.
type Team struct {
	Name    string       `yaml:"name,omitempty"`
	Host    string       `yaml:"host,omitempty"`
	Members []TeamMember `yaml:"members"`
}

type TeamMember struct {
	Name         string `yaml:"name"`
	Profile      string `yaml:"profile"`
	Cwd          string `yaml:"cwd,omitempty"`
	GrantControl bool   `yaml:"grant_control,omitempty"`
	Nudge        bool   `yaml:"nudge,omitempty"`
	Persist      bool   `yaml:"persist,omitempty"`
}

func TeamsDir() string { return filepath.Join(Home(), "teams") }

// LoadTeam loads either an explicit path or ~/.hive/teams/<name>.yaml.
func LoadTeam(nameOrPath string) (Team, string, error) {
	var team Team
	path := nameOrPath
	if filepath.Ext(path) == "" && filepath.Base(path) == path {
		path = filepath.Join(TeamsDir(), nameOrPath+".yaml")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return team, path, err
	}
	if err := yaml.Unmarshal(b, &team); err != nil {
		return team, path, fmt.Errorf("%s: %w", path, err)
	}
	if team.Name == "" {
		team.Name = nameOrPath
	}
	if len(team.Members) == 0 {
		return team, path, fmt.Errorf("%s: team has no members", path)
	}
	seen := map[string]bool{}
	for i, member := range team.Members {
		if !profileNameRe.MatchString(member.Name) {
			return team, path, fmt.Errorf("%s: member %d has invalid name %q", path, i+1, member.Name)
		}
		if !profileNameRe.MatchString(member.Profile) {
			return team, path, fmt.Errorf("%s: member %q has invalid profile %q", path, member.Name, member.Profile)
		}
		if seen[member.Name] {
			return team, path, fmt.Errorf("%s: duplicate member %q", path, member.Name)
		}
		seen[member.Name] = true
	}
	return team, path, nil
}
