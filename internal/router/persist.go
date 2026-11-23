package router

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const stateVersion = 1

// persisted is the on-disk state. It never contains API keys or tokens:
// clients are aliases or caller-scope hashes, accounts are auth IDs/labels.
type persisted struct {
	Version      int                                   `json:"version"`
	SavedAt      time.Time                             `json:"saved_at"`
	ObserveStart time.Time                             `json:"observe_start"`
	Accounts     map[string]*accountState              `json:"accounts"`
	Hours        map[string]map[int64]*hourBucket      `json:"hours"`
	Scopes       map[string]string                     `json:"scopes"`
	Overrides    map[string]Override                   `json:"overrides"`
}

func (r *Router) loadState(path string) {
	raw, err := os.ReadFile(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadedFrom = path
	if errors.Is(err, fs.ErrNotExist) {
		r.stateErr = ""
		return
	}
	if err != nil {
		r.stateErr = "load: " + err.Error()
		return
	}
	var p persisted
	if err := json.Unmarshal(raw, &p); err != nil || p.Version != stateVersion {
		if err == nil {
			err = errors.New("unsupported state version")
		}
		r.stateErr = "load: " + err.Error()
		return
	}
	if !p.ObserveStart.IsZero() && p.ObserveStart.Before(r.observeStart) {
		r.observeStart = p.ObserveStart
	}
	for id, st := range p.Accounts {
		if st == nil {
			continue
		}
		if cur, ok := r.accounts[id]; ok {
			// Live data observed before the load wins; keep history from disk.
			if !cur.HasSnapshot {
				cur.Snapshot, cur.HasSnapshot = st.Snapshot, st.HasSnapshot
			}
			if cur.LastWeekly == nil {
				cur.LastWeekly = st.LastWeekly
			}
			continue
		}
		st.Info.ID = id
		r.accounts[id] = st
	}
	for id, hs := range p.Hours {
		cur := r.hours[id]
		if cur == nil {
			r.hours[id] = hs
			continue
		}
		for h, b := range hs {
			if _, ok := cur[h]; !ok {
				cur[h] = b
			}
		}
	}
	for k, v := range p.Scopes {
		if _, ok := r.scopes[k]; !ok {
			r.scopes[k] = v
		}
	}
	for k, v := range p.Overrides {
		if _, ok := r.overrides[k]; !ok {
			r.overrides[k] = v
		}
	}
	r.stateErr = ""
}

func (r *Router) saveState() {
	r.mu.Lock()
	path := r.cfg.StateFile
	if path == "" || !r.dirty {
		r.mu.Unlock()
		return
	}
	p := persisted{
		Version: stateVersion, SavedAt: r.now(), ObserveStart: r.observeStart,
		Accounts: r.accounts, Hours: r.hours, Scopes: r.scopes, Overrides: r.overrides,
	}
	raw, err := json.Marshal(p)
	r.dirty = false
	r.mu.Unlock()
	if err == nil {
		err = writeAtomic(path, raw)
	}
	r.mu.Lock()
	if err != nil {
		r.stateErr = "save: " + err.Error()
		r.dirty = true
	} else {
		r.stateErr = ""
	}
	r.mu.Unlock()
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".burndown-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
