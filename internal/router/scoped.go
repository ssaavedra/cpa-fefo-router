package router

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ssaavedra/cpa-fefo-router/internal/pricing"
	"github.com/ssaavedra/cpa-fefo-router/internal/quota"
)

// scopeFamily maps a pricing family to the family name Anthropic's usage
// report uses for scoped windows: claude-fable -> fable.
func scopeFamily(pricingFamily string) string {
	return strings.TrimPrefix(strings.ToLower(pricingFamily), "claude-")
}

func (r *Router) requestFamilyLocked(model string) string {
	if strings.TrimSpace(model) == "" {
		return ""
	}
	return scopeFamily(r.prices.Cost(model, pricing.Usage{}).Family)
}

// scopedVerdict is what an account's usage report says about one family.
type scopedVerdict struct {
	Window    quota.ScopedWindow
	HasWindow bool
	// Estimated is the window's remaining share now: the polled value minus
	// what CPA spent on the family since, pro rata to the allowance in dollars.
	Estimated float64
	Spent     float64
	Block     string
}

// scopedLocked judges whether an account may serve a request of family fam.
// Without a usage report nothing is known and the account is allowed. With
// one, a family with its own window is limited by that window; a gated
// family without a window has no allowance on this plan and would run on
// paid credits whenever extra usage is on (or its state is unknown).
func (r *Router) scopedLocked(st *accountState, capacityUSD float64, fam string, now time.Time) scopedVerdict {
	var v scopedVerdict
	rep := st.Usage.Report
	if fam == "" || !rep.HasReport {
		return v
	}
	cfg := r.cfg
	if w, ok := rep.Scoped(fam); ok {
		v.Window, v.HasWindow, v.Spent = w, true, st.Usage.SpendSince[fam]
		v.Estimated = w.Remaining
		if !w.ResetAt.IsZero() && !now.Before(w.ResetAt) {
			v.Estimated = 1
		} else if allowance := w.Allowance * capacityUSD; allowance > 0 {
			v.Estimated -= v.Spent / allowance
		}
		if v.Estimated < cfg.MinRemaining && !cfg.AllowExtraUsage {
			v.Block = fmt.Sprintf("%s allowance ~%.1f%% left (polled %.0f%% at %s, $%.2f spent since of $%.0f), resets %s",
				fam, v.Estimated*100, w.Remaining*100, fmtTime(rep.ObservedAt), v.Spent, w.Allowance*capacityUSD, fmtTime(w.ResetAt))
		}
		return v
	}
	if !contains(cfg.ScopedFamilies, fam) || cfg.AllowExtraUsage {
		return v
	}
	switch {
	case !rep.ExtraUsageHasState:
		v.Block = fmt.Sprintf("no %s allowance on this plan and extra usage state unknown (would bill credits)", fam)
	case rep.ExtraUsageEnabled:
		v.Block = fmt.Sprintf("no %s allowance on this plan and extra usage is enabled (would bill credits)", fam)
	}
	return v
}

// scopedFamilyLocked reports whether fam is limited separately anywhere:
// configured, or a scoped window in any account's report.
func (r *Router) scopedFamilyLocked(fam string) bool {
	if fam == "" {
		return false
	}
	if contains(r.cfg.ScopedFamilies, fam) {
		return true
	}
	for _, st := range r.accounts {
		if _, ok := st.Usage.Report.Scoped(fam); ok {
			return true
		}
	}
	return false
}

var roleOrder = []string{RoleProbe, RoleSpareFirst, RoleOwn, RoleSpare, RoleLastResort}

func roleRank(role string) int {
	for i, r := range roleOrder {
		if r == role {
			return i
		}
	}
	return len(roleOrder)
}

// gateScoped drops the accounts that must not serve a request of family fam
// and, within each role, puts the accounts whose scoped window expires first
// ahead (an account with no allowance for it last). It returns the excluded
// accounts with the reason.
func (pc *planContext) gateScoped(order []planEntry, fam string) ([]planEntry, map[string]string) {
	r := pc.r
	gated := map[string]string{}
	if !r.scopedFamilyLocked(fam) {
		return order, gated
	}
	type rank struct {
		tier  int // 0: own window, 1: not polled, 2: report shows no allowance
		reset time.Time
	}
	ranks := map[string]rank{}
	for _, id := range pc.order {
		st, e := r.accounts[id], pc.evals[id]
		v := r.scopedLocked(st, e.Capacity.USD, fam, pc.now)
		switch {
		case v.Block != "":
			gated[id] = v.Block
		case v.HasWindow:
			ranks[id] = rank{0, v.Window.ResetAt}
		case st.Usage.Report.HasReport:
			ranks[id] = rank{tier: 2}
		default:
			ranks[id] = rank{tier: 1}
		}
	}
	var out []planEntry
	for _, p := range order {
		if _, ok := gated[p.ID]; ok {
			continue
		}
		if ranks[p.ID].tier == 0 {
			st := r.accounts[p.ID]
			v := r.scopedLocked(st, pc.evals[p.ID].Capacity.USD, fam, pc.now)
			p.Why += fmt.Sprintf(", %s ~%.0f%% left, resets %s", fam, v.Estimated*100, fmtTime(v.Window.ResetAt))
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := roleRank(a.Role), roleRank(b.Role); ra != rb {
			return ra < rb
		}
		ka, kb := ranks[a.ID], ranks[b.ID]
		if ka.tier != kb.tier {
			return ka.tier < kb.tier
		}
		return ka.tier == 0 && !ka.reset.IsZero() && (kb.reset.IsZero() || ka.reset.Before(kb.reset))
	})
	return out, gated
}
