package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/benhynes/hive/internal/client"
	"github.com/benhynes/hive/internal/config"
)

func runHosts(args []string) error {
	sub, rest := "list", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, rest = args[0], args[1:]
	}
	fs := flags("hosts "+sub, rest)
	profile := fs.String("profile", "", "default spawn profile for this SSH host")
	home := fs.String("home", "", "remote HIVE_HOME (default ~/.hive)")
	identity := fs.String("identity", "", "ssh key path")
	binPath := fs.String("bin", "", "local path to a target-platform hive binary (else self-copy)")
	port := fs.Int("port", 0, "remote daemon loopback port (default 7777)")
	fs.Parse2()
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	switch sub {
	case "add-ssh":
		name, target := fs.pos(0), fs.pos(1)
		if name == "" || target == "" {
			return fmt.Errorf("usage: hive hosts add-ssh [--profile P] [--home DIR] [--port N] [--identity KEY] [--bin PATH] <name> <ssh-target>")
		}
		if err := c.AddSSHHost(name, config.SSHHost{
			Target: target, Port: *port, Home: *home, Bin: *binPath, Identity: *identity, Profile: *profile,
		}); err != nil {
			return err
		}
		fmt.Printf("registered SSH host %s -> %s (brought up on first spawn)\n", name, target)
		return nil
	case "rm-ssh":
		name := fs.pos(0)
		if name == "" {
			return fmt.Errorf("usage: hive hosts rm-ssh <name>")
		}
		if err := c.RemoveSSHHost(name); err != nil {
			return err
		}
		fmt.Printf("removed SSH host %s (tunnel + transient daemon torn down)\n", name)
		return nil
	case "list":
		res, err := c.Hosts()
		if err != nil {
			return err
		}
		printHosts(res)
		return nil
	case "add":
		name, addr := fs.pos(0), fs.pos(1)
		if name == "" || addr == "" {
			return fmt.Errorf("usage: hive hosts add <name> <addr:port>")
		}
		res, err := c.HostsMod("add", name, addr)
		if err != nil {
			return err
		}
		printHosts(res)
		return nil
	case "rm":
		name := fs.pos(0)
		if name == "" {
			return fmt.Errorf("usage: hive hosts rm <name>")
		}
		res, err := c.HostsMod("rm", name, "")
		if err != nil {
			return err
		}
		printHosts(res)
		return nil
	default:
		return fmt.Errorf("usage: hive hosts <list|add|rm> ...")
	}
}

func printHosts(res client.HostsResp) {
	names := make([]string, 0, len(res.Hosts))
	for n := range res.Hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		marker := " "
		if n == res.Self {
			marker = "*"
		}
		fmt.Printf("%s %-16s %s\n", marker, n, res.Hosts[n])
	}
}
