package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/benhynes/hive/internal/config"
)

type harnessAdapter struct {
	name         string
	binary       string
	guidancePath func() string
	sync         func(binary, hiveBin, logDir string, forcefield *config.ForcefieldMCP) error
	status       func(binary string) error
}

func harnessAdapters() []harnessAdapter {
	return []harnessAdapter{
		{
			name: "codex", binary: "codex",
			guidancePath: func() string { return filepath.Join(userHome(), ".codex", "AGENTS.md") },
			sync:         syncCodexHarness, status: statusCodexHarness,
		},
		{
			name: "claude", binary: "claude",
			guidancePath: func() string { return filepath.Join(userHome(), ".claude", "CLAUDE.md") },
			sync:         syncClaudeHarness, status: statusClaudeHarness,
		},
	}
}

const (
	harnessGuidanceStart = "<!-- hive-managed-tooling:start -->"
	harnessGuidanceEnd   = "<!-- hive-managed-tooling:end -->"
)

const harnessGuidance = harnessGuidanceStart + `
## Local agent tooling

Hive and Forcefield are installed as global MCP tool servers.

- For agents, teams, messaging, or coordination, discover and use the Hive
  tools such as ` + "`hive_agents`" + `, ` + "`hive_send`" + `, and ` + "`hive_recv`" + `.
- Before external API, Git, Linear, or other brokered access, discover and use
  Forcefield's ` + "`capabilities`" + ` tool to learn the available services and
  constraints.
- These are tool-only MCP servers. An empty ` + "`list_mcp_resources`" + ` result
  does not mean Hive or Forcefield is unavailable.
- Do not guess or probe HTTP ports to detect them. If their tools cannot be
  discovered, report the integration failure and recommend ` + "`hive harness status`" + `.
` + harnessGuidanceEnd + `
`

func runHarness(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: hive harness sync|status")
	}
	switch args[0] {
	case "sync":
		return syncHarnesses(true, true)
	case "status":
		return statusHarnesses()
	default:
		return fmt.Errorf("usage: hive harness sync|status")
	}
}

func syncHarnesses(verbose, force bool) error {
	hiveBin, err := exec.LookPath("hive")
	if err != nil {
		hiveBin, err = os.Executable()
		if err != nil {
			return err
		}
	}
	hiveBin, _ = filepath.Abs(hiveBin)
	logDir := filepath.Join(config.Home(), "mcp")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return err
	}
	forcefield, err := config.DiscoverForcefieldMCP()
	if err != nil {
		return err
	}
	stamp := harnessSyncStamp(hiveBin, forcefield)
	stampPath := filepath.Join(config.Home(), "harness-sync.stamp")
	if !force {
		if b, err := os.ReadFile(stampPath); err == nil && strings.TrimSpace(string(b)) == stamp {
			return nil
		}
	}
	found := 0
	for _, adapter := range harnessAdapters() {
		binary, err := exec.LookPath(adapter.binary)
		if err != nil {
			if verbose {
				fmt.Printf("%s: not installed\n", adapter.name)
			}
			continue
		}
		found++
		if err := adapter.sync(binary, hiveBin, logDir, forcefield); err != nil {
			return fmt.Errorf("%s integration: %w", adapter.name, err)
		}
		if err := syncHarnessGuidance(adapter.guidancePath()); err != nil {
			return fmt.Errorf("%s guidance: %w", adapter.name, err)
		}
		if verbose {
			services := "hive"
			if forcefield != nil {
				services += " + forcefield"
			}
			fmt.Printf("%s: synced %s globally\n", adapter.name, services)
		}
	}
	if found == 0 {
		return fmt.Errorf("no supported harnesses found")
	}
	return os.WriteFile(stampPath, []byte(stamp+"\n"), 0600)
}

func harnessSyncStamp(hiveBin string, forcefield *config.ForcefieldMCP) string {
	h := sha256.New()
	fmt.Fprintf(h, "v1\n%s\n%s\n", hiveBin, harnessGuidance)
	if forcefield != nil {
		fmt.Fprintf(h, "%+v\n", *forcefield)
	}
	for _, adapter := range harnessAdapters() {
		if binary, err := exec.LookPath(adapter.binary); err == nil {
			fmt.Fprintf(h, "%s=%s\n", adapter.name, binary)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func syncHarnessGuidance(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(existing)
	if start := strings.Index(text, harnessGuidanceStart); start >= 0 {
		if endRel := strings.Index(text[start:], harnessGuidanceEnd); endRel >= 0 {
			end := start + endRel + len(harnessGuidanceEnd)
			text = text[:start] + strings.TrimSpace(harnessGuidance) + text[end:]
		} else {
			return fmt.Errorf("%s contains an unterminated Hive-managed block", path)
		}
	} else {
		if strings.TrimSpace(text) != "" {
			text = strings.TrimRight(text, "\n") + "\n\n"
		}
		text += strings.TrimSpace(harnessGuidance)
	}
	return os.WriteFile(path, []byte(strings.TrimSpace(text)+"\n"), 0600)
}

func userHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}

func statusHarnesses() error {
	for _, adapter := range harnessAdapters() {
		binary, err := exec.LookPath(adapter.binary)
		if err != nil {
			fmt.Printf("%s: not installed\n", adapter.name)
			continue
		}
		fmt.Printf("%s:\n", adapter.name)
		if err := adapter.status(binary); err != nil {
			return fmt.Errorf("%s status: %w", adapter.name, err)
		}
	}
	return nil
}

func syncCodexHarness(binary, hiveBin, logDir string, forcefield *config.ForcefieldMCP) error {
	replaceMCP(binary, []string{"mcp", "remove", "hive"})
	if err := runQuiet(binary, "mcp", "add", "hive", "--", hiveBin, "mcp", "--log-file", filepath.Join(logDir, "codex-hive.log")); err != nil {
		return err
	}
	if forcefield == nil {
		return nil
	}
	replaceMCP(binary, []string{"mcp", "remove", "forcefield"})
	args := append([]string{"mcp", "add", "forcefield", "--"}, forcefieldArgs(forcefield)...)
	return runQuiet(binary, args...)
}

func syncClaudeHarness(binary, hiveBin, logDir string, forcefield *config.ForcefieldMCP) error {
	replaceMCP(binary, []string{"mcp", "remove", "hive", "-s", "user"})
	if err := runQuiet(binary, "mcp", "add", "-s", "user", "hive", "--", hiveBin, "mcp", "--log-file", filepath.Join(logDir, "claude-hive.log")); err != nil {
		return err
	}
	if forcefield == nil {
		return nil
	}
	replaceMCP(binary, []string{"mcp", "remove", "forcefield", "-s", "user"})
	args := append([]string{"mcp", "add", "-s", "user", "forcefield", "--"}, forcefieldArgs(forcefield)...)
	return runQuiet(binary, args...)
}

func forcefieldArgs(ff *config.ForcefieldMCP) []string {
	args := []string{ff.Command, "mcp", "--url", ff.URL, "--token-file", ff.TokenFile}
	if ff.CACert != "" {
		args = append(args, "--ca-cert", ff.CACert)
	}
	if ff.ClientCert != "" {
		args = append(args, "--client-cert", ff.ClientCert)
	}
	if ff.ClientKey != "" {
		args = append(args, "--client-key", ff.ClientKey)
	}
	return args
}

func replaceMCP(binary string, args []string) {
	_ = exec.Command(binary, args...).Run()
}

func runQuiet(binary string, args ...string) error {
	out, err := exec.Command(binary, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", filepath.Base(binary), strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func statusCodexHarness(binary string) error {
	cmd := exec.Command(binary, "mcp", "list")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func statusClaudeHarness(binary string) error {
	cmd := exec.Command(binary, "mcp", "list")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
