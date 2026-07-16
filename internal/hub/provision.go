package hub

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/benhynes/hive/internal/config"
)

// provisionSpec is a fully-resolved provisioning payload: profile context
// files already read into memory and MCP servers listed, so it can be applied
// locally or carried inside a forwarded spawn request to an SSH host's hub
// (which has no access to the origin's profile files).
type provisionSpec struct {
	Context map[string]string           `json:"context,omitempty"` // basename -> file content
	MCP     map[string]config.MCPServer `json:"mcp,omitempty"`
	NoHive  bool                        `json:"no_hive_mcp,omitempty"`
	Sandbox *config.SandboxRunner       `json:"sandbox,omitempty"`
	Runtime *runtimeProvision           `json:"runtime,omitempty"`
}

type runtimeProvision struct {
	Type        string `json:"type"`
	Auth        []byte `json:"auth,omitempty"`
	State       []byte `json:"state,omitempty"`
	Workspace   string `json:"workspace"`
	HiveCommand string `json:"hive_command"`
}

// buildProvision resolves a profile into a self-contained spec, reading the
// listed context files. A listed-but-missing file is an error — it was named
// on purpose.
func buildProvision(prof config.SpawnProfile) (provisionSpec, error) {
	spec := provisionSpec{NoHive: prof.NoHiveMCP, MCP: prof.MCP, Sandbox: prof.Sandbox}
	if len(prof.Context) > 0 {
		spec.Context = map[string]string{}
		for _, src := range prof.Context {
			b, err := os.ReadFile(src)
			if err != nil {
				return spec, fmt.Errorf("context file %s: %w", src, err)
			}
			spec.Context[filepath.Base(src)] = string(b)
		}
	}
	if prof.RuntimeSetup != nil {
		runtime, err := buildRuntimeProvision(*prof.RuntimeSetup, prof.Sandbox != nil)
		if err != nil {
			return spec, err
		}
		spec.Runtime = runtime
	}
	return spec, nil
}

// applyProvision prepares a local working directory so a freshly spawned
// agent boots already wired into the mesh. It runs on the hub that owns the
// agent's pane, before the runtime starts:
//
//   - seeds the spec's context files into cwd;
//   - writes a project-scoped .mcp.json listing the spec's MCP servers, with
//     the `hive` server auto-included (unless opted out) so the agent can
//     hive_send/hive_recv with zero manual setup;
//   - pre-approves that config in ~/.claude.json — BOTH the workspace-trust
//     gate (hasTrustDialogAccepted) and the MCP-server gate
//     (enabledMcpjsonServers) — because a headless Claude Code agent would
//     otherwise hang on a trust prompt it cannot answer.
//
// hiveBin is the command written for the auto-registered hive server (the
// daemon's own absolute path, so the agent needn't have `hive` on PATH).
func applyProvision(cwd string, spec provisionSpec, hiveBin string) error {
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return fmt.Errorf("cwd %s: %w", cwd, err)
	}

	for base, content := range spec.Context {
		dst := filepath.Join(cwd, filepath.Base(base)) // re-Base: never escape cwd
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			return fmt.Errorf("seed %s: %w", dst, err)
		}
	}

	servers := map[string]config.MCPServer{}
	for name, s := range spec.MCP {
		servers[name] = s
	}
	if !spec.NoHive {
		// The agent's env already carries HIVE_*, so `hive mcp` authenticates
		// with no extra config.
		servers["hive"] = config.MCPServer{Command: hiveBin, Args: []string{"mcp"}}
	}
	if len(servers) == 0 {
		if spec.Runtime == nil {
			return nil
		}
		return applyRuntimeProvision(cwd, spec.Runtime, servers)
	}

	if err := writeMCPJSON(cwd, servers); err != nil {
		return err
	}
	if spec.Runtime != nil {
		return applyRuntimeProvision(cwd, spec.Runtime, servers)
	}
	return preseedClaudeApproval(cwd, servers)
}

func buildRuntimeProvision(setup config.RuntimeSetup, sandboxed bool) (*runtimeProvision, error) {
	if setup.Type != "codex" && setup.Type != "claude" {
		return nil, fmt.Errorf("runtime_setup.type must be codex or claude")
	}
	if setup.AuthSource == "" {
		return nil, fmt.Errorf("runtime_setup.auth_source is required")
	}
	auth, err := os.ReadFile(expandHome(setup.AuthSource))
	if err != nil {
		return nil, fmt.Errorf("runtime auth source: %w", err)
	}
	var state []byte
	if setup.StateSource != "" {
		state, err = os.ReadFile(expandHome(setup.StateSource))
		if err != nil {
			return nil, fmt.Errorf("runtime state source: %w", err)
		}
	}
	workspace := setup.Workspace
	if workspace == "" && sandboxed {
		workspace = "/workspace"
	}
	hiveCommand := setup.HiveCommand
	if hiveCommand == "" && sandboxed {
		hiveCommand = "/usr/local/bin/hive"
	}
	return &runtimeProvision{
		Type: setup.Type, Auth: auth, State: state, Workspace: workspace, HiveCommand: hiveCommand,
	}, nil
}

func applyRuntimeProvision(cwd string, runtime *runtimeProvision, servers map[string]config.MCPServer) error {
	switch runtime.Type {
	case "codex":
		return provisionCodex(cwd, runtime, servers)
	case "claude":
		return provisionClaude(cwd, runtime, servers)
	default:
		return fmt.Errorf("unsupported runtime setup %q", runtime.Type)
	}
}

func provisionCodex(cwd string, runtime *runtimeProvision, servers map[string]config.MCPServer) error {
	directory := filepath.Join(cwd, ".codex")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "auth.json"), runtime.Auth, 0o600); err != nil {
		return err
	}
	var configText strings.Builder
	configText.WriteString("cli_auth_credentials_store = \"file\"\n")
	if runtime.Workspace != "" {
		fmt.Fprintf(&configText, "\n[projects.%q]\ntrust_level = \"trusted\"\n", runtime.Workspace)
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		server := servers[name]
		command := server.Command
		args := append([]string(nil), server.Args...)
		if name == "hive" && runtime.HiveCommand != "" {
			command = runtime.HiveCommand
			args = []string{"mcp", "--log-file", "/workspace/.hive-mcp.log"}
		}
		fmt.Fprintf(&configText, "\n[mcp_servers.%s]\ncommand = %q\n", name, command)
		if name == "hive" {
			configText.WriteString("env_vars = [\"HIVE_ADDR\", \"HIVE_NET\", \"HIVE_AGENT\", \"HIVE_TOKEN\", \"HIVE_CONTROL_TOKEN\", \"HIVE_CONTROL_HOST\"]\n")
			configText.WriteString("startup_timeout_sec = 15.0\n")
		}
		if len(args) > 0 {
			encoded, _ := json.Marshal(args)
			fmt.Fprintf(&configText, "args = %s\n", encoded)
		}
	}
	return os.WriteFile(filepath.Join(directory, "config.toml"), []byte(configText.String()), 0o600)
}

func provisionClaude(cwd string, runtime *runtimeProvision, servers map[string]config.MCPServer) error {
	directory := filepath.Join(cwd, ".claude")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, ".credentials.json"), runtime.Auth, 0o600); err != nil {
		return err
	}
	root := map[string]any{}
	if len(runtime.State) > 0 {
		if err := json.Unmarshal(runtime.State, &root); err != nil {
			return fmt.Errorf("runtime state source is not valid JSON: %w", err)
		}
	}
	root["hasCompletedOnboarding"] = true
	projects, _ := root["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		root["projects"] = projects
	}
	workspace := runtime.Workspace
	if workspace == "" {
		workspace = cwd
	}
	entry, _ := projects[workspace].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
		projects[workspace] = entry
	}
	entry["hasTrustDialogAccepted"] = true
	entry["enabledMcpjsonServers"] = mergeEnabled(entry["enabledMcpjsonServers"], servers)
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	for _, path := range []string{filepath.Join(cwd, ".claude.json"), filepath.Join(directory, ".claude.json")} {
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// provisionAgent resolves a profile and applies it to cwd in one step (the
// local-spawn path; forwarded spawns split build and apply across hubs).
func provisionAgent(cwd string, prof config.SpawnProfile, hiveBin string) error {
	spec, err := buildProvision(prof)
	if err != nil {
		return err
	}
	return applyProvision(cwd, spec, hiveBin)
}

// writeMCPJSON writes <cwd>/.mcp.json in the shape Claude Code reads.
func writeMCPJSON(cwd string, servers map[string]config.MCPServer) error {
	doc := map[string]any{"mcpServers": servers}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cwd, ".mcp.json"), append(b, '\n'), 0o644)
}

// preseedClaudeApproval clears both Claude Code gates for cwd in the user's
// ~/.claude.json: workspace trust and per-project .mcp.json server approval.
// It read-modify-writes the whole file (preserving every other field and
// project) and renames atomically, so a partial write can't corrupt it.
func preseedClaudeApproval(cwd string, servers map[string]config.MCPServer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".claude.json")

	root := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &root); err != nil {
			return fmt.Errorf("%s is not valid JSON (refusing to overwrite): %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	projects, _ := root["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		root["projects"] = projects
	}
	entry, _ := projects[abs].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
		projects[abs] = entry
	}

	entry["hasTrustDialogAccepted"] = true
	entry["enabledMcpjsonServers"] = mergeEnabled(entry["enabledMcpjsonServers"], servers)

	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".hive-tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// mergeEnabled unions the existing enabledMcpjsonServers with the server
// names we just registered, so we never drop a server the user (or a prior
// provision) already approved.
func mergeEnabled(existing any, servers map[string]config.MCPServer) []string {
	set := map[string]bool{}
	if arr, ok := existing.([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				set[s] = true
			}
		}
	}
	for name := range servers {
		set[name] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out) // deterministic write
	return out
}

// hiveBinPath is the command written for the auto-registered hive MCP server:
// the daemon's own binary by absolute path, so a spawned agent can reach it
// even if `hive` is not on its PATH. Falls back to the bare name.
func hiveBinPath() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "hive"
}

// expandHome resolves a leading ~ to the user's home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}
