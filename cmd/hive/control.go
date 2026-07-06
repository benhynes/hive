package main

import (
	"fmt"
	"strings"

	"github.com/benhynes/hive/internal/client"
)

func runSpawn(args []string) error {
	fs := flags("spawn", args)
	host := fs.String("host", "", "target host (default: this host)")
	cwd := fs.String("cwd", "", "working directory for the spawned process")
	grant := fs.Bool("grant-control", false, "inject the network control token (CONTROL layer)")
	waitReady := fs.Bool("wait", false, "wait until the pane draws and goes quiet")
	fs.Parse2()
	name := fs.pos(0)
	cmd := fs.afterDD
	if name == "" || len(cmd) == 0 {
		return fmt.Errorf("usage: hive spawn [--host H] [--cwd D] [--grant-control] [--wait] <name> -- CMD...")
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	res, err := c.Spawn(*host, name, cmd, *cwd, *grant, *waitReady)
	if err != nil {
		return err
	}
	fmt.Printf("spawned %s\n", res.Agent)
	fmt.Printf("  session: %s (attach: tmux attach -t %s)\n", res.Session, res.Session)
	fmt.Printf("  pane:    %s\n", res.Pane)
	if *waitReady {
		fmt.Printf("  ready:   %v\n", res.Ready)
	}
	if *grant {
		fmt.Printf("  control: granted (HIVE_CONTROL_TOKEN injected)\n")
	}
	return nil
}

func runKeys(args []string) error {
	fs := flags("keys", args)
	enter := fs.Bool("enter", false, "press Enter after the text")
	fs.Parse2()
	agent := fs.pos(0)
	text := fs.body(1)
	if agent == "" || (text == "" && !*enter) {
		return fmt.Errorf("usage: hive keys [--enter] <agent> <text...>")
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	if err := c.Keys(agent, text, *enter); err != nil {
		return err
	}
	fmt.Printf("sent %d byte(s) to %s (enter=%v)\n", len(text), agent, *enter)
	return nil
}

func runRead(args []string) error {
	fs := flags("read", args)
	lines := fs.Int("lines", 0, "extra scrollback lines to include")
	fs.Parse2()
	agent := fs.pos(0)
	if agent == "" {
		return fmt.Errorf("usage: hive read [--lines N] <agent>")
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	screen, err := c.Read(agent, *lines)
	if err != nil {
		return err
	}
	// Panes are fixed-height; drop the trailing blank rows.
	fmt.Println(strings.TrimRight(screen, " \t\n"))
	return nil
}

func runKill(args []string) error {
	fs := flags("kill", args)
	fs.Parse2()
	agent := fs.pos(0)
	if agent == "" {
		return fmt.Errorf("usage: hive kill <agent>")
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	killed, err := c.Kill(agent)
	if err != nil {
		return err
	}
	if killed {
		fmt.Printf("killed %s (tmux session terminated, deregistered)\n", agent)
	} else {
		fmt.Printf("deregistered %s (no tmux session to kill)\n", agent)
	}
	return nil
}
