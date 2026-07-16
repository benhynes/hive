package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var ErrNotRetainedLease = errors.New("registration does not have a retained presence lease")

// AgentRec is one registered agent on this host.
type AgentRec struct {
	Name       string `json:"name"`
	Layer      string `json:"layer"` // "msg" | "control"
	TokenHash  string `json:"token_hash"`
	Pane       string `json:"pane,omitempty"`       // tmux pane id (%N); empty = not controllable
	Nudge      bool   `json:"nudge,omitempty"`      // explicitly allow fixed terminal wake notices
	Session    string `json:"session,omitempty"`    // tmux session, set for spawned agents
	Transcript string `json:"transcript,omitempty"` // retained terminal output for spawned agents
	PID        int    `json:"pid,omitempty"`
	StartEpoch string `json:"start_epoch,omitempty"` // `ps -o lstart=` string; guards pid reuse
	Spawned    bool   `json:"spawned,omitempty"`
	Ephemeral  bool   `json:"ephemeral,omitempty"` // generated identity; retired after its recovery grace
	Registered int64  `json:"registered"`          // unix milliseconds
	// LeaseSeconds is zero for legacy registrations, whose liveness is
	// determined only by their pane/PID binding (or explicit deregistration).
	// A positive value opts the registration into renewable presence.
	LeaseSeconds int   `json:"lease_seconds,omitempty"`
	LastSeen     int64 `json:"last_seen,omitempty"`     // unix milliseconds
	LeaseExpires int64 `json:"lease_expires,omitempty"` // unix milliseconds
}

// Registry is the local-only agent registry for one network, snapshotted
// to <dir>/registry.json.
type Registry struct {
	mu     sync.Mutex
	path   string
	agents map[string]AgentRec // by name
	byTok  map[string]string   // token hash -> name
}

// OpenRegistry loads (or creates) the registry under dir.
func OpenRegistry(dir string) (*Registry, error) {
	r := &Registry{
		path:   filepath.Join(dir, "registry.json"),
		agents: map[string]AgentRec{},
		byTok:  map[string]string{},
	}
	b, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &r.agents); err != nil {
		return nil, err
	}
	for name, a := range r.agents {
		r.byTok[a.TokenHash] = name
	}
	return r, nil
}

// Get returns the record for name.
func (r *Registry) Get(name string) (AgentRec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.agents[name]
	return a, ok
}

// ByToken resolves a token hash to an agent record.
func (r *Registry) ByToken(hash string) (AgentRec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name, ok := r.byTok[hash]
	if !ok {
		return AgentRec{}, false
	}
	return r.agents[name], true
}

// List returns all records.
func (r *Registry) List() []AgentRec {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]AgentRec, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, a)
	}
	return out
}

// Put inserts or replaces a record and persists the snapshot.
func (r *Registry) Put(a AgentRec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, hadOld := r.agents[a.Name]
	previousTokenOwner, hadPreviousTokenOwner := r.byTok[a.TokenHash]
	if hadOld {
		delete(r.byTok, old.TokenHash)
	}
	r.agents[a.Name] = a
	r.byTok[a.TokenHash] = a.Name
	if err := r.saveLocked(); err != nil {
		delete(r.byTok, a.TokenHash)
		if hadPreviousTokenOwner {
			r.byTok[a.TokenHash] = previousTokenOwner
		}
		if hadOld {
			r.agents[a.Name] = old
			r.byTok[old.TokenHash] = old.Name
		} else {
			delete(r.agents, a.Name)
		}
		return err
	}
	return nil
}

// RenewLease records activity for name and extends its presence lease from
// now. Legacy, unleased registrations are accepted as a no-op so callers can
// safely heartbeat without first branching on registration metadata.
//
// tokenHash must still own name; this prevents a late heartbeat from an
// expired/replaced process renewing a new claimant's record. The returned bool
// reports whether the name/token pair still exists. A successful renewal is
// persisted before it is returned, keeping discovery consistent across a hub
// restart.
func (r *Registry) RenewLease(name, tokenHash string, now time.Time) (AgentRec, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.agents[name]
	if !ok || a.TokenHash != tokenHash {
		return AgentRec{}, false, nil
	}
	if a.LeaseSeconds <= 0 {
		return a, true, nil
	}
	old := a
	a.LastSeen = now.UnixMilli()
	a.LeaseExpires = now.Add(time.Duration(a.LeaseSeconds) * time.Second).UnixMilli()
	r.agents[name] = a
	if err := r.saveLocked(); err != nil {
		r.agents[name] = old
		return AgentRec{}, true, err
	}
	return a, true, nil
}

// ReleaseLease marks a retained, leased identity offline immediately without
// deleting its registry record or durable mailbox. The name can be reclaimed
// at once, while peers may continue queueing mail for the next claimant.
func (r *Registry) ReleaseLease(name, tokenHash string, now time.Time) (AgentRec, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.agents[name]
	if !ok || a.TokenHash != tokenHash {
		return AgentRec{}, false, nil
	}
	if a.LeaseSeconds <= 0 || a.Ephemeral {
		return a, true, ErrNotRetainedLease
	}
	old := a
	a.LastSeen = now.UnixMilli()
	a.LeaseExpires = now.UnixMilli()
	r.agents[name] = a
	if err := r.saveLocked(); err != nil {
		r.agents[name] = old
		return AgentRec{}, true, err
	}
	return a, true, nil
}

// PruneExpiredEphemeral removes generated registrations whose renewable lease
// has been expired for at least grace. Explicitly named and legacy
// registrations are deliberately retained, preserving their durable
// mailbox/resume semantics. Callers that coordinate name ownership must hold
// their higher-level claim lock while this method runs.
func (r *Registry) PruneExpiredEphemeral(now time.Time, grace time.Duration, beforeDelete func(string) error) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := now.UnixMilli()
	var pruned []string
	for name, a := range r.agents {
		if !a.Ephemeral || a.LeaseSeconds <= 0 || a.LeaseExpires <= 0 || cutoff < a.LeaseExpires+grace.Milliseconds() {
			continue
		}
		pruned = append(pruned, name)
	}
	if len(pruned) == 0 {
		return nil, nil
	}
	sort.Strings(pruned)
	// Retire mailboxes before making names reusable. A cleanup failure leaves
	// the registry record in place, so a replacement cannot inherit old mail.
	if beforeDelete != nil {
		for _, name := range pruned {
			if err := beforeDelete(name); err != nil {
				return nil, err
			}
		}
	}
	removed := make(map[string]AgentRec, len(pruned))
	for _, name := range pruned {
		a := r.agents[name]
		delete(r.byTok, a.TokenHash)
		delete(r.agents, name)
		removed[name] = a
	}
	if err := r.saveLocked(); err != nil {
		// Restore the registry maps to the persisted snapshot so the failed
		// prune does not make these names reusable in memory. Mailbox retirement
		// happened first and cannot be rolled back; callers must use this method
		// only for disposable identities whose recovery grace has ended.
		for name, a := range removed {
			r.agents[name] = a
			r.byTok[a.TokenHash] = name
		}
		return nil, err
	}
	return pruned, nil
}

// Delete removes a record (revoking its token) and persists.
func (r *Registry) Delete(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	old, hadOld := r.agents[name]
	previousTokenOwner, hadPreviousTokenOwner := r.byTok[old.TokenHash]
	if hadOld {
		delete(r.byTok, old.TokenHash)
		delete(r.agents, name)
	}
	if err := r.saveLocked(); err != nil {
		if hadOld {
			r.agents[name] = old
			if hadPreviousTokenOwner {
				r.byTok[old.TokenHash] = previousTokenOwner
			}
		}
		return err
	}
	return nil
}

func (r *Registry) saveLocked() error {
	b, err := json.MarshalIndent(r.agents, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}
