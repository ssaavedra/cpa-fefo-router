package router

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Version is overridden at build time with -ldflags -X.
var Version = "0.1.0"

// AccountView is one account in the status report.
type AccountView struct {
	Account         string    `json:"account"`
	Provider        string    `json:"provider"`
	Buckets         []string  `json:"buckets,omitempty"`
	HomeClients     []string  `json:"home_clients,omitempty"`
	Usable          bool      `json:"usable"`
	Blocked         string    `json:"blocked,omitempty"`
	NoData          bool      `json:"no_data,omitempty"`
	WeeklyRemaining float64   `json:"weekly_remaining"`
	WeeklyReset     time.Time `json:"weekly_reset,omitzero"`
	ShortRemaining  float64   `json:"short_remaining"`
	ShortReset      time.Time `json:"short_reset,omitzero"`
	// Dollar figures are API-equivalent USD.
	CapacityUSD    float64       `json:"capacity_usd"`
	CapacitySource string        `json:"capacity_source"`
	CapacityWhy    string        `json:"capacity_why,omitempty"`
	CapacityLearn  capacityLearn `json:"capacity_learn"`
	RemainingUSD   float64       `json:"remaining_usd"`
	ExpectedUSD    *float64      `json:"expected_usd,omitempty"`
	SpareUSD       *float64      `json:"spare_usd,omitempty"`
	SpareWhy       string        `json:"spare_why,omitempty"`
	DataAge        string        `json:"data_age,omitempty"`
	Usage          *UsageView    `json:"usage_report,omitempty"`
	// Tokens served on the account in the last 24 hours and 7 days.
	Tokens24h int64 `json:"tokens_24h"`
	Tokens7d  int64 `json:"tokens_7d"`
}

// ClientView is one client in the status report.
type ClientView struct {
	Client  string                     `json:"client"`
	Buckets []string                   `json:"buckets"`
	Policy  string                     `json:"policy"`
	Borrow  bool                       `json:"borrow"`
	Demand  map[string]rateInfo        `json:"demand"`
	Plans   map[string][]PlanEntryView `json:"plans"`
	Gate    map[string]string          `json:"gate,omitempty"`
}

// PlanEntryView is one ranked account.
type PlanEntryView struct {
	Account string `json:"account"`
	Role    string `json:"role"`
	Why     string `json:"why"`
}

// Status is the management status document.
type Status struct {
	Plugin       string              `json:"plugin"`
	Version      string              `json:"version"`
	Now          time.Time           `json:"now"`
	Mode         Mode                `json:"mode"`
	SpareFactor  float64             `json:"spare_factor"`
	History      string              `json:"history"`
	HostSync     time.Time           `json:"host_sync,omitzero"`
	HostError    string              `json:"host_error,omitempty"`
	StateFile    string              `json:"state_file,omitempty"`
	StateError   string              `json:"state_error,omitempty"`
	Bindings     int                 `json:"session_bindings"`
	Overrides    map[string]Override `json:"overrides,omitempty"`
	Accounts     []AccountView       `json:"accounts"`
	Clients      []ClientView        `json:"clients"`
	Decisions    []Decision          `json:"recent_decisions"`
	UnknownKeys  []UnknownKey        `json:"unknown_keys,omitempty"`
	Warnings     []string            `json:"warnings,omitempty"`
	ConfigSource string              `json:"config_source"`
	Pricing      PricingView         `json:"pricing"`
	Flows        []Flow              `json:"flows"`
	// Projections project each provider's pool, spare lending included.
	Projections []ProjectionView `json:"projections"`
	// LendLimits are the bucket lending limits, per provider.
	LendLimits []LendLimitView `json:"lend_limits,omitempty"`
	// DisplayUnits is what Text shows: usd, tokens, pct_relative or
	// pct_absolute. JSON carries dollars and tokens.
	DisplayUnits string `json:"display_units"`
}

// UnknownKey is a client API key (as its caller scope) not in any client.
type UnknownKey struct {
	Alias    string    `json:"alias"`
	Scope    string    `json:"scope"`
	LastSeen time.Time `json:"last_seen"`
}

// Flow aggregates traffic from one client to one account since plugin load.
type Flow struct {
	Client   string         `json:"client"`
	Account  string         `json:"account"`
	Buckets  []string       `json:"buckets,omitempty"`
	Picks    map[string]int `json:"picks_by_role"`
	Requests int            `json:"requests"`
	Tokens   int64          `json:"tokens"`
	CostUSD  float64        `json:"cost_usd"`
}

func (r *Router) flowsLocked() []Flow {
	byKey := map[[2]string]*Flow{}
	get := func(c, a string) *Flow {
		k := [2]string{c, a}
		if f, ok := byKey[k]; ok {
			return f
		}
		f := &Flow{Client: c, Account: a, Picks: map[string]int{}}
		for _, st := range r.accounts {
			if st.Info.display() == a {
				f.Buckets = r.cfg.bucketsOf(st.Info)
			}
		}
		byKey[k] = f
		return f
	}
	for k, n := range r.metrics.picks {
		if k[1] == "" {
			continue
		}
		get(k[0], k[1]).Picks[k[2]] += int(n)
	}
	for k, n := range r.metrics.tokens {
		get(k[0], k[1]).Tokens += int64(n)
	}
	for k, n := range r.metrics.costUSD {
		get(k[0], k[1]).CostUSD += n
	}
	for k, n := range r.metrics.requests {
		get(k[0], k[1]).Requests += int(n)
	}
	out := make([]Flow, 0, len(byKey))
	for _, f := range byKey {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Client != out[j].Client {
			return out[i].Client < out[j].Client
		}
		return out[i].Account < out[j].Account
	})
	return out
}

// warningsLocked reports configuration that cannot work as intended.
func (r *Router) warningsLocked() []string {
	var out []string
	for _, name := range sortedKeys(r.cfg.Buckets) {
		if !r.hostReady {
			break // accounts are not all known until CPA's auth list arrives
		}
		for _, ident := range r.cfg.Buckets[name] {
			found := false
			for _, a := range r.accounts {
				found = found || a.Info.matches(ident)
			}
			if !found {
				out = append(out, fmt.Sprintf("bucket %s: %q matches no account CPA knows", name, ident))
			}
		}
	}
	for _, alias := range sortedKeys(r.cfg.Clients) {
		if len(r.cfg.Clients[alias].Keys) == 0 {
			out = append(out, fmt.Sprintf("client %s: no keys configured, it can never be recognised", alias))
		}
	}
	for _, m := range sortedKeys(r.metrics.unknown) {
		u := r.metrics.unknown[m]
		out = append(out, fmt.Sprintf("model %s is not in the price table: %d request(s) priced as the %s range, $%.2f–$%.2f in total; home clients are charged the high end, borrowers the low end, and capacity learning skips those hours. Add it under prices: or wait for the next sync",
			m, u.Requests, u.Family, u.Low, u.High))
	}
	return out
}

// tokensLocked sums the tokens served on an account over the last window.
func (r *Router) tokensLocked(id string, now time.Time, window time.Duration) int64 {
	from := hourOf(now.Add(-window)) + 1
	var n int64
	for h, b := range r.hours[id] {
		if h >= from {
			for _, t := range b.Tokens {
				n += t
			}
		}
	}
	return n
}

// Status builds the diagnostic snapshot.
func (r *Router) Status(decisions int) Status {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg := r.cfg
	st := Status{
		Plugin: PluginName, Version: Version, Now: now, Mode: cfg.Mode,
		SpareFactor: r.spareFactorLocked(now),
		History:     fmtDur(now.Sub(r.observeStart)) + " (min " + fmtDur(cfg.MinHistory) + ")",
		HostSync:    r.hostSyncAt, HostError: r.hostErr,
		StateFile: cfg.StateFile, StateError: r.stateErr,
		Bindings:  len(r.bindings),
		Overrides: map[string]Override{},
		Warnings:  r.warningsLocked(),
		Pricing:   r.pricingViewLocked(),

		DisplayUnits: cfg.DisplayUnits,
	}
	for k, o := range r.overrides {
		if o.active(now) {
			st.Overrides[k] = o
		}
	}
	providers := map[string]bool{}
	for _, a := range r.accounts {
		if a.Info.Provider != "" {
			providers[a.Info.Provider] = true
		}
	}
	pcs := map[string]*planContext{}
	for _, p := range sortedKeys(providers) {
		pc := r.newPlanContextLocked(p, now)
		pcs[p] = pc
		for _, id := range pc.order {
			e := pc.evals[id]
			av := AccountView{
				Account: e.Name, Provider: e.Provider, Buckets: e.Buckets, HomeClients: e.Home,
				Usable: e.Usable, Blocked: e.Block, NoData: e.NoData,
				WeeklyRemaining: e.WeeklyRemaining, WeeklyReset: e.WeeklyReset,
				ShortRemaining: e.ShortRemaining, ShortReset: e.ShortReset,
				CapacityUSD: e.Capacity.USD, CapacitySource: e.Capacity.Source, CapacityWhy: e.Capacity.Why,
				CapacityLearn: r.accounts[id].Learn, RemainingUSD: e.remaining(), SpareWhy: e.SpareWhy,
				Usage:     r.usageViewLocked(r.accounts[id], e.Capacity.USD, now),
				Tokens24h: r.tokensLocked(id, now, 24*time.Hour), Tokens7d: r.tokensLocked(id, now, 7*24*time.Hour),
			}
			if !math.IsInf(e.Expected, 0) {
				x, s := e.Expected, e.Spare
				av.ExpectedUSD, av.SpareUSD = &x, &s
			}
			if a := r.accounts[id]; a.HasSnapshot {
				av.DataAge = fmtDur(now.Sub(a.Snapshot.ObservedAt))
			}
			st.Accounts = append(st.Accounts, av)
		}
	}
	clients := sortedKeys(cfg.Clients)
	for _, scope := range sortedKeys(r.unknownScopes) {
		if _, known := r.lookupScopeLocked(scope); known {
			delete(r.unknownScopes, scope) // assigned since
			continue
		}
		st.UnknownKeys = append(st.UnknownKeys, UnknownKey{Alias: unknownAlias(scope), Scope: scope, LastSeen: r.unknownScopes[scope]})
	}
	st.ConfigSource = "config.yaml"
	if strings.TrimSpace(r.overlay) != "" && r.overlayErr == "" {
		st.ConfigSource = "runtime (edited " + fmtTime(r.overlayAt) + ")"
	}
	if r.overlayErr != "" {
		st.Warnings = append(st.Warnings, r.overlayErr)
	}
	st.Flows = r.flowsLocked()
	unknownAliases := []string{}
	for _, u := range st.UnknownKeys {
		unknownAliases = append(unknownAliases, u.Alias)
	}
	senders := append(append([]string{}, clients...), unknownAliases...)
	for _, c := range senders {
		cc := r.clientConfigLocked(c)
		pol, src := r.policyLocked(c, now)
		cv := ClientView{Client: c, Buckets: cc.Buckets, Policy: string(pol) + " (" + src + ")", Borrow: cc.BorrowEnabled(),
			Demand: map[string]rateInfo{}, Plans: map[string][]PlanEntryView{}}
		for p, pc := range pcs {
			cv.Demand[p] = pc.rate(c, "")
			order, _, _ := pc.plan(c, cfg.MinSpare)
			for _, e := range order {
				cv.Plans[p] = append(cv.Plans[p], PlanEntryView{Account: pc.evals[e.ID].Name, Role: e.Role, Why: e.Why})
			}
		}
		st.Clients = append(st.Clients, cv)
	}
	for _, p := range sortedKeys(pcs) {
		st.Projections = append(st.Projections, pcs[p].projection(senders))
		st.LendLimits = append(st.LendLimits, pcs[p].lendLimitViews()...)
	}
	n := len(r.decisions)
	if decisions > n {
		decisions = n
	}
	st.Decisions = append([]Decision{}, r.decisions[n-decisions:]...)
	if st.Accounts == nil {
		st.Accounts = []AccountView{}
	}
	if st.Clients == nil {
		st.Clients = []ClientView{}
	}
	if st.Projections == nil {
		st.Projections = []ProjectionView{}
	}
	return st
}

// Text renders the status for humans, in s.DisplayUnits.
func (s Status) Text() string {
	tokens := s.DisplayUnits == UnitsTokens
	pct := s.DisplayUnits == UnitsPctRelative || s.DisplayUnits == UnitsPctAbsolute
	largest, pool := map[string]float64{}, map[string]float64{}
	for _, a := range s.Accounts {
		largest[a.Provider] = math.Max(largest[a.Provider], a.CapacityUSD)
		pool[a.Provider] += a.CapacityUSD
	}
	// ref is the week a percent is of: the thing's own (own dollars) under
	// pct_relative, the provider's largest account under pct_absolute.
	ref := func(provider string, own float64) float64 {
		if s.DisplayUnits == UnitsPctAbsolute {
			return largest[provider]
		}
		return own
	}
	share := func(usd, of float64) string {
		if of <= 0 {
			return "?"
		}
		return fmt.Sprintf("%.0f%%", usd/of*100)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s  mode=%s  spare_factor=%.2f  history=%s  sessions=%d\n", s.Plugin, s.Version, s.Mode, s.SpareFactor, s.History, s.Bindings)
	if s.HostError != "" {
		fmt.Fprintf(&b, "host sync error: %s\n", s.HostError)
	}
	if s.StateError != "" {
		fmt.Fprintf(&b, "state file error: %s\n", s.StateError)
	}
	fmt.Fprintf(&b, "prices: %s, %d models", s.Pricing.Source, s.Pricing.Models)
	if !s.Pricing.SyncedAt.IsZero() {
		fmt.Fprintf(&b, ", synced %s", s.Pricing.SyncedAt.UTC().Format(time.RFC3339))
	}
	if s.Pricing.Overrides > 0 {
		fmt.Fprintf(&b, ", %d overridden in config", s.Pricing.Overrides)
	}
	b.WriteString("\n")
	if s.Pricing.SyncError != "" {
		fmt.Fprintf(&b, "price sync error: %s\n", s.Pricing.SyncError)
	}
	for _, w := range s.Warnings {
		fmt.Fprintf(&b, "WARNING: %s\n", w)
	}
	for k, o := range s.Overrides {
		fmt.Fprintf(&b, "override %s: policy=%s spare_factor=%g until %s %s\n", k, o.Policy, o.SpareFactor, fmtTime(o.Until), o.Note)
	}
	if len(s.Projections) > 0 {
		b.WriteString("\nPROJECTION (all accounts as one pool, spare lent, soonest reset first)\n")
		for _, p := range s.Projections {
			b.WriteString("  " + p.text(s.DisplayUnits, largest[p.Provider]) + "\n")
		}
	}
	if len(s.LendLimits) > 0 {
		b.WriteString("\nLEND LIMITS (what a bucket may lend this week)\n")
		for _, l := range s.LendLimits {
			amt := func(usd float64) string { return fmt.Sprintf("$%.2f", usd) }
			if pct || tokens {
				amt = func(usd float64) string { return share(usd, ref(l.Provider, l.CapacityUSD)) }
			}
			state := "open"
			if !l.Open {
				state = "REACHED"
			}
			to := "every other bucket"
			if l.Target != LendAny {
				to = "bucket " + l.Target
			}
			fmt.Fprintf(&b, "  %s → %s (%s): lent %s of %s allowed now (%.0f%% of the bucket's %s week), %s\n",
				l.Bucket, to, l.Provider, amt(l.LentUSD), amt(l.AllowedUSD), l.ShareNow*100, amt(l.CapacityUSD), state)
		}
	}
	b.WriteString("\nACCOUNTS\n")
	switch s.DisplayUnits {
	case UnitsPctRelative:
		b.WriteString("  (amounts as % of each account's own week)\n")
	case UnitsPctAbsolute:
		b.WriteString("  (amounts as % of the provider's largest account's week)\n")
	}
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	if tokens {
		fmt.Fprintln(tw, "  account\tprovider\tbuckets\tweekly\tresets\t5h\tstate\ttokens 24h\ttokens 7d\tspare (of week)\tdata age")
	} else {
		fmt.Fprintln(tw, "  account\tprovider\tbuckets\tweekly\tresets\t5h\tstate\tcapacity\tremaining\texpected\tspare\tdata age")
	}
	for _, a := range s.Accounts {
		state := "usable"
		if !a.Usable {
			state = "BLOCKED: " + a.Blocked
		} else if a.NoData {
			state = "usable (no data)"
		}
		capa, rem := fmt.Sprintf("$%.2f", a.CapacityUSD), fmt.Sprintf("$%.2f", a.RemainingUSD)
		of := ref(a.Provider, a.CapacityUSD)
		if pct {
			capa, rem = share(a.CapacityUSD, of), share(a.RemainingUSD, of)
		}
		exp, spare, spareShare := "?", "?", "?"
		if a.ExpectedUSD != nil {
			exp, spare = fmt.Sprintf("$%.2f", *a.ExpectedUSD), fmt.Sprintf("%+.2f", *a.SpareUSD)
			spare = spare[:1] + "$" + spare[1:]
			if a.CapacityUSD > 0 {
				spareShare = fmt.Sprintf("%+.0f%%", *a.SpareUSD/a.CapacityUSD*100)
			}
			if pct {
				exp, spare = share(*a.ExpectedUSD, of), "?"
				if of > 0 {
					spare = fmt.Sprintf("%+.0f%%", *a.SpareUSD/of*100)
				}
			}
		}
		resets := "?"
		if !a.WeeklyReset.IsZero() {
			resets = fmtTime(a.WeeklyReset) + " (" + fmtDur(time.Until(a.WeeklyReset)) + ")"
			if !s.Now.IsZero() {
				resets = fmtTime(a.WeeklyReset) + " (" + fmtDur(a.WeeklyReset.Sub(s.Now)) + ")"
			}
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%.0f%%\t%s\t%.0f%%\t%s\t", a.Account, a.Provider, strings.Join(a.Buckets, ","),
			a.WeeklyRemaining*100, resets, a.ShortRemaining*100, state)
		if tokens {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", fmtTokens(float64(a.Tokens24h)), fmtTokens(float64(a.Tokens7d)), spareShare, a.DataAge)
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", capa, rem, exp, spare, a.DataAge)
		}
	}
	tw.Flush()
	for _, a := range s.Accounts {
		fmt.Fprintf(&b, "  · %s: capacity $%.2f/week, %s (%s)\n", a.Account, a.CapacityUSD, a.CapacitySource, a.CapacityWhy)
		if a.SpareWhy != "" {
			fmt.Fprintf(&b, "  · %s: %s\n", a.Account, a.SpareWhy)
		}
		if u := a.Usage; u != nil {
			if u.PolledAt.IsZero() {
				fmt.Fprintf(&b, "  · %s: no usage report yet", a.Account)
			} else {
				fmt.Fprintf(&b, "  · %s: usage report %s", a.Account, fmtTime(u.PolledAt))
			}
			if u.ExtraUsageEnabled != nil {
				fmt.Fprintf(&b, ", extra usage %s", map[bool]string{true: "ENABLED", false: "off"}[*u.ExtraUsageEnabled])
			}
			for _, sw := range u.Scoped {
				if sw.Allowance > 0 {
					fmt.Fprintf(&b, ", %s ~%.0f%% of its %.0f%% allowance left (polled %.0f%%, $%.2f since)", sw.Family, sw.EstimatedRemaining*100, sw.Allowance*100, sw.PolledRemaining*100, sw.SpentSinceUSD)
				}
				if sw.Blocked != "" {
					fmt.Fprintf(&b, ", %s BLOCKED: %s", sw.Family, sw.Blocked)
				}
			}
			if u.Error != "" {
				fmt.Fprintf(&b, ", poll error: %s", u.Error)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\nCLIENTS\n")
	for _, c := range s.Clients {
		fmt.Fprintf(&b, "  %s  buckets=[%s]  policy=%s  borrow=%v\n", c.Client, strings.Join(c.Buckets, ","), c.Policy, c.Borrow)
		for _, p := range sortedKeys(c.Plans) {
			d := c.Demand[p]
			dem := "demand unknown: " + d.Why
			switch {
			case d.Known && tokens:
				dem = fmt.Sprintf("demand %s tok/h (24h %s, 7d %s)", fmtTokens(d.TokensPerHour), fmtTokens(d.TokensDay), fmtTokens(d.TokensWeek))
			case d.Known && pct:
				// A week of demand against the client's own accounts (the pool
				// when it has none), or against the largest account.
				own, whose := 0.0, "own accounts'"
				for _, a := range s.Accounts {
					if a.Provider == p && len(intersect(a.Buckets, c.Buckets)) > 0 {
						own += a.CapacityUSD
					}
				}
				if own <= 0 {
					own, whose = pool[p], "pool's"
				}
				if s.DisplayUnits == UnitsPctAbsolute {
					whose = "largest account's"
				}
				of, week := ref(p, own), 7*24.0
				dem = fmt.Sprintf("demand %s of the %s week per week (24h %s, 7d %s)", share(d.PerHour*week, of), whose, share(d.Day*week, of), share(d.Week*week, of))
			case d.Known:
				dem = fmt.Sprintf("demand $%.2f/h (24h $%.2f, 7d $%.2f)", d.PerHour, d.Day, d.Week)
			}
			fmt.Fprintf(&b, "    %s: %s\n", p, dem)
			for i, e := range c.Plans[p] {
				fmt.Fprintf(&b, "      %d. [%s] %s — %s\n", i+1, e.Role, e.Account, e.Why)
			}
			if len(c.Plans[p]) == 0 {
				b.WriteString("      (no usable account)\n")
			}
		}
	}
	b.WriteString("\nRECENT DECISIONS\n")
	for _, d := range s.Decisions {
		fmt.Fprintf(&b, "  %s %-14s %-6s session=%-6s %-9s → %s [%s] %s\n", d.Time.UTC().Format("15:04:05"), d.Client, d.Provider, d.Session, d.Outcome, d.Account, d.Role, d.Reason)
	}
	return b.String()
}

func fmtTokens(n float64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.2fG", n/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.2fM", n/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1fk", n/1e3)
	}
	return fmt.Sprintf("%.0f", n)
}

// overrideRequest is the body of POST /fefo/override.
type overrideRequest struct {
	Client      string  `json:"client"`
	Policy      string  `json:"policy"`
	SpareFactor float64 `json:"spare_factor"`
	For         string  `json:"for"`
	Note        string  `json:"note"`
	Clear       bool    `json:"clear"`
}

// ApplyOverride sets or clears a runtime override.
func (r *Router) ApplyOverride(body []byte) (Override, error) {
	var req overrideRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return Override{}, fmt.Errorf("decode body: %w", err)
	}
	client := strings.TrimSpace(req.Client)
	if client == "" {
		client = "*"
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if client != "*" {
		if _, ok := r.cfg.Clients[client]; !ok && !strings.HasPrefix(client, "key:") {
			return Override{}, fmt.Errorf("unknown client %q (known: %s)", client, strings.Join(sortedKeys(r.cfg.Clients), ", "))
		}
	}
	if req.Clear {
		delete(r.overrides, client)
		r.dirty = true
		return Override{}, nil
	}
	o := Override{}
	if prev, ok := r.overrides[client]; ok && prev.active(now) {
		o = prev // merge: setting one field keeps the other
	}
	o.SetAt, o.Note = now, req.Note
	if p := Policy(strings.ToLower(strings.TrimSpace(req.Policy))); p != "" {
		switch p {
		case PolicyAny, PolicyFail, PolicyWait:
			o.Policy = p
		default:
			return Override{}, fmt.Errorf("policy: want any, fail or wait")
		}
	}
	if req.SpareFactor != 0 {
		if client != "*" {
			return Override{}, fmt.Errorf("spare_factor overrides are global; omit client")
		}
		if req.SpareFactor < 1 {
			return Override{}, fmt.Errorf("spare_factor must be >= 1")
		}
		o.SpareFactor = req.SpareFactor
	}
	if o.Policy == "" && o.SpareFactor == 0 {
		return Override{}, fmt.Errorf("set policy and/or spare_factor, or clear")
	}
	if req.For != "" {
		d, err := time.ParseDuration(req.For)
		if err != nil || d <= 0 {
			return Override{}, fmt.Errorf("for: want a positive duration like 2h")
		}
		o.Until = now.Add(d)
	}
	r.overrides[client] = o
	r.dirty = true
	return o, nil
}
