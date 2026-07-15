package mcp

import (
	"testing"

	"github.com/benhynes/hive/internal/client"
)

func TestAddSelfToAgentsPreservesDirectory(t *testing.T) {
	res := client.AgentsResp{
		Self:   "authenticated@local",
		Agents: []client.AgentInfo{{Agent: "peer@remote", Alive: true}},
		Unreachable: map[string]string{
			"down": "connection refused",
		},
	}
	got := addSelfToAgents("me@local", res)

	if got.Self != "authenticated@local" {
		t.Fatalf("self = %q, want token-authenticated hub identity", got.Self)
	}
	if len(got.Agents) != 1 || got.Agents[0].Agent != "peer@remote" || !got.Agents[0].Alive {
		t.Fatalf("agents were not preserved: %+v", got.Agents)
	}
	if got.Unreachable["down"] != "connection refused" {
		t.Fatalf("unreachable hosts were not preserved: %+v", got.Unreachable)
	}
}

func TestAddSelfToAgentsFallsBackForOlderDaemon(t *testing.T) {
	got := addSelfToAgents("me@local", client.AgentsResp{})
	if got.Self != "me@local" {
		t.Fatalf("fallback self = %q, want me@local", got.Self)
	}
}
