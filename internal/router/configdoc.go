package router

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ssaavedra/cpa-fefo-router/internal/pricing"
)

// ConfigDoc is the editable routing configuration, as JSON (which is also
// valid YAML, so ParseConfig reads it). Durations are strings like "6h".
type ConfigDoc struct {
	SpareFactor       float64              `json:"spare_factor"`
	MinSpare          float64              `json:"min_spare"`
	MinRemaining      float64              `json:"min_remaining"`
	MinHistory        string               `json:"min_history"`
	StaleAfter        string               `json:"stale_after"`
	ProbeInterval     string               `json:"probe_interval"`
	SessionTTL        string               `json:"session_ttl"`
	UsagePollInterval string               `json:"usage_poll_interval"`
	AllowExtraUsage   bool                 `json:"allow_extra_usage"`
	ScopedFamilies    []string             `json:"scoped_families"`
	LogDecisions      bool                 `json:"log_decisions"`
	Buckets           map[string][]string  `json:"buckets"`
	Capacity          map[string]float64   `json:"capacity"`
	CapacityUSD       map[string]float64   `json:"capacity_usd"`
	DefaultUnitUSD    float64              `json:"default_unit_usd"`
	LearnMinDelta     float64              `json:"learn_min_delta"`
	Clients           map[string]ClientDoc `json:"clients"`
	DefaultClient     ClientDoc            `json:"default_client"`
	Pacing            struct {
		Enabled  bool   `json:"enabled"`
		Horizon  string `json:"horizon"`
		MaxDelay string `json:"max_delay"`
	} `json:"pacing"`
	Wait struct {
		Horizon string `json:"horizon"`
		MaxWait string `json:"max_wait"`
	} `json:"wait"`
	Pricing struct {
		Sync         bool   `json:"sync"`
		SyncInterval string `json:"sync_interval"`
		URL          string `json:"url"`
	} `json:"pricing"`
	Prices map[string]pricing.Rates `json:"prices"`
}

// ClientDoc is one client in a ConfigDoc.
type ClientDoc struct {
	Keys        []string `json:"keys"`
	Buckets     []string `json:"buckets"`
	OnExhausted string   `json:"on_exhausted"`
	Borrow      bool     `json:"borrow"`
}

func durStr(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}

func clientDoc(c ClientConfig) ClientDoc {
	return ClientDoc{Keys: append([]string{}, c.Keys...), Buckets: append([]string{}, c.Buckets...), OnExhausted: string(c.OnExhausted), Borrow: c.BorrowEnabled()}
}

// Doc converts a Config into its editable form.
func (c Config) Doc() ConfigDoc {
	d := ConfigDoc{
		SpareFactor: c.SpareFactor, MinSpare: c.MinSpare, MinRemaining: c.MinRemaining,
		MinHistory: durStr(c.MinHistory), StaleAfter: durStr(c.StaleAfter), ProbeInterval: durStr(c.ProbeInterval),
		SessionTTL: durStr(c.SessionTTL), UsagePollInterval: durStr(c.UsagePollInterval), AllowExtraUsage: c.AllowExtraUsage, LogDecisions: c.LogDecisions,
		Buckets: map[string][]string{}, Capacity: map[string]float64{}, CapacityUSD: map[string]float64{}, Clients: map[string]ClientDoc{},
		DefaultUnitUSD: c.DefaultUnitUSD, LearnMinDelta: c.LearnMinDelta,
		ScopedFamilies: append([]string{}, c.ScopedFamilies...),
		DefaultClient:  clientDoc(c.DefaultClient),
	}
	for k, v := range c.Buckets {
		d.Buckets[k] = append([]string{}, v...)
	}
	for k, v := range c.Capacity {
		d.Capacity[k] = v
	}
	for k, v := range c.CapacityUSD {
		d.CapacityUSD[k] = v
	}
	for k, v := range c.Clients {
		d.Clients[k] = clientDoc(v)
	}
	d.Pacing.Enabled, d.Pacing.Horizon, d.Pacing.MaxDelay = c.Pacing.Enabled, durStr(c.Pacing.Horizon), durStr(c.Pacing.MaxDelay)
	d.Wait.Horizon, d.Wait.MaxWait = durStr(c.Wait.Horizon), durStr(c.Wait.MaxWait)
	d.Pricing.Sync, d.Pricing.SyncInterval, d.Pricing.URL = c.Pricing.Sync, durStr(c.Pricing.SyncInterval), c.Pricing.URL
	d.Prices = map[string]pricing.Rates{}
	for k, v := range c.Prices {
		d.Prices[k] = v
	}
	return d
}

// effectiveLocked recomputes the active config from config.yaml and the
// runtime overlay. Mode and state_file always come from config.yaml.
func (r *Router) effectiveLocked() (Config, error) {
	if strings.TrimSpace(r.overlay) == "" {
		return r.base, nil
	}
	cfg, err := ParseConfig([]byte(r.overlay))
	if err != nil {
		return r.base, err
	}
	cfg.Mode, cfg.StateFile, cfg.RefreshInterval = r.base.Mode, r.base.StateFile, r.base.RefreshInterval
	return cfg, nil
}

// SetBaseConfig installs the config.yaml section and reapplies any overlay.
func (r *Router) SetBaseConfig(raw []byte, cfg Config) {
	r.mu.Lock()
	r.baseRaw = string(raw)
	r.mu.Unlock()
	r.SetConfig(cfg)
}

func (r *Router) applyOverlayLocked() {
	eff, err := r.effectiveLocked()
	if err != nil {
		r.overlayErr = "runtime config ignored: " + err.Error()
		r.cfg = r.base
	} else {
		r.overlayErr = ""
		r.cfg = eff
	}
	r.rebuildPricesLocked()
}

// ConfigView is the GET /fefo/config document.
type ConfigView struct {
	Source       string    `json:"source"` // "config.yaml" or "runtime"
	Effective    ConfigDoc `json:"effective"`
	BaseYAML     string    `json:"base_yaml"`
	Overlay      string    `json:"overlay,omitempty"`
	OverlayAt    time.Time `json:"overlay_at,omitzero"`
	OverlayNote  string    `json:"overlay_note,omitempty"`
	OverlayError string    `json:"overlay_error,omitempty"`
	Mode         Mode      `json:"mode"`
	StateFile    string    `json:"state_file"`
	Accounts     []string  `json:"known_accounts"`
}

// ConfigView returns the current config for the editor.
func (r *Router) ConfigView() ConfigView {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := ConfigView{
		Source: "config.yaml", Effective: r.cfg.Doc(), BaseYAML: r.baseRaw,
		Overlay: r.overlay, OverlayAt: r.overlayAt, OverlayNote: r.overlayNote, OverlayError: r.overlayErr,
		Mode: r.base.Mode, StateFile: r.base.StateFile,
	}
	if strings.TrimSpace(r.overlay) != "" && r.overlayErr == "" {
		v.Source = "runtime"
	}
	for _, id := range sortedKeys(r.accounts) {
		a := r.accounts[id].Info
		v.Accounts = append(v.Accounts, a.display()+" ("+a.Provider+")")
	}
	return v
}

// configRequest is the body of POST /fefo/config.
type configRequest struct {
	// Config replaces the routing configuration (keys as in ConfigDoc).
	Config json.RawMessage `json:"config"`
	// YAML replaces it from YAML text instead.
	YAML string `json:"yaml"`
	// Revert drops the runtime config and returns to config.yaml.
	Revert bool `json:"revert"`
	// AssignKey adds a caller scope to a client (creating it if needed).
	AssignKey *struct {
		Scope  string `json:"scope"`
		Client string `json:"client"`
	} `json:"assign_key"`
	Note string `json:"note"`
	// Force saves even if a wait/fail client would have no account.
	Force bool `json:"force"`
}

// UpdateConfig applies a browser edit. The runtime config persists in the
// state file and wins over config.yaml until reverted.
func (r *Router) UpdateConfig(body []byte) (ConfigView, error) {
	var req configRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return ConfigView{}, fmt.Errorf("decode body: %w", err)
	}
	r.mu.Lock()
	now := r.now()
	var text string
	switch {
	case req.Revert:
		r.overlay, r.overlayAt, r.overlayNote = "", time.Time{}, ""
		r.applyOverlayLocked()
		r.dirty = true
		r.mu.Unlock()
		r.saveState()
		return r.ConfigView(), nil
	case req.AssignKey != nil:
		scope := strings.ToLower(strings.TrimSpace(req.AssignKey.Scope))
		client := strings.TrimSpace(req.AssignKey.Client)
		if len(scope) != 64 || client == "" {
			r.mu.Unlock()
			return ConfigView{}, fmt.Errorf("assign_key needs a 64-hex scope and a client name")
		}
		doc := r.cfg.Doc()
		c := doc.Clients[client]
		if c.OnExhausted == "" {
			c = clientDoc(r.cfg.DefaultClient)
			c.Keys = nil
		}
		if !contains(c.Keys, scope) {
			c.Keys = append(c.Keys, scope)
		}
		doc.Clients[client] = c
		raw, _ := json.Marshal(doc)
		text = string(raw)
	case len(req.Config) > 0:
		text = string(req.Config)
	case strings.TrimSpace(req.YAML) != "":
		text = req.YAML
	default:
		r.mu.Unlock()
		return ConfigView{}, fmt.Errorf("send config, yaml, assign_key or revert")
	}
	next, err := ParseConfig([]byte(text))
	if err != nil {
		r.mu.Unlock()
		return ConfigView{}, err
	}
	if !req.Force {
		if err := r.unreachableLocked(next); err != nil {
			r.mu.Unlock()
			return ConfigView{}, err
		}
	}
	r.overlay, r.overlayAt, r.overlayNote = text, now, req.Note
	r.applyOverlayLocked()
	r.dirty = true
	r.mu.Unlock()
	r.saveState()
	if r.log != nil {
		r.log("info", "fefo runtime config updated", map[string]any{"note": req.Note})
	}
	return r.ConfigView(), nil
}

// unreachableLocked refuses configs where a wait/fail client's buckets match
// no account CPA knows: such a client would only ever get 429s or errors.
func (r *Router) unreachableLocked(cfg Config) error {
	var bad []string
	for _, alias := range sortedKeys(cfg.Clients) {
		c := cfg.Clients[alias]
		if c.OnExhausted == PolicyAny || len(c.Buckets) == 0 {
			continue
		}
		found := false
		for _, b := range c.Buckets {
			for _, ident := range cfg.Buckets[b] {
				for _, st := range r.accounts {
					found = found || st.Info.matches(ident)
				}
			}
		}
		if !found {
			bad = append(bad, fmt.Sprintf("%s (%s, buckets %s)", alias, c.OnExhausted, strings.Join(c.Buckets, ",")))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("these clients would have no account and would only get errors: %s — fix the bucket accounts or save with force", strings.Join(bad, "; "))
	}
	return nil
}
