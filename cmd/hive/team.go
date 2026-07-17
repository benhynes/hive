package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/benhynes/hive/internal/client"
	"github.com/benhynes/hive/internal/config"
)

type teamResult struct {
	member config.TeamMember
	spawn  client.SpawnResp
	err    error
	state  string
}

func runTeam(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: hive team up|status|down <name-or-manifest>")
	}
	switch args[0] {
	case "up":
		return runTeamUp(args[1:])
	case "status":
		return runTeamStatus(args[1:])
	case "down":
		return runTeamDown(args[1:])
	default:
		return fmt.Errorf("usage: hive team up|status|down <name-or-manifest>")
	}
}

func loadTeamCommand(name string) (config.Team, error) {
	if name == "" {
		return config.Team{}, fmt.Errorf("missing team name or manifest path")
	}
	team, _, err := config.LoadTeam(name)
	return team, err
}

func runTeamUp(args []string) error {
	fs := flags("team up", args)
	noReplace := fs.Bool("no-replace", false, "leave existing members running instead of replacing them")
	noHarnessSync := fs.Bool("no-harness-sync", false, "skip global Codex/Claude tooling integration")
	fs.Parse2()
	team, err := loadTeamCommand(fs.pos(0))
	if err != nil {
		return err
	}
	if team.Host == "" && !*noHarnessSync {
		if err := syncHarnesses(false, false); err != nil {
			return fmt.Errorf("harness sync: %w", err)
		}
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}

	results := make([]teamResult, len(team.Members))
	var wg sync.WaitGroup
	for i, member := range team.Members {
		wg.Add(1)
		go func(i int, member config.TeamMember) {
			defer wg.Done()
			res, err := c.SpawnWithOptions(client.SpawnOptions{
				Host: team.Host, Name: member.Name, Cwd: member.Cwd, Profile: member.Profile,
				GrantControl: member.GrantControl, WaitReady: true, Nudge: team.NudgeFor(member),
				Persist: member.Persist, Replace: !*noReplace,
			})
			results[i] = teamResult{member: member, spawn: res, err: err}
		}(i, member)
	}
	wg.Wait()
	printTeamResults(team.Name, results)
	var failures []string
	for _, result := range results {
		if result.err != nil {
			failures = append(failures, result.member.Name+": "+result.err.Error())
		} else if !result.spawn.Ready {
			reason := result.spawn.State
			if result.spawn.Detail != "" {
				reason += "/" + result.spawn.Detail
			}
			failures = append(failures, result.member.Name+": "+reason)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("team %s started with %d failure(s): %s", team.Name, len(failures), strings.Join(failures, "; "))
	}
	return nil
}

func printTeamResults(name string, results []teamResult) {
	fmt.Printf("team %s\n", name)
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "MEMBER\tPROFILE\tALIVE\tSTATE\tDETAIL\tTRANSCRIPT")
	for _, result := range results {
		if result.err != nil {
			fmt.Fprintf(w, "%s\t%s\tfalse\terror\t%s\t-\n",
				result.member.Name, result.member.Profile, oneLine(result.err.Error()))
			continue
		}
		state := result.spawn.State
		if state == "" {
			state = "started"
		}
		fmt.Fprintf(w, "%s\t%s\t%v\t%s\t%s\t%s\n",
			result.member.Name, result.member.Profile, state != "exited", state,
			valueOrDash(result.spawn.Detail), valueOrDash(result.spawn.Transcript))
	}
	w.Flush()
}

func runTeamStatus(args []string) error {
	fs := flags("team status", args)
	fs.Parse2()
	team, err := loadTeamCommand(fs.pos(0))
	if err != nil {
		return err
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	host := team.Host
	if host == "" {
		host, err = c.Self()
		if err != nil {
			return err
		}
	}
	agents, err := c.Agents(false)
	if err != nil {
		return err
	}
	byName := map[string]client.AgentInfo{}
	for _, agent := range agents.Agents {
		byName[agent.Agent] = agent
	}
	fmt.Printf("team %s\n", team.Name)
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "MEMBER\tPROFILE\tALIVE\tCONTROLLABLE\tTRANSCRIPT")
	for _, member := range team.Members {
		agent, ok := byName[member.Name+"@"+host]
		if !ok {
			fmt.Fprintf(w, "%s\t%s\tfalse\tfalse\t-\n", member.Name, member.Profile)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%v\t%v\t%s\n",
			member.Name, member.Profile, agent.Alive, agent.Controllable, valueOrDash(agent.Transcript))
	}
	w.Flush()
	return nil
}

func runTeamDown(args []string) error {
	fs := flags("team down", args)
	fs.Parse2()
	team, err := loadTeamCommand(fs.pos(0))
	if err != nil {
		return err
	}
	c, err := client.Resolve(*fs.net)
	if err != nil {
		return err
	}
	host := team.Host
	if host == "" {
		host, err = c.Self()
		if err != nil {
			return err
		}
	}
	agents, err := c.Agents(false)
	if err != nil {
		return err
	}
	present := map[string]bool{}
	for _, agent := range agents.Agents {
		present[agent.Agent] = true
	}
	var failures []string
	for _, member := range team.Members {
		full := member.Name + "@" + host
		if !present[full] {
			fmt.Printf("%s: already down\n", member.Name)
			continue
		}
		if _, err := c.Kill(full, true); err != nil {
			failures = append(failures, member.Name+": "+err.Error())
			continue
		}
		fmt.Printf("%s: down\n", member.Name)
	}
	sort.Strings(failures)
	if len(failures) > 0 {
		return fmt.Errorf("team %s stopped with %d failure(s): %s", team.Name, len(failures), strings.Join(failures, "; "))
	}
	return nil
}

func valueOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
