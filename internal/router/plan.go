package router

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Roles of an account in a client's plan, in priority order.
const (
	RoleProbe      = "probe"       // no quota data yet: one request to learn it
	RoleSpareFirst = "spare-first" // another bucket's spare that expires before ours
	RoleOwn        = "own"         // one of the client's buckets
	RoleSpare      = "spare"       // another bucket's spare
	RoleLastResort = "last-resort" // anything usable, under the "any" policy
)

// eval is the routing view of one account at one instant.
type eval struct {
	ID       string
	Name     string
	Provider string
	APIKey   bool

	NoData       bool
	Usable       bool
	Block        string
	BlockedUntil time.Time

	WeeklyRemaining float64 // 0..1 of the weekly window
	WeeklyReset     time.Time
	ShortRemaining  float64 // 0..1 of the shortest window (5h)
	ShortReset      time.Time
	Capacity        capacityInfo // weekly window in API-equivalent dollars

	Buckets []string
	Home    []string // clients whose buckets include this account

	// Expected dollars of use by home clients (and outside CPA) until
	// WeeklyReset. +Inf when it cannot be estimated yet.
	Expected float64
	// Spare dollars: remaining − factor × Expected.
	Spare float64
	// SpareWhy explains Expected/Spare for diagnostics.
	SpareWhy string
}

// remaining is the dollars left in the weekly window.
func (e *eval) remaining() float64 { return e.WeeklyRemaining * e.Capacity.USD }

func (e *eval) hoursToReset(now time.Time) float64 {
	if e.WeeklyReset.IsZero() {
		return math.Inf(1)
	}
	return math.Max(0, e.WeeklyReset.Sub(now).Hours())
}

// planEntry is one account in a client's ordered plan.
type planEntry struct {
	ID   string
	Role string
	Why  string
}

// planContext caches per-provider evaluation for one decision.
type planContext struct {
	r        *Router
	cfg      Config
	now      time.Time
	provider string
	factor   float64
	evals    map[string]*eval
	order    []string // account IDs of this provider, stable order
	demand   map[string]rateInfo
}

// rateInfo is a client's demand. Dollars drive routing; tokens are display.
type rateInfo struct {
	Known   bool    `json:"known"`
	PerHour float64 `json:"usd_per_hour"` // max of the 24h and 7d averages
	Day     float64 `json:"usd_per_hour_24h"`
	Week    float64 `json:"usd_per_hour_7d"`
	// Tokens per hour over the same windows.
	TokensPerHour float64 `json:"tokens_per_hour"`
	TokensDay     float64 `json:"tokens_per_hour_24h"`
	TokensWeek    float64 `json:"tokens_per_hour_7d"`
	Why           string  `json:"why,omitempty"`
}

// spareFactorLocked applies a runtime override.
func (r *Router) spareFactorLocked(now time.Time) float64 {
	if o, ok := r.overrides["*"]; ok && o.active(now) && o.SpareFactor >= 1 {
		return o.SpareFactor
	}
	return r.cfg.SpareFactor
}

// policyLocked returns the effective exhaustion policy and where it came from.
func (r *Router) policyLocked(client string, now time.Time) (Policy, string) {
	if o, ok := r.overrides[client]; ok && o.active(now) && o.Policy != "" {
		return o.Policy, "override until " + fmtTime(o.Until)
	}
	if o, ok := r.overrides["*"]; ok && o.active(now) && o.Policy != "" {
		return o.Policy, "global override until " + fmtTime(o.Until)
	}
	return r.clientConfigLocked(client).OnExhausted, "config"
}

func (r *Router) newPlanContextLocked(provider string, now time.Time) *planContext {
	pc := &planContext{
		r: r, cfg: r.cfg, now: now, provider: provider,
		factor: r.spareFactorLocked(now),
		evals:  map[string]*eval{},
		demand: map[string]rateInfo{},
	}
	ids := make([]string, 0, len(r.accounts))
	for id, st := range r.accounts {
		if provider == "" || st.Info.Provider == provider {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	pc.order = ids
	type unit struct {
		usd float64
		why string
	}
	units := map[string]unit{}
	for _, id := range ids {
		st := r.accounts[id]
		u, ok := units[st.Info.Provider]
		if !ok {
			u.usd, u.why = r.unitUSDLocked(st.Info.Provider)
			units[st.Info.Provider] = u
		}
		pc.evals[id] = r.evaluateLocked(st, now, r.capacityLocked(st, u.usd, u.why))
	}
	for _, id := range ids {
		pc.expected(pc.evals[id])
	}
	return pc
}

// evaluateLocked classifies one account from its snapshot and host state.
func (r *Router) evaluateLocked(st *accountState, now time.Time, capa capacityInfo) *eval {
	cfg := r.cfg
	e := &eval{
		ID: st.Info.ID, Name: st.Info.display(), Provider: st.Info.Provider,
		APIKey:          strings.EqualFold(st.Info.AuthKind, "apikey"),
		WeeklyRemaining: 1, ShortRemaining: 1,
		Capacity: capa,
		Buckets:  cfg.bucketsOf(st.Info),
	}
	for _, alias := range sortedKeys(cfg.Clients) {
		for _, b := range cfg.Clients[alias].Buckets {
			if contains(e.Buckets, b) {
				e.Home = append(e.Home, alias)
				break
			}
		}
	}
	block := func(reason string, until time.Time) {
		if e.Block == "" {
			e.Block = reason
		} else {
			e.Block += "; " + reason
		}
		if until.After(e.BlockedUntil) {
			e.BlockedUntil = until
		}
	}
	if st.Disabled {
		block("disabled", time.Time{})
	}
	if st.NextRetryAfter.After(now) {
		block("CPA cooldown", st.NextRetryAfter)
	}
	if !st.HasSnapshot || len(st.Snapshot.Windows) == 0 {
		e.NoData = true
	}
	snap := st.Snapshot
	exhausted := false
	var shortLen time.Duration = -1
	for _, w := range snap.Windows {
		rem, reset := w.Remaining, w.ResetAt
		if !reset.IsZero() && !now.Before(reset) {
			// The window refilled since it was reported.
			rem = 1
			if w.Length > 0 {
				periods := now.Sub(reset)/w.Length + 1
				reset = reset.Add(periods * w.Length)
			} else {
				reset = time.Time{}
			}
		}
		weekly := w.Name == "7d" || w.Length >= 6*24*time.Hour
		if weekly {
			e.WeeklyRemaining, e.WeeklyReset = rem, reset
		} else if shortLen < 0 || w.Length < shortLen {
			shortLen = w.Length
			e.ShortRemaining, e.ShortReset = rem, reset
		}
		if rem <= 0 || rem < cfg.MinRemaining {
			exhausted = true
			until := reset
			if until.IsZero() {
				until = w.ObservedAt.Add(cfg.StaleAfter)
			}
			if until.After(now) {
				block(fmt.Sprintf("%s window %.0f%% left until %s", w.Name, rem*100, fmtTime(until)), until)
			}
		}
	}
	if snap.LimitReached && !snap.LimitObservedAt.IsZero() {
		until := snap.LimitUntil
		if until.IsZero() {
			until = snap.LimitObservedAt.Add(cfg.StaleAfter)
		}
		if until.After(now) {
			exhausted = true
			block("provider reported limit reached until "+fmtTime(until), until)
		}
	}
	if exhausted && cfg.AllowExtraUsage && snap.ExtraUsage && e.Block != "" && !st.Disabled && !st.NextRetryAfter.After(now) {
		e.Block = ""
		e.BlockedUntil = time.Time{}
		e.SpareWhy = "exhausted, serving from paid extra usage"
	}
	e.Usable = e.Block == ""
	return e
}

// rate computes a client's (or the external pseudo-client's) consumption on
// this provider's accounts in dollars per hour: its share of each hour's
// weekly delta times the account's capacity. onlyAccount restricts it to
// one account.
func (pc *planContext) rate(client, onlyAccount string) rateInfo {
	key := client + "|" + onlyAccount
	if ri, ok := pc.demand[key]; ok {
		return ri
	}
	r := pc.r
	observed := pc.now.Sub(r.observeStart)
	ri := rateInfo{}
	if observed < pc.cfg.MinHistory {
		ri.Why = fmt.Sprintf("only %s of history (need %s)", fmtDur(observed), fmtDur(pc.cfg.MinHistory))
		pc.demand[key] = ri
		return ri
	}
	nowH := hourOf(pc.now)
	sum := func(window time.Duration) (usd, tokens float64) {
		eff := math.Min(window.Hours(), observed.Hours())
		from := nowH - int64(math.Ceil(eff)) + 1
		for _, id := range pc.order {
			if onlyAccount != "" && id != onlyAccount {
				continue
			}
			e := pc.evals[id]
			home := map[string]bool{}
			for _, c := range e.Home {
				home[c] = true
			}
			for h, b := range r.hours[id] {
				if h >= from && h <= nowH {
					usd += b.split(home)[client] * e.Capacity.USD
					tokens += float64(b.Tokens[client])
				}
			}
		}
		hours := math.Max(eff, 1)
		return usd / hours, tokens / hours
	}
	ri.Known = true
	ri.Day, ri.TokensDay = sum(24 * time.Hour)
	ri.Week, ri.TokensWeek = sum(7 * 24 * time.Hour)
	ri.PerHour, ri.TokensPerHour = math.Max(ri.Day, ri.Week), math.Max(ri.TokensDay, ri.TokensWeek)
	pc.demand[key] = ri
	return ri
}

// ownList returns a client's own accounts of this provider in preference
// order: bucket order, then soonest weekly reset.
func (pc *planContext) ownList(cc ClientConfig) []*eval {
	seen := map[string]bool{}
	var out []*eval
	for _, b := range cc.Buckets {
		var group []*eval
		for _, id := range pc.order {
			e := pc.evals[id]
			if !seen[id] && contains(e.Buckets, b) {
				seen[id] = true
				group = append(group, e)
			}
		}
		sortByReset(group)
		out = append(out, group...)
	}
	return out
}

func sortByReset(es []*eval) {
	sort.SliceStable(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if a.WeeklyReset.IsZero() != b.WeeklyReset.IsZero() {
			return !a.WeeklyReset.IsZero()
		}
		if !a.WeeklyReset.Equal(b.WeeklyReset) {
			return a.WeeklyReset.Before(b.WeeklyReset)
		}
		return a.ID < b.ID
	})
}

// expected fills Expected and Spare: how much of the account its home clients
// (and usage outside CPA) will need before its weekly reset, walking each
// home client's preference list so earlier accounts absorb demand first.
func (pc *planContext) expected(e *eval) {
	if e.NoData || e.WeeklyReset.IsZero() {
		e.Expected, e.Spare = math.Inf(1), math.Inf(-1)
		e.SpareWhy = "weekly window not known yet"
		return
	}
	hours := e.hoursToReset(pc.now)
	ext := pc.rate(externalClient, e.ID)
	if !ext.Known {
		e.Expected, e.Spare = math.Inf(1), math.Inf(-1)
		e.SpareWhy = ext.Why
		return
	}
	total := ext.PerHour * hours
	parts := []string{}
	if total > 0 {
		parts = append(parts, fmt.Sprintf("outside CPA $%.2f", total))
	}
	for _, client := range e.Home {
		cc := pc.r.clientConfigLocked(client)
		d := pc.rate(client, "")
		if !d.Known {
			e.Expected, e.Spare = math.Inf(1), math.Inf(-1)
			e.SpareWhy = client + ": " + d.Why
			return
		}
		need := d.PerHour * hours
		supply := 0.0
		for _, b := range pc.ownList(cc) {
			if b.ID == e.ID {
				break
			}
			if !b.Usable || b.NoData {
				continue
			}
			s := b.remaining()
			if !b.WeeklyReset.IsZero() && b.WeeklyReset.Before(e.WeeklyReset) {
				s += b.Capacity.USD // refills within this account's window
			}
			supply += s
		}
		reaching := math.Max(0, need-supply)
		total += reaching
		parts = append(parts, fmt.Sprintf("%s $%.2f/h×%s→$%.2f", client, d.PerHour, fmtDur(time.Duration(hours*float64(time.Hour))), reaching))
	}
	e.Expected = total
	e.Spare = e.remaining() - pc.factor*total
	e.SpareWhy = fmt.Sprintf("left $%.2f − %.1f×expected $%.2f", e.remaining(), pc.factor, total)
	if len(parts) > 0 {
		e.SpareWhy += " (" + strings.Join(parts, ", ") + ")"
	}
}

// plan orders the accounts a client may use right now. minSpare is a share
// of each account's capacity.
func (pc *planContext) plan(client string, minSpare float64) ([]planEntry, Policy, string) {
	r := pc.r
	cc := r.clientConfigLocked(client)
	policy, policySrc := r.policyLocked(client, pc.now)
	own := pc.ownList(cc)
	isOwn := map[string]bool{}
	for _, e := range own {
		isOwn[e.ID] = true
	}

	var out []planEntry
	add := func(e *eval, role, why string) {
		for _, p := range out {
			if p.ID == e.ID {
				return
			}
		}
		out = append(out, planEntry{ID: e.ID, Role: role, Why: why})
	}

	// Probe accounts without data that are ours or unassigned.
	for _, id := range pc.order {
		e := pc.evals[id]
		if e.NoData && e.Block == "" && !e.APIKey && (isOwn[id] || len(e.Buckets) == 0) {
			st := r.accounts[id]
			if pc.now.Sub(st.ProbedAt) >= pc.cfg.ProbeInterval {
				add(e, RoleProbe, "no quota data yet")
			}
		}
	}

	var firstOwn *eval
	for _, e := range own {
		if e.Usable && !e.NoData {
			firstOwn = e
			break
		}
	}
	// Spare: quota nobody is expected to need before it resets. Includes the
	// client's own later buckets, so their expiring quota is burned too.
	var spare []*eval
	for _, id := range pc.order {
		e := pc.evals[id]
		if (firstOwn != nil && id == firstOwn.ID) || !e.Usable || e.NoData || e.APIKey {
			continue
		}
		if !isOwn[id] && !cc.BorrowEnabled() {
			continue
		}
		if e.Spare > minSpare*e.Capacity.USD {
			spare = append(spare, e)
		}
	}
	sortByReset(spare)
	for _, e := range spare {
		if firstOwn == nil || (!e.WeeklyReset.IsZero() && e.WeeklyReset.Before(firstOwn.WeeklyReset)) {
			whose := "other bucket"
			if isOwn[e.ID] {
				whose = "own later bucket"
			}
			add(e, RoleSpareFirst, fmt.Sprintf("%s spare $%.2f expires %s, before first own account", whose, e.Spare, fmtTime(e.WeeklyReset)))
		}
	}
	for i, e := range own {
		if !e.Usable {
			continue
		}
		why := fmt.Sprintf("bucket %s", strings.Join(intersect(e.Buckets, cc.Buckets), ","))
		if e.NoData {
			why += ", no quota data"
		} else {
			why += fmt.Sprintf(", weekly %.0f%% left, resets %s", e.WeeklyRemaining*100, fmtTime(e.WeeklyReset))
		}
		_ = i
		add(e, RoleOwn, why)
	}
	for _, e := range spare {
		add(e, RoleSpare, fmt.Sprintf("spare $%.2f, resets %s", e.Spare, fmtTime(e.WeeklyReset)))
	}
	if policy == PolicyAny {
		var rest []*eval
		for _, id := range pc.order {
			e := pc.evals[id]
			if e.Usable {
				rest = append(rest, e)
			}
		}
		sort.SliceStable(rest, func(i, j int) bool {
			a, b := rest[i], rest[j]
			if a.APIKey != b.APIKey {
				return !a.APIKey
			}
			return a.Spare > b.Spare
		})
		for _, e := range rest {
			add(e, RoleLastResort, fmt.Sprintf("policy any (%s)", policySrc))
		}
	}
	return out, policy, policySrc
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func intersect(a, b []string) []string {
	var out []string
	for _, x := range b {
		if contains(a, x) {
			out = append(out, x)
		}
	}
	return out
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format("Mon 15:04Z")
}

func fmtDur(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	h := int(d.Hours())
	switch {
	case h >= 48:
		return fmt.Sprintf("%dd%dh", h/24, h%24)
	case h >= 1:
		return fmt.Sprintf("%dh%02dm", h, int(d.Minutes())%60)
	default:
		return d.Round(time.Second).String()
	}
}
