package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// AgentRec is one registered agent on this host.
type AgentRec struct {
	Name       string `json:"name"`
	Layer      string `json:"layer"` // "msg" | "control"
	TokenHash  string `json:"token_hash"`
	Pane       string `json:"pane,omitempty"`    // tmux pane id (%N); empty = not controllable
	Session    string `json:"session,omitempty"` // tmux session, set for spawned agents
	PID        int    `json:"pid,omitempty"`
	StartEpoch string `json:"start_epoch,omitempty"` // `ps -o lstart=` string; guards pid reuse
	Spawned    bool   `json:"spawned,omitempty"`
	Registered int64  `json:"registered"` // unix milliseconds
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
	if old, ok := r.agents[a.Name]; ok {
		delete(r.byTok, old.TokenHash)
	}
	r.agents[a.Name] = a
	r.byTok[a.TokenHash] = a.Name
	return r.saveLocked()
}

// Delete removes a record (revoking its token) and persists.
func (r *Registry) Delete(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.agents[name]; ok {
		delete(r.byTok, old.TokenHash)
		delete(r.agents, name)
	}
	return r.saveLocked()
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
