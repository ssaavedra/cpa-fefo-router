package router

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// projectionHorizon is how far ahead the pool projection looks: one week,
// so every account resets at least once inside it.
const projectionHorizon = 7 * 24 * time.Hour

// ProjectionView projects one provider's whole pool: every client's demand
// plus usage outside CPA, served from all accounts as one pool in FEFO order
// (soonest reset first), so lending each account's spare is counted. It is an
// upper bound on what routing can achieve: clients that never borrow, or
// buckets that do not reach an account, can leave quota unused that this
// projection spends.
type ProjectionView struct {
	Provider string `json:"provider"`
	// Accounts counted, and accounts left out (no quota data or weekly window).
	Accounts int      `json:"accounts"`
	Excluded []string `json:"excluded,omitempty"`
	// Current state, summed over counted accounts.
	CapacityUSD  float64 `json:"capacity_usd"`  // weekly capacity
	RemainingUSD float64 `json:"remaining_usd"` // left until each account's reset
	ExpectedUSD  float64 `json:"expected_usd"`  // home clients' expected use until reset
	SpareUSD     float64 `json:"spare_usd"`     // positive spare, lendable now
	// Demand is the pool's dollars per hour: every client plus outside CPA.
	DemandKnown      bool    `json:"demand_known"`
	DemandUSDPerHour float64 `json:"demand_usd_per_hour,omitempty"`
	Why              string  `json:"why,omitempty"`
	// Over HorizonHours, serving demand FEFO from the pool.
	HorizonHours float64   `json:"horizon_hours"`
	UseUSD       float64   `json:"use_usd,omitempty"`       // demand served
	WastedUSD    float64   `json:"wasted_usd,omitempty"`    // quota that resets unused
	ShortfallUSD float64   `json:"shortfall_usd,omitempty"` // demand nothing could serve
	DryAt        time.Time `json:"dry_at,omitzero"`         // first hour demand went unserved
	// Utilisation is demand over a week against weekly capacity.
	Utilisation float64 `json:"utilisation,omitempty"`
}

// poolAccount is one account's quota as the pool simulation sees it.
type poolAccount struct {
	balance, capacity float64
	reset, from       time.Time // next weekly reset; usable from
}

// simulatePool serves demand dollars per hour from accounts for horizon,
// hour by hour, draining the account that resets soonest first. Quota left at
// a reset is wasted; demand no account can serve is a shortfall.
func simulatePool(now time.Time, accts []poolAccount, demand float64, horizon time.Duration) (use, wasted, short float64, dry time.Time) {
	as := append([]poolAccount(nil), accts...)
	for t := now; t.Before(now.Add(horizon)); t = t.Add(time.Hour) {
		end := t.Add(time.Hour)
		for i := range as {
			a := &as[i]
			for !a.reset.After(t) {
				wasted += a.balance
				a.balance = a.capacity
				a.reset = a.reset.Add(7 * 24 * time.Hour)
			}
		}
		sort.SliceStable(as, func(i, j int) bool { return as[i].reset.Before(as[j].reset) })
		need := demand
		for i := range as {
			if need <= 0 {
				break
			}
			a := &as[i]
			if a.from.After(t) || a.balance <= 0 {
				continue
			}
			// An account resetting mid-hour serves only the part before it.
			take := math.Min(a.balance, need)
			if a.reset.Before(end) {
				take = math.Min(take, demand*a.reset.Sub(t).Hours())
			}
			a.balance -= take
			need -= take
			use += take
		}
		if need > 1e-9 {
			short += need
			if dry.IsZero() {
				dry = t
			}
		}
	}
	for _, a := range as {
		if !a.reset.After(now.Add(horizon)) {
			wasted += a.balance
		}
	}
	return use, wasted, short, dry
}

// projection builds the pool projection for this provider. clients are every
// client alias that may send traffic, including unassigned keys.
func (pc *planContext) projection(clients []string) ProjectionView {
	pv := ProjectionView{Provider: pc.provider, HorizonHours: projectionHorizon.Hours()}
	var accts []poolAccount
	var missing []string
	for _, id := range pc.order {
		e := pc.evals[id]
		if e.NoData || e.WeeklyReset.IsZero() || e.Capacity.USD <= 0 {
			pv.Excluded = append(pv.Excluded, e.Name)
			continue
		}
		pv.Accounts++
		pv.CapacityUSD += e.Capacity.USD
		pv.RemainingUSD += math.Max(0, e.remaining())
		if !math.IsInf(e.Expected, 0) {
			pv.ExpectedUSD += e.Expected
			pv.SpareUSD += math.Max(0, e.Spare)
		}
		a := poolAccount{balance: math.Max(0, e.remaining()), capacity: e.Capacity.USD, reset: e.WeeklyReset, from: pc.now}
		if !e.Usable && e.BlockedUntil.After(pc.now) {
			a.from = e.BlockedUntil
		}
		accts = append(accts, a)
		ext := pc.rate(externalClient, id)
		if !ext.Known {
			missing = append(missing, "outside CPA: "+ext.Why)
			continue
		}
		pv.DemandUSDPerHour += ext.PerHour
	}
	for _, c := range clients {
		d := pc.rate(c, "")
		if !d.Known {
			missing = append(missing, c+": "+d.Why)
			continue
		}
		pv.DemandUSDPerHour += d.PerHour
	}
	if len(missing) > 0 {
		pv.DemandUSDPerHour = 0
		pv.Why = "demand unknown: " + strings.Join(dedupe(missing), "; ")
		return pv
	}
	if pv.Accounts == 0 {
		pv.Why = "no account has a known weekly window"
		return pv
	}
	pv.DemandKnown = true
	pv.UseUSD, pv.WastedUSD, pv.ShortfallUSD, pv.DryAt = simulatePool(pc.now, accts, pv.DemandUSDPerHour, projectionHorizon)
	pv.Utilisation = pv.DemandUSDPerHour * 7 * 24 / pv.CapacityUSD
	pv.Why = fmt.Sprintf("$%.2f/h from %d clients and outside CPA, served soonest-reset first across %d accounts", pv.DemandUSDPerHour, len(clients), pv.Accounts)
	return pv
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	out := xs[:0]
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// text is the one-line human rendering of the projection: dollars, or
// shares of the pool's weekly capacity when tokens is set.
func (p ProjectionView) text(tokens bool) string {
	amt := func(usd float64) string { return fmt.Sprintf("$%.2f", usd) }
	if tokens {
		amt = func(usd float64) string {
			if p.CapacityUSD <= 0 {
				return "?"
			}
			return fmt.Sprintf("%.0f%%", usd/p.CapacityUSD*100)
		}
	}
	s := fmt.Sprintf("%s: %d accounts", p.Provider, p.Accounts)
	if tokens {
		s += fmt.Sprintf(" (amounts as share of the pool's week), remaining %s, home expected %s, spare now %s",
			amt(p.RemainingUSD), amt(p.ExpectedUSD), amt(p.SpareUSD))
	} else {
		s += fmt.Sprintf(", capacity $%.2f/week, remaining %s, home expected %s, spare now %s",
			p.CapacityUSD, amt(p.RemainingUSD), amt(p.ExpectedUSD), amt(p.SpareUSD))
	}
	if len(p.Excluded) > 0 {
		s += " (left out, no window: " + strings.Join(p.Excluded, ", ") + ")"
	}
	if !p.DemandKnown {
		return s + "; " + p.Why
	}
	if tokens {
		s += fmt.Sprintf("; pool load %.0f%% of weekly capacity", p.Utilisation*100)
	} else {
		s += fmt.Sprintf("; pool load $%.2f/h = %.0f%% of weekly capacity", p.DemandUSDPerHour, p.Utilisation*100)
	}
	s += fmt.Sprintf("; next %s: use %s, reset unused %s",
		fmtDur(time.Duration(p.HorizonHours*float64(time.Hour))), amt(p.UseUSD), amt(p.WastedUSD))
	if p.ShortfallUSD > 0 {
		s += fmt.Sprintf(", SHORT %s from %s", amt(p.ShortfallUSD), fmtTime(p.DryAt))
	}
	return s
}
