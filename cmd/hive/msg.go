package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/benhynes/hive/internal/client"
	"github.com/benhynes/hive/internal/proto"
)

func runSend(args []string) error {
	fs := flags("send", args)
	fs.Parse2()
	to := fs.pos(0)
	body := fs.body(1)
	if to == "" || body == "" {
		return fmt.Errorf("usage: hive send <to|@all> <body...>")
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	to, err = c.ExpandAgent(to)
	if err != nil {
		return err
	}
	res, err := c.Send(to, proto.KindMsg, body, "")
	if err != nil {
		return err
	}
	if to != proto.Broadcast {
		if st := res.Results[to]; st != "delivered" {
			return fmt.Errorf("%s: %s", to, st)
		}
		fmt.Printf("delivered %s (id %s)\n", to, res.ID)
		return nil
	}
	targets := make([]string, 0, len(res.Results))
	for t := range res.Results {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	delivered := 0
	for _, t := range targets {
		st := res.Results[t]
		if st == "delivered" {
			delivered++
		}
		fmt.Printf("%-32s %s\n", t, st)
	}
	fmt.Printf("delivered to %d agent(s) (id %s)\n", delivered, res.ID)
	return nil
}

func runRecv(args []string) error {
	fs := flags("recv", args)
	wait := fs.Int("wait", 0, "seconds to wait for at least one message")
	follow := fs.Bool("follow", false, "keep listening; print messages as they arrive")
	jsonOut := fs.Bool("json", false, "print records as JSON lines")
	max := fs.Int("max", 100, "max messages per read")
	noAck := fs.Bool("no-ack", false, "peek without advancing the cursor")
	agent := fs.String("agent", "", "read another agent's inbox (control layer; implies --no-ack)")
	fs.Parse2()
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	if *agent != "" {
		*noAck = true
	}

	after := int64(-1) // -1 = server-side stored cursor
	deadline := time.Now().Add(time.Duration(*wait) * time.Second)
	printed := 0
	for {
		w := 0
		if *follow {
			w = 25
		} else if *wait > 0 {
			rem := int(time.Until(deadline).Seconds()) + 1
			if rem > 25 {
				rem = 25
			}
			if rem > 0 {
				w = rem
			}
		}
		res, err := c.Inbox(after, w, *max, *agent)
		if err != nil {
			return err
		}
		if res.Skipped > 0 {
			fmt.Fprintf(os.Stderr, "hive: warning: %d message(s) dropped before they were read (inbox overflow)\n", res.Skipped)
		}
		var top int64
		for _, m := range res.Msgs {
			if *jsonOut {
				b, _ := json.Marshal(m)
				fmt.Println(string(b))
			} else {
				printRec(m)
			}
			if m.Seq > top {
				top = m.Seq
			}
		}
		if top > 0 {
			after = top
			printed += len(res.Msgs)
			if !*noAck {
				if err := c.Ack(top); err != nil {
					return err
				}
			}
		}
		if *follow {
			continue
		}
		if printed > 0 || *wait <= 0 || !time.Now().Before(deadline) {
			break
		}
	}
	if printed == 0 && !*jsonOut {
		fmt.Fprintln(os.Stderr, "(no new messages)")
	}
	return nil
}

// printRec renders one inbox record. Asks carry their id so the reader
// can `hive answer <id> ...`.
func printRec(m client.Rec) {
	ts := time.UnixMilli(m.Env.TS).Format("15:04:05")
	head := fmt.Sprintf("#%d %s <%s>", m.Seq, ts, m.Env.From)
	switch m.Env.Kind {
	case proto.KindAsk:
		head += " ask(" + m.Env.ID + ")"
	case proto.KindAnswer:
		head += " answer(" + m.Env.CorrID + ")"
	}
	if strings.Contains(m.Env.Body, "\n") {
		fmt.Printf("%s:\n%s\n", head, m.Env.Body)
	} else {
		fmt.Printf("%s: %s\n", head, m.Env.Body)
	}
}

func runAsk(args []string) error {
	fs := flags("ask", args)
	timeout := fs.Int("timeout", 60, "seconds to wait for the answer")
	fs.Parse2()
	to := fs.pos(0)
	question := fs.body(1)
	if to == "" || question == "" {
		return fmt.Errorf("usage: hive ask [--timeout S] <to> <question...>")
	}
	if to == proto.Broadcast {
		return fmt.Errorf("ask needs a single target (broadcast answers would race)")
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	to, err = c.ExpandAgent(to)
	if err != nil {
		return err
	}
	answer, status, err := c.Ask(to, question, time.Duration(*timeout)*time.Second)
	switch status {
	case "answered":
		fmt.Println(answer)
		return nil
	case "undeliverable":
		return fmt.Errorf("%s undeliverable: %v", to, err)
	case "timeout":
		return fmt.Errorf("no answer from %s within %ds (the ask was delivered — the answer may still arrive in `hive recv`)", to, *timeout)
	default:
		return err
	}
}

func runAsks(args []string) error {
	fs := flags("asks", args)
	fs.Parse2()
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	asks, err := c.Asks()
	if err != nil {
		return err
	}
	if len(asks) == 0 {
		fmt.Println("(no pending asks)")
		return nil
	}
	for _, m := range asks {
		fmt.Printf("%s  from %s  %s ago\n", m.Env.ID, m.Env.From, ago(m.Env.TS))
		for _, line := range strings.Split(m.Env.Body, "\n") {
			fmt.Printf("  %s\n", line)
		}
	}
	fmt.Println("\nanswer with: hive answer <id> <text...>")
	return nil
}

func runAnswer(args []string) error {
	fs := flags("answer", args)
	fs.Parse2()
	askID := fs.pos(0)
	body := fs.body(1)
	if askID == "" || body == "" {
		return fmt.Errorf("usage: hive answer <ask-id> <body...>")
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	res, err := c.Answer(askID, body)
	if err != nil {
		return err
	}
	for to, st := range res.Results {
		if st != "delivered" {
			return fmt.Errorf("%s: %s", to, st)
		}
		fmt.Printf("answered %s\n", to)
	}
	return nil
}
