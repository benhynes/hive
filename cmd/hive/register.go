package main

import (
	"fmt"
	"os"

	"github.com/benhynes/hive/internal/client"
)

// runRegister registers the calling agent with the local hub and prints
// export lines on stdout so a shell can `eval "$(hive register ...)"`.
// With a pane bound (tmux pane id on Unix, win:<pid> on Windows) the
// agent is nudgeable + controllable; without one it is message-only.
func runRegister(args []string) error {
	fs := flags("register", args)
	name := fs.String("name", "", "agent name (unique per host per network)")
	pane := fs.String("pane", os.Getenv("TMUX_PANE"), "tmux pane id to bind (default $TMUX_PANE)")
	pid := fs.Int("pid", 0, "process id to bind for liveness (optional, outside tmux)")
	fs.Parse2()
	if *name == "" {
		*name = fs.pos(0)
	}
	if *name == "" {
		return fmt.Errorf("usage: hive register --name <name> [--pane %%ID]")
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	resp, err := c.Register(*name, *pane, *pid)
	if err != nil {
		return err
	}
	fmt.Printf("export HIVE_ADDR=%s\n", c.Addr)
	fmt.Printf("export HIVE_NET=%s\n", c.Net)
	fmt.Printf("export HIVE_AGENT=%s\n", resp.Agent)
	fmt.Printf("export HIVE_TOKEN=%s\n", resp.Token)
	how := "message-only (no pane bound)"
	if *pane != "" {
		how = "controllable + nudgeable via pane " + *pane
	}
	fmt.Fprintf(os.Stderr, "hive: registered %s — %s\n", resp.Agent, how)
	fmt.Fprintf(os.Stderr, "hive: apply with: eval \"$(hive register --name %s)\"\n", *name)
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
