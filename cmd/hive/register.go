package main

import (
	"fmt"
	"os"

	"github.com/benhynes/hive/internal/client"
)

// runRegister registers the calling agent with the local hub and prints
// export lines on stdout so a shell can `eval "$(hive register ...)"`.
// With an explicitly bound tmux pane on Unix the agent is controllable.
// Client-supplied pane bindings are intentionally unsupported on Windows;
// --pid there provides liveness only, while hub-spawned consoles are the
// controllable path. Automatic terminal wake notices require --nudge.
func runRegister(args []string) error {
	fs := flags("register", args)
	name := fs.String("name", "", "agent name (unique per host per network)")
	pane := fs.String("pane", "", "tmux pane id to bind explicitly (requires CONTROL; e.g. $TMUX_PANE)")
	nudge := fs.Bool("nudge", false, "opt into automatic terminal wake + Enter (requires --pane; controlled idle panes only)")
	pid := fs.Int("pid", 0, "process id to bind for liveness (optional, outside tmux)")
	fs.Parse2()
	if *name == "" {
		*name = fs.pos(0)
	}
	if *name == "" {
		return fmt.Errorf("usage: hive register --name <name> [--pane %%ID [--nudge]] [--pid PID]")
	}
	if *nudge && *pane == "" {
		return fmt.Errorf("--nudge requires an explicit --pane (and CONTROL credential)")
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	resp, err := c.RegisterWithNudge(*name, *pane, *pid, *nudge)
	if err != nil {
		return err
	}
	fmt.Printf("export HIVE_ADDR=%s\n", c.Addr)
	fmt.Printf("export HIVE_NET=%s\n", c.Net)
	fmt.Printf("export HIVE_AGENT=%s\n", resp.Agent)
	fmt.Printf("export HIVE_TOKEN=%s\n", resp.Token)
	how := "message-only (no pane bound)"
	if *pane != "" {
		how = "controllable via pane " + *pane + "; automatic wake disabled"
	}
	if *nudge {
		how = "controllable + automatic terminal wake enabled via pane " + *pane
	}
	fmt.Fprintf(os.Stderr, "hive: registered %s — %s\n", resp.Agent, how)
	if *nudge {
		fmt.Fprintln(os.Stderr, "hive: warning: --nudge may submit text typed after the idle check; use only for controlled idle panes")
	}
	return nil
}

func runDeregister(args []string) error {
	fs := flags("deregister", args)
	name := fs.String("name", "", "agent name (default: yourself)")
	fs.Parse2()
	if *name == "" {
		*name = fs.pos(0)
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	if err := c.Deregister(*name); err != nil {
		return err
	}
	who := *name
	if who == "" {
		who = c.Agent
	}
	fmt.Printf("deregistered %s\n", who)
	return nil
}
