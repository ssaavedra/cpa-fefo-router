package router

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ssaavedra/cpa-fefo-router/internal/pricing"
)

// metrics are cumulative counters since plugin load.
type metrics struct {
	picks        map[[4]string]float64 // client, account, role, outcome
	tokens       map[[2]string]float64 // client, account
	costUSD      map[[2]string]float64 // client, account
	unknown      map[string]*unknownModel
	requests     map[[3]string]float64 // client, account, failed
	pacedSeconds map[string]float64
	pacedCount   map[string]float64
	waitRejects  map[string]float64
}

func newMetrics() metrics {
	return metrics{
		picks:        map[[4]string]float64{},
		tokens:       map[[2]string]float64{},
		costUSD:      map[[2]string]float64{},
		unknown:      map[string]*unknownModel{},
		requests:     map[[3]string]float64{},
		pacedSeconds: map[string]float64{},
		pacedCount:   map[string]float64{},
		waitRejects:  map[string]float64{},
	}
}

func (m *metrics) pick(d Decision) {
	m.picks[[4]string{d.Client, d.Account, d.Role, d.Outcome}]++
}

func (m *metrics) usage(client, account string, tokens int64, usd float64, failed bool) {
	m.tokens[[2]string{client, account}] += float64(tokens)
	m.costUSD[[2]string{client, account}] += usd
	m.requests[[3]string{client, account, fmt.Sprint(failed)}]++
}

// unknownModel counts requests priced as a family range because the model
// is missing from the price table.
type unknownModel struct {
	Family    string
	Requests  int
	Low, High float64
}

func (m *metrics) unpriced(model string, q pricing.Quote) {
	u := m.unknown[model]
	if u == nil {
		if len(m.unknown) >= 100 {
			return
		}
		u = &unknownModel{}
		m.unknown[model] = u
	}
	u.Family = q.Family
	u.Requests++
	u.Low += q.Low
	u.High += q.High
}

func (m *metrics) paced(client string, d time.Duration) {
	m.pacedSeconds[client] += d.Seconds()
	m.pacedCount[client]++
}

func (m *metrics) waitRejected(client string) { m.waitRejects[client]++ }

// Prometheus renders the text exposition format.
func (r *Router) Prometheus() string {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	gauge := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}
	counter := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	}
	num := func(v float64) string {
		switch {
		case math.IsInf(v, 1):
			return "+Inf"
		case math.IsInf(v, -1):
			return "-Inf"
		case math.IsNaN(v):
			return "NaN"
		}
		return fmt.Sprintf("%g", v)
	}
	lbl := func(kv ...string) string {
		parts := make([]string, 0, len(kv)/2)
		for i := 0; i+1 < len(kv); i += 2 {
			v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(kv[i+1])
			parts = append(parts, fmt.Sprintf(`%s="%s"`, kv[i], v))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}

	type row struct {
		pc *planContext
		e  *eval
	}
	var rows []row
	providers := map[string]bool{}
	for _, st := range r.accounts {
		providers[st.Info.Provider] = true
	}
	for _, p := range sortedKeys(providers) {
		pc := r.newPlanContextLocked(p, now)
		for _, id := range pc.order {
			rows = append(rows, row{pc, pc.evals[id]})
		}
	}

	gauge("fefo_account_weekly_remaining_ratio", "Unused share of the weekly window.")
	for _, x := range rows {
		fmt.Fprintf(&b, "fefo_account_weekly_remaining_ratio%s %s\n", lbl("account", x.e.Name, "provider", x.e.Provider), num(x.e.WeeklyRemaining))
	}
	gauge("fefo_account_short_remaining_ratio", "Unused share of the shortest (5h) window.")
	for _, x := range rows {
		fmt.Fprintf(&b, "fefo_account_short_remaining_ratio%s %s\n", lbl("account", x.e.Name, "provider", x.e.Provider), num(x.e.ShortRemaining))
	}
	gauge("fefo_account_weekly_reset_seconds", "Seconds until the weekly window refills.")
	for _, x := range rows {
		if !x.e.WeeklyReset.IsZero() {
			fmt.Fprintf(&b, "fefo_account_weekly_reset_seconds%s %s\n", lbl("account", x.e.Name, "provider", x.e.Provider), num(x.e.WeeklyReset.Sub(now).Seconds()))
		}
	}
	gauge("fefo_account_capacity_usd", "Weekly capacity in API-equivalent USD.")
	for _, x := range rows {
		fmt.Fprintf(&b, "fefo_account_capacity_usd%s %s\n", lbl("account", x.e.Name, "provider", x.e.Provider, "source", x.e.Capacity.Source), num(x.e.Capacity.USD))
	}
	gauge("fefo_account_remaining_usd", "API-equivalent USD left in the weekly window.")
	gauge("fefo_account_expected_usd", "Expected use by home clients until the weekly reset, in API-equivalent USD.")
	gauge("fefo_account_spare_usd", "Lendable quota in API-equivalent USD (remaining − spare_factor × expected).")
	for _, x := range rows {
		l := lbl("account", x.e.Name, "provider", x.e.Provider)
		fmt.Fprintf(&b, "fefo_account_remaining_usd%s %s\n", l, num(x.e.remaining()))
		fmt.Fprintf(&b, "fefo_account_expected_usd%s %s\n", l, num(x.e.Expected))
		fmt.Fprintf(&b, "fefo_account_spare_usd%s %s\n", l, num(x.e.Spare))
	}
	gauge("fefo_account_scoped_remaining", "Estimated unused share of a model family's own weekly allowance (polled, minus spend since).")
	for _, x := range rows {
		st := r.accounts[x.e.ID]
		for _, w := range st.Usage.Report.ScopedWindows {
			v := r.scopedLocked(st, x.e.Capacity.USD, w.Family, now)
			fmt.Fprintf(&b, "fefo_account_scoped_remaining%s %s\n", lbl("account", x.e.Name, "provider", x.e.Provider, "window", w.Name), num(v.Estimated))
		}
	}
	gauge("fefo_account_usable", "1 when the account can take traffic.")
	for _, x := range rows {
		v := 0.0
		if x.e.Usable {
			v = 1
		}
		fmt.Fprintf(&b, "fefo_account_usable%s %s\n", lbl("account", x.e.Name, "provider", x.e.Provider, "reason", x.e.Block), num(v))
	}
	gauge("fefo_client_demand_per_hour", "Client consumption per hour in API-equivalent USD (unit=usd) or tokens (unit=tokens).")
	for _, p := range sortedKeys(providers) {
		pc := r.newPlanContextLocked(p, now)
		clients := append(sortedKeys(r.cfg.Clients), externalClient)
		for _, c := range clients {
			ri := pc.rate(c, "")
			if !ri.Known {
				continue
			}
			fmt.Fprintf(&b, "fefo_client_demand_per_hour%s %s\n", lbl("client", c, "provider", p, "window", "24h", "unit", "usd"), num(ri.Day))
			fmt.Fprintf(&b, "fefo_client_demand_per_hour%s %s\n", lbl("client", c, "provider", p, "window", "7d", "unit", "usd"), num(ri.Week))
			fmt.Fprintf(&b, "fefo_client_demand_per_hour%s %s\n", lbl("client", c, "provider", p, "window", "24h", "unit", "tokens"), num(ri.TokensDay))
			fmt.Fprintf(&b, "fefo_client_demand_per_hour%s %s\n", lbl("client", c, "provider", p, "window", "7d", "unit", "tokens"), num(ri.TokensWeek))
		}
	}
	counter("fefo_picks_total", "Routing decisions.")
	for _, k := range sortedTupleKeys(r.metrics.picks) {
		fmt.Fprintf(&b, "fefo_picks_total%s %s\n", lbl("client", k[0], "account", k[1], "role", k[2], "outcome", k[3]), num(r.metrics.picks[k]))
	}
	counter("fefo_tokens_total", "Tokens served.")
	for _, k := range sortedTupleKeys(r.metrics.tokens) {
		fmt.Fprintf(&b, "fefo_tokens_total%s %s\n", lbl("client", k[0], "account", k[1]), num(r.metrics.tokens[k]))
	}
	counter("fefo_cost_usd_total", "API-equivalent USD served (high end of the quote for unpriced models).")
	for _, k := range sortedTupleKeys(r.metrics.costUSD) {
		fmt.Fprintf(&b, "fefo_cost_usd_total%s %s\n", lbl("client", k[0], "account", k[1]), num(r.metrics.costUSD[k]))
	}
	counter("fefo_unknown_model_requests_total", "Requests whose model is missing from the price table (priced as a family range).")
	for _, m := range sortedKeys(r.metrics.unknown) {
		fmt.Fprintf(&b, "fefo_unknown_model_requests_total%s %d\n", lbl("model", m), r.metrics.unknown[m].Requests)
	}
	gauge("fefo_prices_synced_timestamp", "Unix time of the last successful models.dev price sync (0: never).")
	synced := 0.0
	if !r.synced.SyncedAt.IsZero() {
		synced = float64(r.synced.SyncedAt.Unix())
	}
	fmt.Fprintf(&b, "fefo_prices_synced_timestamp %s\n", num(synced))
	counter("fefo_requests_total", "Requests served.")
	for _, k := range sortedTupleKeys(r.metrics.requests) {
		fmt.Fprintf(&b, "fefo_requests_total%s %s\n", lbl("client", k[0], "account", k[1], "failed", k[2]), num(r.metrics.requests[k]))
	}
	counter("fefo_paced_seconds_total", "Delay added by pacing and waiting.")
	for _, c := range sortedKeys(r.metrics.pacedSeconds) {
		fmt.Fprintf(&b, "fefo_paced_seconds_total%s %s\n", lbl("client", c), num(r.metrics.pacedSeconds[c]))
	}
	counter("fefo_paced_requests_total", "Requests delayed.")
	for _, c := range sortedKeys(r.metrics.pacedCount) {
		fmt.Fprintf(&b, "fefo_paced_requests_total%s %s\n", lbl("client", c), num(r.metrics.pacedCount[c]))
	}
	counter("fefo_wait_rejections_total", "Requests answered 429 under the wait policy.")
	for _, c := range sortedKeys(r.metrics.waitRejects) {
		fmt.Fprintf(&b, "fefo_wait_rejections_total%s %s\n", lbl("client", c), num(r.metrics.waitRejects[c]))
	}
	return b.String()
}

func sortedTupleKeys[K [2]string | [3]string | [4]string](m map[K]float64) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]) < fmt.Sprint(out[j]) })
	return out
}
