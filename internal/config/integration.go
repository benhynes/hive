package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

type ForcefieldMCP struct {
	Command    string
	URL        string
	TokenFile  string
	CACert     string
	ClientCert string
	ClientKey  string
}

type runnerIntegrationFile struct {
	Profiles map[string]runnerIntegrationProfile `yaml:"profiles"`
}

type runnerIntegrationProfile struct {
	ForcefieldURL string `yaml:"forcefield_url"`
	TokenFile     string `yaml:"token_file"`
	CACert        string `yaml:"ca_cert"`
	ClientCert    string `yaml:"client_cert"`
	ClientKey     string `yaml:"client_key"`
}

// DiscoverForcefieldMCP derives the host-side Forcefield MCP connection from
// installed Hive spawn profiles, avoiding a second copy of the same endpoint
// and credential configuration.
func DiscoverForcefieldMCP() (*ForcefieldMCP, error) {
	entries, err := os.ReadDir(ProfilesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			names = append(names, entry.Name()[:len(entry.Name())-len(".json")])
		}
	}
	sort.Strings(names)
	var found *ForcefieldMCP
	for _, name := range names {
		profile, err := LoadProfile(name)
		if err != nil || profile.Sandbox == nil {
			continue
		}
		b, err := os.ReadFile(expandUserPath(profile.Sandbox.Profiles))
		if err != nil {
			return nil, fmt.Errorf("profile %s runner configuration: %w", name, err)
		}
		var runner runnerIntegrationFile
		if err := yaml.Unmarshal(b, &runner); err != nil {
			return nil, fmt.Errorf("profile %s runner configuration: %w", name, err)
		}
		rp, ok := runner.Profiles[profile.Sandbox.Profile]
		if !ok || rp.ForcefieldURL == "" || rp.TokenFile == "" {
			continue
		}
		current := &ForcefieldMCP{
			Command: profile.Sandbox.Command, URL: rp.ForcefieldURL,
			TokenFile: expandUserPath(rp.TokenFile), CACert: expandUserPath(rp.CACert),
			ClientCert: expandUserPath(rp.ClientCert), ClientKey: expandUserPath(rp.ClientKey),
		}
		if found != nil && *found != *current {
			return nil, fmt.Errorf("spawn profiles contain multiple Forcefield client configurations")
		}
		found = current
	}
	return found, nil
}

func expandUserPath(path string) string {
	if path == "" || path == "~" || len(path) < 2 || path[:2] != "~/" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[2:])
}
