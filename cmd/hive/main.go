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
	"github.com/benhynes/hive/internal/hub"
	"github.com/benhynes/hive/internal/tmux"
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
	case "send":
		err = runSend(args)
	case "recv":
		err = runRecv(args)
	case "ask":
		err = runAsk(args)
	case "asks":
		err = runAsks(args)
	case "answer":
		err = runAnswer(args)
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
	fs.Parse2()

	if err := tmux.Available(); err != nil {
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
	return h.ListenAndServe(ctx)
}

func usage() {
	fmt.Print(`hive — agent communication + control mesh

DAEMON
  hive daemon [--bind ADDR] [--port N]     run this host's hub

NETWORKS
  hive net create <name>                   create a network (prints tokens)
  hive net join <name> --hub ADDR --msg-token T [--control-token T]
  hive net list                            list local networks
  hive net show <name>                     show tokens + hosts

MESSAGING (msg layer; flags before positionals)
  hive register --name N [--pane %ID]      register self, prints export lines
  hive deregister [name]
  hive agents [--local] [--json]           list agents across the mesh
  hive send <to|@all> <body...>            to = name[@host]
  hive recv [--wait N] [--follow] [--json] [--no-ack]   read + ack own inbox
  hive ask [--timeout S] <to> <question...>  send question, wait for answer
  hive asks                                list questions waiting on you
  hive answer <ask-id> <body...>           answer a question

HOSTS (control layer)
  hive hosts list
  hive hosts add <name> <addr:port>
  hive hosts rm <name>
  hive node install [--name N] [--bind IP] [--port N] [--hub A:P]
                    [--msg-only] [--restart] [--no-start] <ssh-target>
                                           bootstrap a new host over ssh

CONTROL (control layer; goes direct to the target host)
  hive spawn [--host H] [--cwd D] [--grant-control] [--wait] <name> -- CMD...
  hive keys [--enter] <agent> <text...>
  hive read [--lines N] <agent>
  hive kill <agent>

Config: HIVE_ADDR HIVE_NET HIVE_TOKEN HIVE_CONTROL_TOKEN HIVE_AGENT
        (per-host: ~/.hive/config.json; per-net: ~/.hive/nets/<net>/)
`)
}
