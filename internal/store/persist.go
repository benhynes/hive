package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// PersistSpec is one declared (desired-state) session: enough of the
// original spawn request to recreate the agent after a reboot or crash.
type PersistSpec struct {
	Name         string   `json:"name"`
	Cmd          []string `json:"cmd"`
	Cwd          string   `json:"cwd,omitempty"`
	GrantControl bool     `json:"grant_control,omitempty"`
	Declared     int64    `json:"declared"` // unix milliseconds
}

// PersistStore holds a network's declared sessions, snapshotted to
// <dir>/persist.json. The daemon reconciles against it on startup and on a
// timer: any declared session with no live agent is respawned.
type PersistStore struct {
	mu    sync.Mutex
	path  string
	specs map[string]PersistSpec // by name
}

// OpenPersist loads (or creates) the persist store under dir.
func OpenPersist(dir string) (*PersistStore, error) {
	p := &PersistStore{
		path:  filepath.Join(dir, "persist.json"),
		specs: map[string]PersistSpec{},
	}
	b, err := os.ReadFile(p.path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &p.specs); err != nil {
		return nil, err
	}
	return p, nil
}

// Get returns the spec for name.
func (p *PersistStore) Get(name string) (PersistSpec, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.specs[name]
	return s, ok
}

// List returns all declared specs.
func (p *PersistStore) List() []PersistSpec {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PersistSpec, 0, len(p.specs))
	for _, s := range p.specs {
		out = append(out, s)
	}
	return out
}

// Put inserts or replaces a spec and persists the snapshot.
func (p *PersistStore) Put(s PersistSpec) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.specs[s.Name] = s
	return p.saveLocked()
}

// Delete removes a spec and persists. Deleting a missing name is a no-op.
func (p *PersistStore) Delete(name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.specs[name]; !ok {
		return nil
	}
	delete(p.specs, name)
	return p.saveLocked()
}

func (p *PersistStore) saveLocked() error {
	b, err := json.MarshalIndent(p.specs, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}
