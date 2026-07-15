package client

import (
	"fmt"
	"os"

	"github.com/benhynes/hive/internal/config"
)

// ResolveAgent builds the least-privileged client suitable for an agent
// runtime. Unlike Resolve, it never inherits CONTROL from net.json and never
// falls back to a legacy CONTROL token when the network has no MSG token.
// CONTROL is retained only when HIVE_CONTROL_TOKEN was explicitly supplied.
func ResolveAgent(netFlag string) (*Client, error) {
	explicitToken := os.Getenv("HIVE_TOKEN") != ""
	explicitControl := os.Getenv("HIVE_CONTROL_TOKEN") != ""
	explicitAgent := os.Getenv("HIVE_AGENT") != ""
	if explicitAgent != explicitToken {
		return nil, fmt.Errorf("HIVE_AGENT and HIVE_TOKEN must be set together for an existing agent identity")
	}

	c, err := Resolve(netFlag)
	if err != nil {
		return nil, err
	}
	if !explicitToken {
		nc, err := config.LoadNet(c.Net)
		if err != nil {
			return nil, fmt.Errorf("resolve agent message credential: %w", err)
		}
		if nc.MsgToken == "" {
			return nil, fmt.Errorf("network %q has no MSG token; refusing to use CONTROL for an agent", c.Net)
		}
		c.Token = nc.MsgToken
	}
	if !explicitControl {
		c.Control = ""
		c.ControlHost = ""
	}
	return c, nil
}

// ResolveBootstrap returns a network credential suitable for minting a new
// agent identity. It deliberately ignores an enclosing HIVE_AGENT/HIVE_TOKEN
// pair: launchers such as `hive run` create a child identity rather than act as
// the parent agent. An explicit token without an agent is treated as a network
// bootstrap credential; otherwise a configured MSG token is preferred. An
// explicitly held CONTROL token is an acceptable parent-to-child bootstrap
// only when no local network state exists. The returned client never retains
// CONTROL as an ambient capability.
func ResolveBootstrap(netFlag string) (*Client, error) {
	explicitToken := os.Getenv("HIVE_TOKEN") != ""
	explicitAgent := os.Getenv("HIVE_AGENT") != ""
	if explicitAgent && !explicitToken {
		return nil, fmt.Errorf("HIVE_AGENT requires HIVE_TOKEN for an enclosing agent identity")
	}
	c, err := Resolve(netFlag)
	if err != nil {
		return nil, err
	}
	// A token without an agent is explicitly a network bootstrap credential.
	// Preserve it even if unrelated local state for the same network name is
	// present (common in containers and when HIVE_ADDR targets another hub).
	if explicitToken && !explicitAgent {
		// Resolve already selected the explicit token.
	} else if nc, err := config.LoadNet(c.Net); err == nil && nc.MsgToken != "" {
		c.Token = nc.MsgToken
	} else if explicitAgent {
		// An agent token cannot call /register. A trusted controller can still
		// launch a child from a remote/minimal environment by using its explicit
		// control capability once as the registration credential.
		if os.Getenv("HIVE_CONTROL_TOKEN") == "" || c.Control == "" {
			return nil, fmt.Errorf("cannot create a child identity from agent %q: no network MSG or explicit CONTROL credential", c.Agent)
		}
		tok, err := c.localControlToken()
		if err != nil {
			return nil, fmt.Errorf("validate bootstrap CONTROL credential: %w", err)
		}
		c.Token = tok
	} else {
		return nil, fmt.Errorf("network %q has no MSG token; refusing to use CONTROL to bootstrap an agent", c.Net)
	}
	if c.Token == "" {
		return nil, fmt.Errorf("no network credential available to register a child agent")
	}
	c.Agent = ""
	c.Control = ""
	c.ControlHost = ""
	return c, nil
}
