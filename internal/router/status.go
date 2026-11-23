package router

import (
	"encoding/json"
	"fmt"
	"math"
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
	Capacity        float64   `json:"capacity"`
	Expected        *float64  `json:"expected_home_usage,omitempty"`
	Spare           *float64  `json:"spare,omitempty"`
	SpareWhy        string    `json:"spare_why,omitempty"`
	DataAge         string    `json:"data_age,omitempty"`
}

// ClientView is one client in the status report.
type ClientView struct {
	Client  string                       `json:"client"`
	Buckets []string                     `json:"buckets"`
	Policy  string                       `json:"policy"`
	Borrow  bool                         `json:"borrow"`
	Demand  map[string]rateInfo          `json:"demand"`
	Plans   map[string][]PlanEntryView   `json:"plans"`
	Gate    map[string]string            `json:"gate,omitempty"`
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
	UnknownKeys  []string            `json:"unknown_keys_seen,omitempty"`
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
				Capacity: e.Capacity, SpareWhy: e.SpareWhy,
			}
			if !math.IsInf(e.Expected, 0) {
				x, s := e.Expected, e.Spare
				av.Expected, av.Spare = &x, &s
			}
			if a := r.accounts[id]; a.HasSnapshot {
				av.DataAge = fmtDur(now.Sub(a.Snapshot.ObservedAt))
			}
			st.Accounts = append(st.Accounts, av)
		}
	}
	clients := sortedKeys(cfg.Clients)
	unknown := map[string]bool{}
	for _, d := range r.decisions {
		if _, ok := cfg.Clients[d.Client]; !ok {
			unknown[d.Client] = true
		}
	}
	st.UnknownKeys = sortedKeys(unknown)
	for _, c := range append(clients, st.UnknownKeys...) {
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
	n := len(r.decisions)
	if decisions > n {
		decisions = n
	}
	st.Decisions = append([]Decision(nil), r.decisions[n-decisions:]...)
	return st
}

// Text renders the status for humans.
func (s Status) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s  mode=%s  spare_factor=%.2f  history=%s  sessions=%d\n", s.Plugin, s.Version, s.Mode, s.SpareFactor, s.History, s.Bindings)
	if s.HostError != "" {
		fmt.Fprintf(&b, "host sync error: %s\n", s.HostError)
	}
	if s.StateError != "" {
		fmt.Fprintf(&b, "state file error: %s\n", s.StateError)
	}
	for k, o := range s.Overrides {
		fmt.Fprintf(&b, "override %s: policy=%s spare_factor=%g until %s %s\n", k, o.Policy, o.SpareFactor, fmtTime(o.Until), o.Note)
	}
	b.WriteString("\nACCOUNTS\n")
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "  account\tprovider\tbuckets\tweekly\tresets\t5h\tstate\texpected\tspare\tdata age")
	for _, a := range s.Accounts {
		state := "usable"
		if !a.Usable {
			state = "BLOCKED: " + a.Blocked
		} else if a.NoData {
			state = "usable (no data)"
		}
		exp, spare := "?", "?"
		if a.Expected != nil {
			exp, spare = fmt.Sprintf("%.2f", *a.Expected), fmt.Sprintf("%+.2f", *a.Spare)
		}
		resets := "?"
		if !a.WeeklyReset.IsZero() {
			resets = fmtTime(a.WeeklyReset) + " (" + fmtDur(time.Until(a.WeeklyReset)) + ")"
			if !s.Now.IsZero() {
				resets = fmtTime(a.WeeklyReset) + " (" + fmtDur(a.WeeklyReset.Sub(s.Now)) + ")"
			}
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%.0f%%\t%s\t%.0f%%\t%s\t%s\t%s\t%s\n", a.Account, a.Provider, strings.Join(a.Buckets, ","),
			a.WeeklyRemaining*100, resets, a.ShortRemaining*100, state, exp, spare, a.DataAge)
	}
	tw.Flush()
	for _, a := range s.Accounts {
		if a.SpareWhy != "" {
			fmt.Fprintf(&b, "  · %s: %s\n", a.Account, a.SpareWhy)
		}
	}
	b.WriteString("\nCLIENTS\n")
	for _, c := range s.Clients {
		fmt.Fprintf(&b, "  %s  buckets=[%s]  policy=%s  borrow=%v\n", c.Client, strings.Join(c.Buckets, ","), c.Policy, c.Borrow)
		for _, p := range sortedKeys(c.Plans) {
			d := c.Demand[p]
			dem := "demand unknown: " + d.Why
			if d.Known {
				dem = fmt.Sprintf("demand %.4f plan-weeks/h (24h %.4f, 7d %.4f)", d.PerHour, d.Day, d.Week)
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

// overrideRequest is the body of POST /burndown/override.
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
