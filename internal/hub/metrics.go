package hub

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/benhynes/hive/internal/config"
)

// hMetrics exposes aggregate, read-only transport health. It deliberately
// omits agent names, tokens, addresses, message bodies, and pane contents so a
// metrics collector never needs Hive control authority.
func (h *Hub) hMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	host := metricLabel(h.Cfg.HostName)
	fmt.Fprintln(w, "# HELP hive_up Whether the Hive daemon metrics handler is responding.")
	fmt.Fprintln(w, "# TYPE hive_up gauge")
	fmt.Fprintf(w, "hive_up{host=\"%s\"} 1\n", host)

	names, err := config.ListNets()
	fmt.Fprintln(w, "# HELP hive_metrics_scrape_error Whether a configured network could not be observed.")
	fmt.Fprintln(w, "# TYPE hive_metrics_scrape_error gauge")
	if err != nil {
		fmt.Fprintf(w, "hive_metrics_scrape_error{host=\"%s\",network=\"_all\"} 1\n", host)
		return
	}
	sort.Strings(names)

	writeHiveMetricHeaders(w)
	for _, name := range names {
		netLabel := metricLabel(name)
		n, err := h.net(name)
		if err != nil {
			fmt.Fprintf(w, "hive_metrics_scrape_error{host=\"%s\",network=\"%s\"} 1\n", host, netLabel)
			continue
		}

		aliveCount, deadCount, controllable, persistentReady, inboxLag := 0, 0, 0, 0, int64(0)
		livePane := make(map[string]bool)
		scrapeError := false
		for _, rec := range n.reg.List() {
			isAlive := alive(rec)
			if isAlive {
				aliveCount++
			} else {
				deadCount++
			}
			if isAlive && rec.Pane != "" {
				controllable++
				livePane[rec.Name] = true
			}
			if ib, err := n.inbox(rec.Name); err == nil {
				inboxLag += ib.Lag()
			} else {
				scrapeError = true
			}
		}
		fmt.Fprintf(w, "hive_agents{host=\"%s\",network=\"%s\",state=\"alive\"} %d\n", host, netLabel, aliveCount)
		fmt.Fprintf(w, "hive_agents{host=\"%s\",network=\"%s\",state=\"dead\"} %d\n", host, netLabel, deadCount)
		fmt.Fprintf(w, "hive_agents_controllable{host=\"%s\",network=\"%s\"} %d\n", host, netLabel, controllable)
		fmt.Fprintf(w, "hive_inbox_lag_messages{host=\"%s\",network=\"%s\"} %d\n", host, netLabel, inboxLag)
		persistent := n.persist.List()
		for _, spec := range persistent {
			if livePane[spec.Name] {
				persistentReady++
			}
		}
		fmt.Fprintf(w, "hive_persistent_sessions{host=\"%s\",network=\"%s\"} %d\n", host, netLabel, len(persistent))
		fmt.Fprintf(w, "hive_persistent_sessions_ready{host=\"%s\",network=\"%s\"} %d\n", host, netLabel, persistentReady)
		fmt.Fprintf(w, "hive_known_hosts{host=\"%s\",network=\"%s\"} %d\n", host, netLabel, len(n.hosts()))

		n.mu.Lock()
		scope := "legacy_shared"
		if n.cfg.ControlHost != "" {
			scope = "host_local"
		}
		n.mu.Unlock()
		fmt.Fprintf(w, "hive_control_scope{host=\"%s\",network=\"%s\",scope=\"%s\"} 1\n", host, netLabel, scope)
		fmt.Fprintf(w, "hive_metrics_scrape_error{host=\"%s\",network=\"%s\"} %d\n", host, netLabel, boolMetric(scrapeError))
	}
}

func writeHiveMetricHeaders(w http.ResponseWriter) {
	for _, line := range []string{
		"# HELP hive_agents Registered agents by observed liveness.",
		"# TYPE hive_agents gauge",
		"# HELP hive_agents_controllable Registered agents with a controllable pane.",
		"# TYPE hive_agents_controllable gauge",
		"# HELP hive_inbox_lag_messages Total unacknowledged messages across registered agents.",
		"# TYPE hive_inbox_lag_messages gauge",
		"# HELP hive_persistent_sessions Declared sessions supervised by the daemon.",
		"# TYPE hive_persistent_sessions gauge",
		"# HELP hive_persistent_sessions_ready Declared sessions with a live controllable pane.",
		"# TYPE hive_persistent_sessions_ready gauge",
		"# HELP hive_known_hosts Hosts in this hub's local routing table.",
		"# TYPE hive_known_hosts gauge",
		"# HELP hive_control_scope Control capability scope configured on this hub.",
		"# TYPE hive_control_scope gauge",
	} {
		fmt.Fprintln(w, line)
	}
}

func metricLabel(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"")
	return r.Replace(s)
}

func boolMetric(v bool) int {
	if v {
		return 1
	}
	return 0
}
