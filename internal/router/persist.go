package router

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/ssaavedra/cpa-fefo-router/internal/pricing"
)

const stateVersion = 1

// persisted is the on-disk state. It never contains API keys or tokens:
// clients are aliases or caller-scope hashes, accounts are auth IDs/labels.
type persisted struct {
	Version      int                              `json:"version"`
	SavedAt      time.Time                        `json:"saved_at"`
	ObserveStart time.Time                        `json:"observe_start"`
	Accounts     map[string]*accountState         `json:"accounts"`
	Hours        map[string]map[int64]*hourBucket `json:"hours"`
	Scopes       map[string]string                `json:"scopes"`
	Overrides    map[string]Override              `json:"overrides"`
	Overlay      string                           `json:"runtime_config,omitempty"`
	OverlayAt    time.Time                        `json:"runtime_config_at,omitzero"`
	OverlayNote  string                           `json:"runtime_config_note,omitempty"`
	// Prices is the last models.dev sync.
	Prices *pricing.Table `json:"prices,omitempty"`
	// MonotonicDelta marks state recorded after weekly readings stopped
	// flapping into the delta. Older learned capacities counted every flap
	// and are discarded on load.
	MonotonicDelta bool `json:"monotonic_delta,omitempty"`
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
		if !p.MonotonicDelta {
			st.Learn = capacityLearn{Folded: st.Learn.Folded}
		}
		if cur, ok := r.accounts[id]; ok {
			// Live data observed before the load wins; keep history from disk.
			if !cur.HasSnapshot {
				cur.Snapshot, cur.HasSnapshot = st.Snapshot, st.HasSnapshot
			}
			if cur.LastWeekly == nil {
				cur.LastWeekly = st.LastWeekly
			}
			if cur.Learn.Folded == 0 {
				cur.Learn = st.Learn
			}
			if !cur.Usage.Report.HasReport {
				cur.Usage.Report, cur.Usage.SpendSince = st.Usage.Report, st.Usage.SpendSince
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
	if r.overlay == "" && p.Overlay != "" {
		r.overlay, r.overlayAt, r.overlayNote = p.Overlay, p.OverlayAt, p.OverlayNote
	}
	if p.Prices != nil && p.Prices.SyncedAt.After(r.synced.SyncedAt) {
		r.synced = *p.Prices
		r.rebuildPricesLocked()
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
		Version: stateVersion, SavedAt: r.now(), ObserveStart: r.observeStart, MonotonicDelta: true,
		Accounts: r.accounts, Hours: r.hours, Scopes: r.scopes, Overrides: r.overrides,
		Overlay: r.overlay, OverlayAt: r.overlayAt, OverlayNote: r.overlayNote,
	}
	if len(r.synced.Models) > 0 {
		synced := r.synced
		p.Prices = &synced
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
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fefo-*.tmp")
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
