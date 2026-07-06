package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/benhynes/hive/internal/client"
)

func runHosts(args []string) error {
	sub, rest := "list", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, rest = args[0], args[1:]
	}
	fs := flags("hosts "+sub, rest)
	fs.Parse2()
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	switch sub {
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
