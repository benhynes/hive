package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/benhynes/hive/internal/client"
)

func runAgents(args []string) error {
	fs := flags("agents", args)
	local := fs.Bool("local", false, "only this host's agents")
	jsonOut := fs.Bool("json", false, "JSON output")
	fs.Parse2()
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	res, err := c.Agents(*local)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(os.Stdout).Encode(res)
	}
	if len(res.Agents) == 0 {
		fmt.Println("(no agents registered)")
	} else {
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "AGENT\tALIVE\tMAILBOX\tCONTROLLABLE\tNUDGE\tSPAWNED\tREGISTERED")
		for _, a := range res.Agents {
			mailbox := "retained"
			if a.Ephemeral {
				mailbox = "disposable"
			}
			fmt.Fprintf(w, "%s\t%v\t%s\t%v\t%v\t%v\t%s ago\n",
				a.Agent, a.Alive, mailbox, a.Controllable, a.Nudgeable, a.Spawned, ago(a.Registered))
		}
		w.Flush()
	}
	if len(res.Unreachable) > 0 {
		hosts := make([]string, 0, len(res.Unreachable))
		for h := range res.Unreachable {
			hosts = append(hosts, h)
		}
		sort.Strings(hosts)
		for _, h := range hosts {
			fmt.Fprintf(os.Stderr, "hive: host %s unreachable: %s\n", h, res.Unreachable[h])
		}
	}
	return nil
}

// ago renders a unix-millisecond timestamp as a coarse age. Remote
// hosts' clocks may run ahead of ours; never show a negative age.
func ago(ms int64) string {
	d := time.Since(time.UnixMilli(ms))
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
