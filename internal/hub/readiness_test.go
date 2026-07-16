package hub

import "testing"

func TestClassifyKnownRuntimePromptText(t *testing.T) {
	tests := map[string]string{
		"✨ Update available!":                                     "runtime_update_prompt",
		"Do you trust the contents of this directory?":            "workspace_trust_prompt",
		"Choose the text style that looks best":                   "runtime_theme_prompt",
		"Select login method:":                                    "runtime_login_prompt",
		"New MCP server found in this project: hive":              "mcp_approval_prompt",
		"WARNING: Claude Code running in Bypass Permissions mode": "permission_bypass_prompt",
	}
	for screen, want := range tests {
		if got := classifyRuntimePrompt(screen); got != want {
			t.Fatalf("screen %q: got %q, want %q", screen, got, want)
		}
	}
}
