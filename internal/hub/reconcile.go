package hub

import (
	"context"
	"time"

	"github.com/benhynes/hive/internal/config"
)

// reconcileEvery is how often the daemon sweeps declared sessions. It is
// also the effective respawn backoff for a crash-looping command.
const reconcileEvery = 30 * time.Second

// Reconcile keeps declared (persisted) sessions alive: an immediate sweep at
// daemon startup brings a rebooted host's agents back with no outside help,
// then a periodic sweep respawns anything that has since died. Run it in a
// goroutine next to ListenAndServe.
func (h *Hub) Reconcile(ctx context.Context) {
	tick := time.NewTicker(reconcileEvery)
	defer tick.Stop()
	for {
		h.reconcileOnce()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// reconcileOnce respawns every declared session with no live agent, across
// all locally configured networks. Failures are audited and retried on the
// next sweep; one bad spec never blocks the others.
func (h *Hub) reconcileOnce() {
	nets, err := config.ListNets()
	if err != nil {
		return
	}
	for _, name := range nets {
		n, err := h.net(name)
		if err != nil {
			continue
		}
		for _, spec := range n.persist.List() {
			if rec, ok := n.reg.Get(spec.Name); ok && alive(rec) {
				continue
			}
			req := spawnReq{
				Name: spec.Name, Cmd: spec.Cmd, Cwd: spec.Cwd,
				GrantControl: spec.GrantControl, Persist: true,
			}
			tok, env, err := h.spawnEnv(n, req)
			if err != nil {
				n.auditLine("daemon (reconcile)", "respawn", spec.Name+"@"+h.Cfg.HostName, "error: "+err.Error())
				continue
			}
			session := "hive-" + n.name + "-" + spec.Name
			if _, serr := h.spawnCore(n, "daemon (reconcile)", req, session, tok, env); serr != nil {
				n.auditLine("daemon (reconcile)", "respawn", spec.Name+"@"+h.Cfg.HostName, "error: "+serr.msg)
			}
		}
	}
}
