package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/proto"
)

func runNet(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: hive net <create|join|list|show> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return netCreate(rest)
	case "join":
		return netJoin(rest)
	case "list":
		nets, err := config.ListNets()
		if err != nil {
			return err
		}
		if len(nets) == 0 {
			fmt.Println("(no networks — create one with: hive net create <name>)")
		}
		for _, n := range nets {
			fmt.Println(n)
		}
		return nil
	case "show":
		return netShow(rest)
	default:
		return fmt.Errorf("unknown: hive net %s", sub)
	}
}

func netCreate(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: hive net create <name>")
	}
	name := args[0]
	if !proto.ValidName(name) {
		return fmt.Errorf("bad network name (want [a-z0-9][a-z0-9_-]*, ≤32)")
	}
	if _, err := config.LoadNet(name); err == nil {
		return fmt.Errorf("network %q already exists here (hive net show %s)", name, name)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	nc := config.NetConfig{
		Name:         name,
		MsgToken:     proto.NewToken(),
		ControlToken: proto.NewToken(),
		Hosts:        map[string]string{cfg.HostName: fmt.Sprintf("127.0.0.1:%d", cfg.Port)},
	}
	if err := config.SaveNet(nc); err != nil {
		return err
	}
	fmt.Printf("Created network %q on host %q.\n\n", name, cfg.HostName)
	fmt.Printf("  msg token:     %s\n", nc.MsgToken)
	fmt.Printf("  control token: %s\n\n", nc.ControlToken)
	fmt.Printf("Join from another host:\n")
	fmt.Printf("  hive net join %s --hub <this-host>:%d --msg-token %s --control-token %s\n",
		name, cfg.Port, nc.MsgToken, nc.ControlToken)
	fmt.Printf("\nGive an agent MSG-only access with just --msg-token.\n")
	return nil
}

func netJoin(args []string) error {
	// Accept `net join <name> --flags` (the shape `net create` prints):
	// take the name first so flag parsing doesn't stop at it.
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fs := flags("net join", args)
	hub := fs.String("hub", "", "addr:port of a hub already in the network")
	msgTok := fs.String("msg-token", "", "network msg token")
	ctlTok := fs.String("control-token", "", "network control token (optional; omit for msg-only)")
	fs.Parse2()
	if name == "" {
		name = fs.pos(0)
	}
	if name == "" || *hub == "" || *msgTok == "" {
		return fmt.Errorf("usage: hive net join <name> --hub ADDR --msg-token T [--control-token T]")
	}
	if !proto.ValidName(name) {
		return fmt.Errorf("bad network name")
	}
	if _, err := config.LoadNet(name); err == nil {
		return fmt.Errorf("already a member of %q here (hive net show %s)", name, name)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Hosts-map keys must equal the host part of agent ids for routing,
	// so learn the peer's real host name instead of guessing a label.
	peer, err := probeHost(*hub)
	if err != nil {
		return fmt.Errorf("cannot reach hub at %s: %v", *hub, err)
	}
	if peer == cfg.HostName {
		return fmt.Errorf("hub at %s reports host name %q, same as this host — give the hosts distinct host_name values in config.json", *hub, peer)
	}
	nc := config.NetConfig{
		Name: name, MsgToken: *msgTok, ControlToken: *ctlTok,
		Hosts: map[string]string{
			cfg.HostName: fmt.Sprintf("127.0.0.1:%d", cfg.Port),
			peer:         *hub,
		},
	}
	if err := config.SaveNet(nc); err != nil {
		return err
	}
	fmt.Printf("Joined network %q. Peer host %q at %s.\n", name, peer, *hub)
	fmt.Printf("Run `hive daemon` if it isn't already running, then `hive agents`.\n")
	fmt.Printf("Hosts lists are local: on existing hosts run `hive hosts add %s <this-host>:%d`.\n", cfg.HostName, cfg.Port)
	return nil
}

// probeHost asks a hub for its host name via the unauthenticated
// health endpoint.
func probeHost(addr string) (string, error) {
	hc := &http.Client{Timeout: 3 * time.Second}
	resp, err := hc.Get("http://" + addr + "/v1/health")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("health: %s", resp.Status)
	}
	var v struct {
		Host string `json:"host"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if !proto.ValidName(v.Host) {
		return "", fmt.Errorf("peer reported bad host name %q", v.Host)
	}
	return v.Host, nil
}

func netShow(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: hive net show <name>")
	}
	nc, err := config.LoadNet(args[0])
	if err != nil {
		return fmt.Errorf("network %q not found here", args[0])
	}
	fmt.Printf("network:       %s\n", nc.Name)
	fmt.Printf("msg token:     %s\n", nc.MsgToken)
	if nc.ControlToken != "" {
		fmt.Printf("control token: %s\n", nc.ControlToken)
	} else {
		fmt.Printf("control token: (none — this host is msg-only)\n")
	}
	fmt.Printf("hosts:\n")
	for name, addr := range nc.Hosts {
		fmt.Printf("  %-16s %s\n", name, addr)
	}
	return nil
}
