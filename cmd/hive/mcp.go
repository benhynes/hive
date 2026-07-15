package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/benhynes/hive/internal/client"
	"github.com/benhynes/hive/internal/mcp"
)

// mcpServerVersion is reported in the MCP handshake. It tracks the hive wire
// protocol version, which is what actually constrains the tools.
const mcpServerVersion = "1"

// runMCP serves the mesh over MCP on stdin/stdout, so an agent calls
// hive_send / hive_ask / hive_recv as native tools instead of shelling out.
//
// Identity comes from the same env `hive spawn` already injects, so a spawned
// agent needs no configuration at all:
//
//	claude mcp add hive -- hive mcp
//
// stdout is the protocol channel — nothing may print there but JSON-RPC.
// Diagnostics go to stderr.
func runMCP(args []string) error {
	fs := flags("mcp", args)
	list := fs.Bool("list", false, "print the tools this agent would be offered, then exit")
	fs.Parse2()

	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	tools := mcp.Tools(c)

	if *list {
		layer := "MSG"
		if c.HasControl() {
			layer = "MSG + CONTROL"
		}
		fmt.Printf("net %s · agent %s · layer %s\n", c.Net, orUnknown(c.Agent), layer)
		fmt.Println(strings.Join(mcp.Names(tools), "\n"))
		return nil
	}

	// A dead hub is worth reporting now, on stderr, rather than as a failure
	// inside the model's first tool call.
	if _, err := c.Self(); err != nil {
		fmt.Fprintf(os.Stderr, "hive mcp: warning: hub at %s unreachable: %v\n", c.Addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := mcp.NewServer("hive", mcpServerVersion, tools)
	return srv.ServeStdio(ctx, os.Stdin, os.Stdout)
}

func orUnknown(s string) string {
	if s == "" {
		return "(unregistered)"
	}
	return s
}
