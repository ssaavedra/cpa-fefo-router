// Package router implements bucket-aware, reset-aware credential selection
// for the CLIProxyAPI scheduler capability.
package router

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/ssaavedra/cpa-fefo-router/internal/pricing"
	"github.com/ssaavedra/cpa-fefo-router/internal/quota"
)

// LogFunc receives one structured diagnostic line.
type LogFunc func(level, msg string, fields map[string]any)

// Override is a temporary runtime change set through the management API.
type Override struct {
	Policy      Policy    `json:"policy,omitempty"`
	SpareFactor float64   `json:"spare_factor,omitempty"`
	Until       time.Time `json:"until,omitzero"`
	SetAt       time.Time `json:"set_at"`
	Note        string    `json:"note,omitempty"`
}

func (o Override) active(now time.Time) bool { return o.Until.IsZero() || now.Before(o.Until) }

type binding struct {
	AuthID  string
	Expires time.Time
}

// Router holds configuration, quota data, usage history and bindings.
type Router struct {
	registry *quota.Registry
	now      func() time.Time
	log      LogFunc
	lister   AuthLister
	sleep    func(time.Duration)

	mu  sync.Mutex
	cfg Config // effective: config.yaml, or the runtime overlay when set
	// config.yaml section and the runtime overlay edited from the browser.
	base        Config
	baseRaw     string
	overlay     string
	overlayAt   time.Time
	overlayNote string
	overlayErr  string
	// Caller scopes seen that map to no configured client.
	unknownScopes map[string]time.Time

	fetcher    HostFetcher
	prices     pricing.Table // effective: overrides > synced > embedded
	synced     pricing.Table // last models.dev sync
	pricingErr string
	// pricingTriedAt is the last sync attempt, successful or not.
	pricingTriedAt time.Time
	accounts       map[string]*accountState
	hours          map[string]map[int64]*hourBucket
	observeStart   time.Time
	scopes         map[string]string // caller scope -> client alias
	overrides      map[string]Override
	bindings       map[string]binding
	decisions      []Decision
	metrics        metrics
	dirty          bool
	hostSyncAt     time.Time
	hostReady      bool // a host auth list with IDs was applied
	hostErr        string
	stateErr       string
	loadedFrom     string

	refreshMu   sync.Mutex
	refreshStop chan struct{}
	refreshDone chan struct{}
}

// Option customizes a Router.
type Option func(*Router)

// WithClock overrides time.Now, for tests.
func WithClock(now func() time.Time) Option { return func(r *Router) { r.now = now } }

// WithSleep overrides time.Sleep, for tests.
func WithSleep(sleep func(time.Duration)) Option { return func(r *Router) { r.sleep = sleep } }

// WithLogger sets the diagnostic sink.
func WithLogger(log LogFunc) Option { return func(r *Router) { r.log = log } }

// WithAuthLister enables the periodic host auth refresh.
func WithAuthLister(l AuthLister) Option { return func(r *Router) { r.lister = l } }

// HostFetcher reads credentials and sends HTTP requests through the host.
// Its calls block on the C ABI: never call it while holding r.mu.
type HostFetcher interface {
	AuthJSON(ctx context.Context, authIndex string) (json.RawMessage, error)
	HTTPDo(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

// WithFetcher enables price syncing and Claude usage polling.
func WithFetcher(f HostFetcher) Option { return func(r *Router) { r.fetcher = f } }

// New returns a router with the default config.
func New(opts ...Option) *Router {
	r := &Router{
		registry:  quota.DefaultRegistry(),
		now:       time.Now,
		sleep:     time.Sleep,
		cfg:       DefaultConfig(),
		accounts:  map[string]*accountState{},
		hours:     map[string]map[int64]*hourBucket{},
		scopes:    map[string]string{},
		overrides: map[string]Override{},
		bindings:  map[string]binding{},
		metrics:   newMetrics(),
		prices:    embeddedPrices,

		unknownScopes: map[string]time.Time{},
	}
	r.base = r.cfg
	for _, opt := range opts {
		opt(r)
	}
	r.observeStart = r.now()
	return r
}

// Config returns the active configuration.
func (r *Router) Config() Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg
}

// SetConfig swaps the configuration. The first config naming a state file
// loads it.
func (r *Router) SetConfig(cfg Config) {
	r.mu.Lock()
	changedInterval := r.cfg.RefreshInterval != cfg.RefreshInterval
	r.base = cfg
	r.cfg = cfg
	load := cfg.StateFile != "" && r.loadedFrom != cfg.StateFile
	r.mu.Unlock()
	if load {
		r.loadState(cfg.StateFile)
	}
	r.mu.Lock()
	r.applyOverlayLocked()
	r.mu.Unlock()
	if changedInterval && r.refreshing() {
		r.StopRefresher()
		r.StartRefresher()
	}
}

// --- observation -----------------------------------------------------------

// ObserveUsage records one usage record: quota headers, usage deltas and the
// client that caused them.
func (r *Router) ObserveUsage(rec pluginapi.UsageRecord) {
	authID := strings.TrimSpace(rec.AuthID)
	if authID == "" {
		return
	}
	// The usage record is published when the response completes, so its
	// headers describe the account as of now.
	at := r.now()
	snap, hasQuota := r.registry.Parse(rec.Provider, rec.ResponseHeaders, at)

	usage := pricing.NormalizeUsage(rec.Provider, rec.Detail)
	model := firstNonEmpty(rec.ResponseModel, rec.Model)

	r.mu.Lock()
	defer r.mu.Unlock()
	client := r.clientForKeyLocked(rec.APIKey)
	st := r.accountLocked(authID, rec.Provider)
	tokens := rec.Detail.TotalTokens
	if tokens <= 0 {
		tokens = rec.Detail.InputTokens + rec.Detail.OutputTokens + rec.Detail.ReasoningTokens
	}
	var cost pricing.Quote
	if usage != (pricing.Usage{}) {
		cost = r.prices.Cost(model, usage)
	}
	r.foldLocked(authID, at)
	b := r.bucketLocked(authID, at)
	if !rec.Failed {
		addTo(&b.Requests, client, 1)
		if tokens > 0 {
			addTo(&b.Tokens, client, tokens)
		}
		if cost.High > 0 {
			addTo(&b.USDLow, client, cost.Low)
			addTo(&b.USDHigh, client, cost.High)
			if cost.Match == pricing.Unknown {
				b.Tainted = true
				r.metrics.unpriced(strings.ToLower(model), cost)
			}
			if st.Usage.Report.HasReport {
				addTo(&st.Usage.SpendSince, scopeFamily(cost.Family), cost.High)
			}
		}
	}
	r.metrics.usage(client, st.Info.display(), tokens, cost.High, rec.Failed)
	r.dirty = true
	if !hasQuota {
		return
	}
	for i := range snap.Windows {
		if snap.Windows[i].ObservedAt.IsZero() {
			snap.Windows[i].ObservedAt = at
		}
	}
	if st.HasSnapshot {
		st.Snapshot = st.Snapshot.Merge(snap)
	} else {
		st.Snapshot, st.HasSnapshot = snap, true
	}
	if len(snap.Windows) > 0 {
		st.ProbedAt = time.Time{}
	}
	for _, w := range snap.Windows {
		st.Recent = append(st.Recent, usedSample{At: at, Window: w.Name, Used: 1 - w.Remaining, Reset: w.ResetAt})
	}
	cut := at.Add(-recentRetention)
	for len(st.Recent) > 0 && st.Recent[0].At.Before(cut) {
		st.Recent = st.Recent[1:]
	}
	if w, ok := weeklyWindow(snap); ok {
		switch {
		case b.Cycle.IsZero():
			b.Cycle = w.ResetAt
		case !sameReset(b.Cycle, w.ResetAt):
			b.Tainted = true
			if w.ResetAt.After(b.Cycle) {
				b.Cycle = w.ResetAt
			}
		}
		used := 1 - w.Remaining
		cur := usedSample{At: at, Window: w.Name, Used: used, Reset: w.ResetAt}
		if prev := st.LastWeekly; prev != nil {
			delta := 0.0
			switch {
			case sameReset(prev.Reset, w.ResetAt) && used > prev.Used:
				delta = used - prev.Used
			case !sameReset(prev.Reset, w.ResetAt) && w.ResetAt.After(prev.Reset) && !prev.Reset.IsZero():
				// New weekly period: everything used so far happened after the reset.
				delta = used
			}
			if at.Sub(prev.At) > attributionGap {
				b.External += delta
			} else {
				b.Delta += delta
			}
		}
		st.LastWeekly = &cur
	}
}

// clientForKeyLocked maps a raw client API key to a client alias and teaches
// the scope map, so later scheduler picks (which only see the scope) resolve.
func (r *Router) clientForKeyLocked(rawKey string) string {
	if strings.TrimSpace(rawKey) == "" {
		return anonymousClient
	}
	scope := callerScope(rawKey)
	if alias := r.clientForScopeLocked(scope); !strings.HasPrefix(alias, "key:") {
		return alias
	}
	plain := plainSHA(rawKey)
	for alias, c := range r.cfg.Clients {
		for _, k := range c.Keys {
			if k == plain || k == scope {
				r.scopes[scope] = alias
				return alias
			}
		}
	}
	return unknownAlias(scope)
}

// clientForScopeLocked resolves a caller scope to a client alias.
func (r *Router) clientForScopeLocked(scope string) string {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		return anonymousClient
	}
	if alias, ok := r.lookupScopeLocked(scope); ok {
		return alias
	}
	if len(r.unknownScopes) < 1000 {
		r.unknownScopes[scope] = r.now()
	}
	return unknownAlias(scope)
}

// lookupScopeLocked finds the configured client of a caller scope.
func (r *Router) lookupScopeLocked(scope string) (string, bool) {
	for alias, c := range r.cfg.Clients {
		for _, k := range c.Keys {
			if k == scope {
				return alias, true
			}
		}
	}
	if alias, ok := r.scopes[scope]; ok {
		if _, still := r.cfg.Clients[alias]; still {
			return alias, true
		}
	}
	return "", false
}

func unknownAlias(scope string) string {
	if len(scope) > 10 {
		scope = scope[:10]
	}
	return "key:" + scope
}

// clientConfigLocked returns the policy for an alias.
func (r *Router) clientConfigLocked(alias string) ClientConfig {
	if c, ok := r.cfg.Clients[alias]; ok {
		return c
	}
	return r.cfg.DefaultClient
}

func (r *Router) accountLocked(id, provider string) *accountState {
	st, ok := r.accounts[id]
	if !ok {
		st = &accountState{Info: accountInfo{ID: id}}
		r.accounts[id] = st
	}
	if p := strings.ToLower(strings.TrimSpace(provider)); p != "" {
		st.Info.Provider = p
	}
	return st
}

func (r *Router) bucketLocked(authID string, at time.Time) *hourBucket {
	hs := r.hours[authID]
	if hs == nil {
		hs = map[int64]*hourBucket{}
		r.hours[authID] = hs
	}
	h := hourOf(at)
	b := hs[h]
	if b == nil {
		b = &hourBucket{}
		hs[h] = b
	}
	return b
}

func (r *Router) pruneLocked(now time.Time) {
	cut := hourOf(now.Add(-historyRetention))
	for _, hs := range r.hours {
		for h := range hs {
			if h < cut {
				delete(hs, h)
			}
		}
	}
	for k, b := range r.bindings {
		if now.After(b.Expires) {
			delete(r.bindings, k)
		}
	}
	for k, o := range r.overrides {
		if !o.active(now) {
			delete(r.overrides, k)
		}
	}
}

// --- host sync ------------------------------------------------------------

// AuthLister lists host credentials.
type AuthLister interface {
	ListAuths(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error)
}

// Refresh syncs labels, disabled flags and cooldowns from the host, prunes
// old data and saves state.
func (r *Router) Refresh(ctx context.Context) error {
	now := r.now()
	var err error
	if r.lister != nil {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		var entries []pluginapi.HostAuthFileEntry
		entries, err = r.lister.ListAuths(ctx)
		cancel()
		r.mu.Lock()
		if err != nil {
			r.hostErr = err.Error()
		} else {
			r.applyHostLocked(entries, now)
		}
		r.mu.Unlock()
	}
	r.syncPrices(ctx, now)
	r.pollUsage(ctx, now)
	r.mu.Lock()
	for id := range r.accounts {
		r.foldLocked(id, now)
	}
	r.pruneLocked(now)
	r.mu.Unlock()
	r.saveState()
	return err
}

func (r *Router) applyHostLocked(entries []pluginapi.HostAuthFileEntry, now time.Time) {
	withID := 0
	for _, e := range entries {
		if strings.TrimSpace(e.ID) != "" {
			withID++
		}
	}
	if withID == 0 && len(entries) > 0 {
		// At startup CPA answers from disk before its auth manager is ready;
		// those entries carry no IDs. Wait for the next sync.
		r.hostErr = "CPA auth list not ready yet (no credential IDs); retrying"
		return
	}
	r.hostErr = ""
	r.hostSyncAt = now
	r.hostReady = true
	for _, e := range entries {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			continue
		}
		st := r.accountLocked(id, firstNonEmpty(e.Provider, e.Type))
		if e.Label != "" {
			st.Info.Label = e.Label
		}
		if e.Name != "" {
			st.Info.Name = e.Name
		}
		st.Info.AuthIndex = strings.TrimSpace(e.AuthIndex)
		st.Disabled = e.Disabled || strings.EqualFold(e.Status, "disabled")
		st.NextRetryAfter = e.NextRetryAfter
	}
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// StartRefresher runs Refresh periodically.
func (r *Router) StartRefresher() {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	if r.refreshStop != nil {
		return
	}
	stop, done := make(chan struct{}), make(chan struct{})
	r.refreshStop, r.refreshDone = stop, done
	interval := r.Config().RefreshInterval
	go func() {
		defer close(done)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { <-stop; cancel() }()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if err := r.Refresh(ctx); err != nil && r.log != nil && ctx.Err() == nil {
				r.log("warn", "fefo-router refresh failed", map[string]any{"error": err.Error()})
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
}

// StopRefresher stops the refresher and saves state.
func (r *Router) StopRefresher() {
	r.refreshMu.Lock()
	stop, done := r.refreshStop, r.refreshDone
	r.refreshStop, r.refreshDone = nil, nil
	r.refreshMu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	r.saveState()
}

func (r *Router) refreshing() bool {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	return r.refreshStop != nil
}

// --- helpers --------------------------------------------------------------

// providerOfModel guesses the provider of a requested model name.
func providerOfModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	switch {
	case strings.Contains(m, "claude") || strings.HasPrefix(m, "opus") || strings.HasPrefix(m, "sonnet") || strings.HasPrefix(m, "haiku") || strings.HasPrefix(m, "fable"):
		return "claude"
	case strings.HasPrefix(m, "gpt") || strings.Contains(m, "codex") || strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4"):
		return "codex"
	}
	return ""
}

func headerFirst(h map[string][]string, name string) string {
	for k, vs := range h {
		if strings.EqualFold(k, name) {
			for _, v := range vs {
				if v = strings.TrimSpace(v); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
