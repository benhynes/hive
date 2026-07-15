package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/benhynes/hive/internal/client"
)

func runSpawn(args []string) error {
	fs := flags("spawn", args)
	host := fs.String("host", "", "target host (default: this host)")
	cwd := fs.String("cwd", "", "working directory for the spawned process")
	grant := fs.Bool("grant-control", false, "inject this host's control token (CONTROL layer)")
	waitReady := fs.Bool("wait", false, "wait until the pane draws and goes quiet")
	headed := fs.Bool("headed", false, "open a visible terminal window on the target host attached to the session")
	persist := fs.Bool("persist", false, "declare the session: the daemon respawns it after reboot or crash")
	fs.Parse2()
	name := fs.pos(0)
	cmd := fs.afterDD
	if name == "" || len(cmd) == 0 {
		return fmt.Errorf("usage: hive spawn [--host H] [--cwd D] [--grant-control] [--wait] [--headed] [--persist] <name> -- CMD...")
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	res, err := c.Spawn(*host, name, cmd, *cwd, *grant, *waitReady, *headed, *persist)
	if err != nil {
		return err
	}
	fmt.Printf("spawned %s\n", res.Agent)
	if strings.HasPrefix(res.Pane, "win:") {
		// Windows console sessions have no attach command; --headed (or
		// later `hive spawn --headed`) is the way to see them.
		fmt.Printf("  session: %s\n", res.Session)
	} else {
		fmt.Printf("  session: %s (attach: tmux attach -t %s)\n", res.Session, res.Session)
	}
	fmt.Printf("  pane:    %s\n", res.Pane)
	if *waitReady {
		fmt.Printf("  ready:   %v\n", res.Ready)
	}
	if *grant {
		fmt.Printf("  control: granted (HIVE_CONTROL_TOKEN injected)\n")
	}
	if *persist {
		fmt.Printf("  persist: declared (daemon respawns it after reboot/crash; remove with hive kill --forget)\n")
	}
	if *headed {
		if res.Window == "opened" {
			fmt.Printf("  window:  opened on the target host\n")
		} else {
			fmt.Printf("  window:  FAILED — %s (session still running headless)\n",
				strings.TrimPrefix(res.Window, "error: "))
		}
	}
	return nil
}

func runKeys(args []string) error {
	fs := flags("keys", args)
	enter := fs.Bool("enter", false, "press Enter after the text")
	stdin := fs.Bool("stdin", false, "read text from stdin instead of argv")
	raw := fs.Bool("raw", false, "send bytes without paste-mode heuristics")
	fs.Parse2()
	agent := fs.pos(0)
	text := fs.body(1)
	if *stdin {
		if text != "" {
			return fmt.Errorf("keys --stdin does not accept text arguments")
		}
		b, err := io.ReadAll(io.LimitReader(os.Stdin, (1<<20)+1))
		if err != nil {
			return err
		}
		if len(b) > 1<<20 {
			return fmt.Errorf("stdin exceeds 1 MiB")
		}
		text = string(b)
	}
	if agent == "" || (text == "" && !*enter) {
		return fmt.Errorf("usage: hive keys [--enter] [--stdin] [--raw] <agent> <text...>")
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	if *raw {
		if err := c.KeysRaw(agent, text); err != nil {
			return err
		}
		if *enter {
			if err := c.Keys(agent, "", true); err != nil {
				return err
			}
		}
	} else if err := c.Keys(agent, text, *enter); err != nil {
		return err
	}
	fmt.Printf("sent %d byte(s) to %s (enter=%v raw=%v)\n", len(text), agent, *enter, *raw)
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
	forget := fs.Bool("forget", false, "also drop the persist declaration (else a declared agent respawns)")
	fs.Parse2()
	agent := fs.pos(0)
	if agent == "" {
		return fmt.Errorf("usage: hive kill [--forget] <agent>")
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	killed, err := c.Kill(agent, *forget)
	if err != nil {
		return err
	}
	if killed {
		fmt.Printf("killed %s (session terminated, deregistered)\n", agent)
	} else {
		fmt.Printf("deregistered %s (no live session to kill)\n", agent)
	}
	if *forget {
		fmt.Printf("  persist: declaration removed\n")
	}
	return nil
}
