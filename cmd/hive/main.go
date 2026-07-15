// Command hive is the single binary for the agent communication mesh:
// `hive daemon` runs a host's hub; every other subcommand is a client.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/control"
	"github.com/benhynes/hive/internal/hub"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "daemon":
		err = runDaemon(args)
	case "net":
		err = runNet(args)
	case "register":
		err = runRegister(args)
	case "deregister":
		err = runDeregister(args)
	case "agents":
		err = runAgents(args)
	case "hosts":
		err = runHosts(args)
	case "node":
		err = runNode(args)
	case "spawn":
		err = runSpawn(args)
	case "keys":
		err = runKeys(args)
	case "read":
		err = runRead(args)
	case "kill":
		err = runKill(args)
	case "mcp":
		err = runMCP(args)
	case "__conop":
		// Hidden Windows console-op helper the daemon re-execs itself
		// as; see internal/control/conop_windows.go.
		err = control.RunConOp(args)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "hive: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hive: "+err.Error())
		os.Exit(1)
	}
}

func runDaemon(args []string) error {
	fs := flags("daemon", args)
	bind := fs.String("bind", "", "override bind address (default from config, 127.0.0.1)")
	port := fs.Int("port", 0, "override port (default from config, 7777)")
	home := fs.String("home", "", "state directory (default $HIVE_HOME or ~/.hive)")
	fs.Parse2()
	if *home != "" {
		os.Setenv("HIVE_HOME", *home)
	}

	if err := control.Available(); err != nil {
		fmt.Fprintln(os.Stderr, "hive: warning: "+err.Error()+" — control ops will fail")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *bind != "" {
		cfg.Bind = *bind
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if err := config.Save(cfg); err != nil {
		return err
	}
	h := hub.New(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	fmt.Printf("hive daemon: host=%s listening on %s:%d\n", cfg.HostName, cfg.Bind, cfg.Port)
	go h.Reconcile(ctx)
	go h.SweepNudges(ctx)
	return h.ListenAndServe(ctx)
}

func usage() {
	fmt.Print(`hive — agent communication + control mesh

DAEMON
  hive daemon [--bind ADDR] [--port N] [--home DIR]   run this host's hub

NETWORKS
  hive net create <name>                   create a network (prints tokens)
  hive net join <name> --hub ADDR --msg-token T [--control-token T]
  hive net list                            list local networks
  hive net show <name>                     show tokens + hosts
  hive net rotate-control <name>           replace this hub's control token with a host-local token

IDENTITY
  hive register --name N [--pane %ID]      register self, prints export lines
  hive deregister [name]
  hive agents [--local] [--json]           list agents across the mesh

MESSAGING is MCP-only — agents send/recv/ask/answer via the hive_* tools.
  hive mcp [--list]                        stdio MCP server: hive_send, hive_recv,
                                           hive_ask, hive_answer, hive_asks,
                                           hive_agents (+ spawn/keys/read/kill
                                           when the agent holds control).
                                           Register once per agent:
                                             claude mcp add hive -- hive mcp
                                           --list prints the tools and exits.

HOSTS (control layer)
  hive hosts list
  hive hosts add <name> <addr:port>
  hive hosts rm <name>
  hive node install [--name N] [--bind IP] [--port N] [--hub A:P] [--persist]
                    [--msg-only|--local-control] [--restart] [--no-start] <ssh-target>
                                           bootstrap a new host over ssh

CONTROL (control layer; goes direct to the target host)
  hive spawn [--host H] [--cwd D] [--profile P] [--grant-control] [--wait] [--headed] [--persist] <name> [-- CMD...]
                                            --persist: daemon respawns it after reboot/crash
                                            --profile: provision the cwd from ~/.hive/profiles/P.json
                                            (context files + .mcp.json with the hive server,
                                            pre-approved so a headless agent sees no prompts;
                                            a profile runtime makes -- CMD optional)
  hive keys [--enter] <agent> <text...>
  hive read [--lines N] <agent>
  hive kill [--forget] <agent>            --forget drops the persist declaration too

Config: HIVE_ADDR HIVE_NET HIVE_TOKEN HIVE_CONTROL_TOKEN HIVE_CONTROL_HOST HIVE_AGENT
        (per-host: ~/.hive/config.json; per-net: ~/.hive/nets/<net>/)
`)
}
