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
}

// buildProvision resolves a profile into a self-contained spec, reading the
// listed context files. A listed-but-missing file is an error — it was named
// on purpose.
func buildProvision(prof config.SpawnProfile) (provisionSpec, error) {
	spec := provisionSpec{NoHive: prof.NoHiveMCP, MCP: prof.MCP}
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
		return nil // nothing to register; no .mcp.json, no trust needed
	}

	if err := writeMCPJSON(cwd, servers); err != nil {
		return err
	}
	return preseedClaudeApproval(cwd, servers)
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
