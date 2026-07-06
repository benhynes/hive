// Package config handles ~/.hive/config.json and per-network state
// directories (~/.hive/nets/<name>/).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	Name         string            `json:"name"`
	MsgToken     string            `json:"msg_token"`
	ControlToken string            `json:"control_token,omitempty"`
	Hosts        map[string]string `json:"hosts"` // host name -> "addr:port"
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
	return os.Rename(tmp, path)
}
