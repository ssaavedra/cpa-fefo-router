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
	SpareFactor float64 `json:"spare_factor"`
	MinSpare    float64 `json:"min_spare"`
	// LendReserve is a share of the week: a number, or time left → share.
	LendReserve any `json:"lend_reserve"`
	// LendLimits is lending bucket → target bucket (or "any") → share, each
	// share as in LendReserve.
	LendLimits        map[string]map[string]any `json:"lend_limits"`
	MinRemaining      float64                   `json:"min_remaining"`
	MinHistory        string                    `json:"min_history"`
	StaleAfter        string                    `json:"stale_after"`
	ProbeInterval     string                    `json:"probe_interval"`
	SessionTTL        string                    `json:"session_ttl"`
	UsagePollInterval string                    `json:"usage_poll_interval"`
	AllowExtraUsage   bool                      `json:"allow_extra_usage"`
	ScopedFamilies    []string                  `json:"scoped_families"`
	LogDecisions      bool                      `json:"log_decisions"`
	DisplayUnits      string                    `json:"display_units"`
	Buckets           map[string][]string       `json:"buckets"`
	Capacity          map[string]float64        `json:"capacity"`
	CapacityUSD       map[string]float64        `json:"capacity_usd"`
	DefaultUnitUSD    float64                   `json:"default_unit_usd"`
	LearnMinDelta     float64                   `json:"learn_min_delta"`
	Clients           map[string]ClientDoc      `json:"clients"`
	DefaultClient     ClientDoc                 `json:"default_client"`
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

// leftStr writes a lend_reserve time left, in days when it is whole days.
func leftStr(d time.Duration) string {
	if d > 0 && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return durStr(d)
}

// curveDoc writes a share-of-the-week curve as a number when it is the same
// all week, else as a map from time left to share.
func curveDoc(pts []ReservePoint) any {
	if len(pts) == 1 && pts[0].Left == 0 {
		return pts[0].Keep
	}
	m := map[string]float64{}
	for _, p := range pts {
		m[leftStr(p.Left)] = p.Keep
	}
	return m
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
		ScopedFamilies: append([]string{}, c.ScopedFamilies...), DisplayUnits: c.DisplayUnits,
		DefaultClient: clientDoc(c.DefaultClient),
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
	d.LendReserve = curveDoc(c.LendReserve)
	d.LendLimits = map[string]map[string]any{}
	for b, targets := range c.LendLimits {
		d.LendLimits[b] = map[string]any{}
		for t, pts := range targets {
			d.LendLimits[b][t] = curveDoc(pts)
		}
	}
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
	// Backups are config.yaml sections replaced from the browser, newest
	// first, for restoring.
	Backups []configBackup `json:"backups"`
	// ConfigYAML and Section answer promote and restore: the config.yaml to
	// write and the plugin section in it.
	ConfigYAML string          `json:"config_yaml,omitempty"`
	Section    json.RawMessage `json:"section,omitempty"`
}

// configBackup is a config.yaml section as it was before the browser
// replaced it.
type configBackup struct {
	At      time.Time       `json:"at"`
	Note    string          `json:"note,omitempty"`
	Section json.RawMessage `json:"section"`
}

// maxConfigBackups bounds the backups kept in the state file.
const maxConfigBackups = 20

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
	v.Backups = make([]configBackup, 0, len(r.configBackups))
	for i := len(r.configBackups) - 1; i >= 0; i-- {
		v.Backups = append(v.Backups, r.configBackups[i])
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

	// The browser writes config.yaml itself, with CPA's GET and PUT
	// /v0/management/config.yaml (management proxies such as CPA Manager
	// Plus pass those through but not CPA's plugin config API): it sends the
	// file here with promote or restore, PUTs the answer, then adopts.

	// Promote answers with ConfigYAML whose plugin section makes the runtime
	// config permanent: the current section with its routing keys replaced
	// by the runtime config. The current section is kept as a backup.
	Promote *struct {
		ConfigYAML string `json:"config_yaml"`
	} `json:"promote"`
	// Restore answers with ConfigYAML whose plugin section is the backup
	// taken at At. The current section is kept as a backup.
	Restore *struct {
		At         time.Time `json:"at"`
		ConfigYAML string    `json:"config_yaml"`
	} `json:"restore"`
	// Adopt installs a section just written to config.yaml without waiting
	// for CPA to pass it on. DropOverlay also ends the runtime config, which
	// the new section now holds.
	Adopt *struct {
		Section     json.RawMessage `json:"section"`
		DropOverlay bool            `json:"drop_overlay"`
	} `json:"adopt"`
}

// UpdateConfig applies a browser edit. The runtime config persists in the
// state file and wins over config.yaml until reverted.
func (r *Router) UpdateConfig(body []byte) (ConfigView, error) {
	var req configRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return ConfigView{}, fmt.Errorf("decode body: %w", err)
	}
	switch {
	case req.Promote != nil:
		return r.promoteConfig(req.Promote.ConfigYAML)
	case req.Restore != nil:
		return r.restoreConfig(req.Restore.At, req.Restore.ConfigYAML)
	case req.Adopt != nil:
		return r.adoptSection(req.Adopt.Section, req.Adopt.DropOverlay)
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

// sectionObject decodes a config.yaml section sent as a JSON object.
func sectionObject(raw json.RawMessage) (map[string]any, error) {
	obj := map[string]any{}
	if len(raw) == 0 || string(raw) == "null" {
		return obj, nil
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("config.yaml section must be a JSON object: %w", err)
	}
	return obj, nil
}

func (r *Router) backupSectionLocked(section json.RawMessage, note string) {
	r.configBackups = append(r.configBackups, configBackup{At: r.now(), Note: note, Section: append(json.RawMessage{}, section...)})
	if n := len(r.configBackups) - maxConfigBackups; n > 0 {
		r.configBackups = append([]configBackup{}, r.configBackups[n:]...)
	}
	r.dirty = true
}

// promoteConfig makes the runtime config permanent in configYAML and backs
// up the section it replaces. Keys the runtime config does not own
// (enabled, priority, mode, state_file, refresh_interval) are kept.
func (r *Router) promoteConfig(configYAML string) (ConfigView, error) {
	_, current, err := spliceSection(configYAML, nil)
	if err != nil {
		return ConfigView{}, err
	}
	obj, err := sectionObject(current)
	if err != nil {
		return ConfigView{}, err
	}
	r.mu.Lock()
	if strings.TrimSpace(r.overlay) == "" || r.overlayErr != "" {
		r.mu.Unlock()
		return ConfigView{}, fmt.Errorf("no runtime config in effect to make permanent")
	}
	doc, _ := json.Marshal(r.cfg.Doc())
	r.mu.Unlock()
	var routing map[string]any
	if err := json.Unmarshal(doc, &routing); err != nil {
		return ConfigView{}, err
	}
	for k, v := range routing {
		obj[k] = v
	}
	section, err := json.Marshal(obj)
	if err != nil {
		return ConfigView{}, err
	}
	return r.replaceSection(configYAML, current, section, "before making the runtime config permanent")
}

// restoreConfig puts the backup taken at `at` back into configYAML and backs
// up the section it replaces.
func (r *Router) restoreConfig(at time.Time, configYAML string) (ConfigView, error) {
	r.mu.Lock()
	var section json.RawMessage
	for _, b := range r.configBackups {
		if b.At.Equal(at) {
			section = b.Section
		}
	}
	r.mu.Unlock()
	if section == nil {
		return ConfigView{}, fmt.Errorf("no backup from %s", at.Format(time.RFC3339))
	}
	_, current, err := spliceSection(configYAML, nil)
	if err != nil {
		return ConfigView{}, err
	}
	return r.replaceSection(configYAML, current, section, "before restoring the backup from "+at.UTC().Format(time.RFC3339))
}

// replaceSection validates section, backs up current and answers with
// configYAML holding section.
func (r *Router) replaceSection(configYAML string, current, section json.RawMessage, note string) (ConfigView, error) {
	if _, err := ParseConfig(section); err != nil {
		return ConfigView{}, fmt.Errorf("the new config.yaml section would be invalid: %w", err)
	}
	out, _, err := spliceSection(configYAML, section)
	if err != nil {
		return ConfigView{}, err
	}
	r.mu.Lock()
	r.backupSectionLocked(current, note)
	r.mu.Unlock()
	r.saveState()
	v := r.ConfigView()
	v.ConfigYAML, v.Section = out, section
	return v, nil
}

// adoptSection installs a section the browser has written to config.yaml.
func (r *Router) adoptSection(section json.RawMessage, dropOverlay bool) (ConfigView, error) {
	if _, err := sectionObject(section); err != nil {
		return ConfigView{}, err
	}
	cfg, err := ParseConfig(section)
	if err != nil {
		return ConfigView{}, err
	}
	r.SetBaseConfig(section, cfg)
	if dropOverlay {
		r.mu.Lock()
		r.overlay, r.overlayAt, r.overlayNote = "", time.Time{}, ""
		r.applyOverlayLocked()
		r.dirty = true
		r.mu.Unlock()
	}
	r.saveState()
	if r.log != nil {
		r.log("info", "fefo config.yaml section adopted", map[string]any{"runtime_config_dropped": dropOverlay})
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
