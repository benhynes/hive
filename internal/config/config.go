// Package config handles ~/.hive/config.json and per-network state
// directories (~/.hive/nets/<name>/).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Config is the per-host daemon configuration.
type Config struct {
	HostName string `json:"host_name"`
	Bind     string `json:"bind"`
	Port     int    `json:"port"`
}

// NetConfig is one network's local state: its join tokens and the hosts
// this hub knows how to reach. The hosts list is local-only by design —
// there is no cross-hub sync.
type NetConfig struct {
	Name         string `json:"name"`
	MsgToken     string `json:"msg_token"`
	ControlToken string `json:"control_token,omitempty"`
	// ControlHost binds ControlToken to one hub. Empty preserves the
	// original network-wide token behavior for existing configurations.
	ControlHost string            `json:"control_host,omitempty"`
	Hosts       map[string]string `json:"hosts"` // host name -> "addr:port"
	// SSHHosts are lightweight on-demand hosts reached over SSH tunnels
	// (docs/ssh-hosts-design.md). Local to this hub, like Hosts — no sync.
	SSHHosts map[string]SSHHost `json:"ssh_hosts,omitempty"`
}

// SSHHost is one registered SSH target: an on-demand host brought up with a
// transient loopback daemon at first spawn, no permanent install.
type SSHHost struct {
	Target   string `json:"target"`             // user@host or an ssh_config alias
	Port     int    `json:"port,omitempty"`     // remote daemon loopback port (default 7777)
	Home     string `json:"home,omitempty"`     // remote HIVE_HOME (default ~/.hive)
	Bin      string `json:"bin,omitempty"`      // local path to a target-platform hive binary (default: self-copy, platforms must match)
	Identity string `json:"identity,omitempty"` // ssh key path
	Profile  string `json:"profile,omitempty"`  // default spawn profile
}

// ControlFor returns this configuration's control token only when it is
// valid for host. A blank ControlHost means the legacy network-wide scope.
func (n NetConfig) ControlFor(host string) string {
	if n.ControlHost != "" && n.ControlHost != host {
		return ""
	}
	return n.ControlToken
}

// Home returns the hive state directory ($HIVE_HOME or ~/.hive).
func Home() string {
	if h := os.Getenv("HIVE_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".hive"
	}
	return filepath.Join(home, ".hive")
}

func NetDir(net string) string { return filepath.Join(Home(), "nets", net) }

func netPath(net string) string { return filepath.Join(NetDir(net), "net.json") }

// ProfilesDir is where spawn profiles live (~/.hive/profiles/<name>.json).
func ProfilesDir() string { return filepath.Join(Home(), "profiles") }

// MCPServer is one entry in a spawned agent's .mcp.json.
type MCPServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// SpawnProfile is a named provisioning spec applied when an agent is spawned:
// what to run, where, and which context + MCP servers it boots with. It is
// orthogonal to host type — the same profile applies to a local, tailnet, or
// (later) SSH spawn.
type SpawnProfile struct {
	Runtime []string             `json:"runtime,omitempty"` // agent command; a `-- CMD` on spawn overrides it
	Cwd     string               `json:"cwd,omitempty"`     // working dir on the target; --cwd overrides
	Repo    string               `json:"repo,omitempty"`    // clone into cwd if cwd is empty (P2+; ignored for now)
	Context []string             `json:"context,omitempty"` // files (abs or relative to the hub's cwd) seeded into cwd
	MCP     map[string]MCPServer `json:"mcp,omitempty"`     // extra MCP servers registered for the agent
	// NoHiveMCP opts out of auto-registering the `hive` MCP server. By default
	// every provisioned agent gets it, so it is mesh-aware with zero setup.
	NoHiveMCP bool `json:"no_hive_mcp,omitempty"`
	// Sandbox makes containment an operator-selected property of spawning.
	// Hive owns identity and lifecycle; the named Forcefield runner owns the
	// isolation and external capability boundary.
	Sandbox *SandboxRunner `json:"sandbox,omitempty"`
	// RuntimeSetup provisions runtime-specific auth, trust, and Hive MCP
	// configuration into the selected cwd before a sandbox starts.
	RuntimeSetup *RuntimeSetup `json:"runtime_setup,omitempty"`
}

type RuntimeSetup struct {
	Type            string `json:"type"`                        // codex or claude
	AuthSource      string `json:"auth_source"`                 // trusted host credential file copied 0600
	StateSource     string `json:"state_source,omitempty"`      // Claude state file copied and pre-approved
	Workspace       string `json:"workspace,omitempty"`         // runtime-visible cwd; defaults to /workspace when sandboxed
	HiveCommand     string `json:"hive_command,omitempty"`      // runtime-visible hive binary; defaults to /usr/local/bin/hive in a sandbox
	CheckForUpdates *bool  `json:"check_for_updates,omitempty"` // Codex startup update check; nil preserves its default
}

type SandboxRunner struct {
	Command  string `json:"command"`  // absolute path to the trusted ff binary
	Profiles string `json:"profiles"` // absolute Forcefield runner profile file
	Profile  string `json:"profile"`  // operator-selected runner profile name
}

var profileNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// LoadProfile reads ~/.hive/profiles/<name>.json. The name is validated to
// keep it from escaping the profiles directory.
func LoadProfile(name string) (SpawnProfile, error) {
	var p SpawnProfile
	if !profileNameRe.MatchString(name) {
		return p, fmt.Errorf("bad profile name %q (want [a-z0-9][a-z0-9_-]{0,31})", name)
	}
	b, err := os.ReadFile(filepath.Join(ProfilesDir(), name+".json"))
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("%s.json: %w", name, err)
	}
	return p, nil
}

// Load reads config.json, filling defaults for anything unset.
func Load() (Config, error) {
	c := Config{Bind: "127.0.0.1", Port: 7777}
	b, err := os.ReadFile(filepath.Join(Home(), "config.json"))
	if err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("config.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return c, err
	}
	if c.HostName == "" {
		hn, err := os.Hostname()
		if err != nil {
			return c, err
		}
		c.HostName, _, _ = strings.Cut(hn, ".")
		c.HostName = Sanitize(c.HostName)
	}
	if c.Bind == "" {
		c.Bind = "127.0.0.1"
	}
	if c.Port == 0 {
		c.Port = 7777
	}
	return c, nil
}

// Sanitize lowercases a hostname-ish string into a legal hive name.
func Sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		out = "host"
	}
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

// Save writes config.json.
func Save(c Config) error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return writeFile0600(filepath.Join(Home(), "config.json"), b)
}

// LoadNet reads one network's net.json. os.IsNotExist(err) means this
// host is not a member of the network.
func LoadNet(net string) (NetConfig, error) {
	var n NetConfig
	b, err := os.ReadFile(netPath(net))
	if err != nil {
		return n, err
	}
	if err := json.Unmarshal(b, &n); err != nil {
		return n, fmt.Errorf("%s: %w", netPath(net), err)
	}
	if n.Hosts == nil {
		n.Hosts = map[string]string{}
	}
	n.Name = net
	return n, nil
}

// SaveNet writes one network's net.json (tokens inside → 0600).
func SaveNet(n NetConfig) error {
	if err := os.MkdirAll(NetDir(n.Name), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(n, "", "  ")
	return writeFile0600(netPath(n.Name), b)
}

// ListNets returns the names of all locally configured networks.
func ListNets() ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(Home(), "nets"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			if _, err := os.Stat(netPath(e.Name())); err == nil {
				out = append(out, e.Name())
			}
		}
	}
	return out, nil
}

func writeFile0600(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
