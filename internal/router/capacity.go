package router

import (
	"fmt"
	"sort"
	"time"
)

// learnCycle is one weekly cycle of an account: the API-equivalent dollars
// CPA sent it against the share of the weekly window they consumed.
type learnCycle struct {
	// Reset is the weekly reset that ends the cycle.
	Reset time.Time `json:"reset,omitzero"`
	USD   float64   `json:"usd"`
	Delta float64   `json:"delta"`
}

// estimate is the dollars one full weekly window holds, once the cycle has
// seen enough of the window for the ratio to be meaningful.
func (c learnCycle) estimate(minDelta float64) (float64, bool) {
	if c.Delta < minDelta || c.USD <= 0 {
		return 0, false
	}
	return c.USD / c.Delta, true
}

// capacityLearn re-learns an account's weekly capacity every cycle, so plan
// upgrades and downgrades show up within a week.
type capacityLearn struct {
	Current learnCycle `json:"current"`
	// Previous is the last finished cycle that produced an estimate.
	Previous *learnCycle `json:"previous,omitempty"`
	// Folded is the last hour bucket added to Current.
	Folded int64 `json:"folded_hour,omitempty"`
}

// foldLocked adds an account's completed hours to its learning cycle. An
// hour whose readings belong to a later weekly reset starts a new cycle.
// Hours with unpriced requests, a weekly reset inside them, or no prices at
// all (recorded before pricing existed) are skipped.
func (r *Router) foldLocked(id string, now time.Time) {
	st := r.accounts[id]
	if st == nil {
		return
	}
	l := &st.Learn
	cur := hourOf(now)
	var pending []int64
	for h := range r.hours[id] {
		if h > l.Folded && h < cur {
			pending = append(pending, h)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i] < pending[j] })
	for _, h := range pending {
		b := r.hours[id][h]
		l.Folded = h
		if !b.Cycle.IsZero() && !sameReset(b.Cycle, l.Current.Reset) {
			switch {
			case l.Current.Reset.IsZero():
				l.Current.Reset = b.Cycle
			case b.Cycle.After(l.Current.Reset):
				if _, ok := l.Current.estimate(r.cfg.LearnMinDelta); ok {
					prev := l.Current
					l.Previous = &prev
				}
				l.Current = learnCycle{Reset: b.Cycle}
			default:
				continue
			}
		}
		if b.Tainted || !b.Priced || len(b.USDHigh) == 0 {
			continue
		}
		for _, v := range b.USDHigh {
			l.Current.USD += v
		}
		l.Current.Delta += b.Delta
	}
}

// Sources of an account's effective capacity, in precedence order.
const (
	CapacityPinned   = "pinned"
	CapacityCurrent  = "learned this cycle"
	CapacityPrevious = "learned last cycle"
	CapacityPrior    = "prior"
)

// capacityInfo is an account's weekly capacity in dollars and where it came from.
type capacityInfo struct {
	USD    float64
	Source string
	Why    string
}

// learned returns the account's own estimate, current cycle first.
func (c Config) learned(l capacityLearn) (float64, string, string, bool) {
	if v, ok := l.Current.estimate(c.LearnMinDelta); ok {
		return v, CapacityCurrent, fmt.Sprintf("$%.2f over %.1f%% of the week", l.Current.USD, l.Current.Delta*100), true
	}
	if l.Previous != nil {
		if v, ok := l.Previous.estimate(c.LearnMinDelta); ok {
			return v, CapacityPrevious, fmt.Sprintf("cycle ending %s: $%.2f over %.1f%% of the week", fmtTime(l.Previous.Reset), l.Previous.USD, l.Previous.Delta*100), true
		}
	}
	return 0, "", "", false
}

// unitUSD is the dollars per unit of relative capacity among accounts of one
// provider: the median over accounts that learned an estimate, else the
// configured default.
func (r *Router) unitUSDLocked(provider string) (float64, string) {
	var units []float64
	for _, st := range r.accounts {
		if st.Info.Provider != provider {
			continue
		}
		if _, pinned := r.cfg.capacityUSDOf(st.Info); pinned {
			continue
		}
		if v, _, _, ok := r.cfg.learned(st.Learn); ok {
			units = append(units, v/r.cfg.capacityOf(st.Info))
		}
	}
	if len(units) == 0 {
		return r.cfg.DefaultUnitUSD, fmt.Sprintf("default_unit_usd $%.0f", r.cfg.DefaultUnitUSD)
	}
	sort.Float64s(units)
	m := units[len(units)/2]
	if len(units)%2 == 0 {
		m = (units[len(units)/2-1] + units[len(units)/2]) / 2
	}
	return m, fmt.Sprintf("median $%.0f/unit over %d learned account(s)", m, len(units))
}

func (r *Router) capacityLocked(st *accountState, unit float64, unitWhy string) capacityInfo {
	if v, ok := r.cfg.capacityUSDOf(st.Info); ok {
		return capacityInfo{USD: v, Source: CapacityPinned, Why: "capacity_usd in config"}
	}
	if v, src, why, ok := r.cfg.learned(st.Learn); ok {
		return capacityInfo{USD: v, Source: src, Why: why}
	}
	rel := r.cfg.capacityOf(st.Info)
	why := fmt.Sprintf("capacity %g × %s", rel, unitWhy)
	if c := st.Learn.Current; c.Delta > 0 {
		why += fmt.Sprintf("; learning: $%.2f over %.1f%% of the week (need %.0f%%)", c.USD, c.Delta*100, r.cfg.LearnMinDelta*100)
	}
	return capacityInfo{USD: rel * unit, Source: CapacityPrior, Why: why}
}
