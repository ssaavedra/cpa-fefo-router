package router

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// expectHorizon bounds the expected-use walk. Weekly windows reset within a
// week; the extra day covers readings that put a reset slightly further out.
const expectHorizon = 8 * 24 * time.Hour

// walkAccount is one account as the expected-use walk sees it.
type walkAccount struct {
	e *eval
	// balance is the dollars left in the weekly window; below floor
	// (min_remaining) the account is blocked and serves nothing.
	balance, floor float64
	// reset is the next weekly reset in the walk; zero when unknown, and then
	// the account never refills. end is e.WeeklyReset: Expected covers the
	// demand that reaches the account before it.
	reset, end time.Time
	from       time.Time // blocked until
	off        bool      // disabled: never serves
	// reached is the demand that reached the account before end, by client
	// ("" for usage outside CPA). The account may not have served all of it.
	reached map[string]float64
}

// ask offers the account need dollars at t and returns what it could not
// serve.
func (a *walkAccount) ask(t time.Time, client string, need float64) float64 {
	if t.Before(a.end) {
		a.reached[client] += need
	}
	if a.off || a.from.After(t) {
		return need
	}
	take := math.Min(need, math.Max(0, a.balance-a.floor))
	a.balance -= take
	return need - take
}

// walkClient is one client drawing on its own accounts.
type walkClient struct {
	perHour float64
	name    string
	// groups are its accounts by bucket, in bucket order; within a bucket the
	// account that resets soonest goes first, as ownList orders them.
	groups [][]*walkAccount
}

func (c *walkClient) order() []*walkAccount {
	var out []*walkAccount
	for _, g := range c.groups {
		g = append([]*walkAccount(nil), g...)
		sort.SliceStable(g, func(i, j int) bool {
			a, b := g[i], g[j]
			if a.reset.IsZero() != b.reset.IsZero() {
				return !a.reset.IsZero()
			}
			if !a.reset.Equal(b.reset) {
				return a.reset.Before(b.reset)
			}
			return a.e.ID < b.e.ID
		})
		out = append(out, g...)
	}
	return out
}

// expectAll fills Expected and Spare for every account: how much of it its
// home clients (and usage outside CPA) will need before its weekly reset.
//
// It walks time forward from now, an hour at a time and at every reset, with
// every client drawing its demand from its own accounts in preference order
// and each account refilling at its weekly reset. An account's expected use
// is the demand that reaches it before its reset. Walking time, rather than
// adding up what earlier accounts hold, catches two things: an earlier
// account that refills only partway through this account's week leaves a gap
// until it does, and clients that share an account share what it has left.
func (pc *planContext) expectAll() {
	week := 7 * 24 * time.Hour
	var accts []*walkAccount
	byID := map[string]*walkAccount{}
	end := pc.now
	for _, id := range pc.order {
		e := pc.evals[id]
		if e.NoData {
			continue // nothing known to draw on
		}
		a := &walkAccount{
			e: e, balance: math.Max(0, e.remaining()), floor: pc.cfg.MinRemaining * e.Capacity.USD,
			reset: e.WeeklyReset, end: e.WeeklyReset, reached: map[string]float64{},
		}
		if !e.Usable {
			a.from = e.BlockedUntil
			a.off = e.BlockedUntil.IsZero() || pc.r.accounts[id].Disabled
		}
		accts = append(accts, a)
		byID[id] = a
		if a.end.After(end) {
			end = a.end
		}
	}
	if limit := pc.now.Add(expectHorizon); end.After(limit) {
		end = limit
	}

	ext := map[string]rateInfo{}
	for _, a := range accts {
		ext[a.e.ID] = pc.rate(externalClient, a.e.ID)
	}
	var clients []*walkClient
	for _, alias := range sortedKeys(pc.cfg.Clients) {
		d := pc.rate(alias, "")
		if !d.Known || d.PerHour <= 0 {
			continue // unknown demand makes its accounts' expected use unknown below
		}
		c := &walkClient{perHour: d.PerHour, name: alias}
		seen := map[string]bool{}
		for _, b := range pc.r.clientConfigLocked(alias).Buckets {
			var g []*walkAccount
			for _, a := range accts {
				if !seen[a.e.ID] && contains(a.e.Buckets, b) {
					seen[a.e.ID] = true
					g = append(g, a)
				}
			}
			if len(g) > 0 {
				c.groups = append(c.groups, g)
			}
		}
		if len(c.groups) > 0 {
			clients = append(clients, c)
		}
	}

	for t := pc.now; t.Before(end); {
		next := t.Add(time.Hour)
		for _, a := range accts {
			if a.reset.After(t) && a.reset.Before(next) {
				next = a.reset
			}
			if a.from.After(t) && a.from.Before(next) {
				next = a.from
			}
		}
		if next.After(end) {
			next = end
		}
		hours := next.Sub(t).Hours()
		// Usage outside CPA runs on its own account and cannot move.
		for _, a := range accts {
			if r := ext[a.e.ID]; r.PerHour > 0 {
				a.ask(t, "", r.PerHour*hours)
			}
		}
		for _, c := range clients {
			need := c.perHour * hours
			for _, a := range c.order() {
				if need <= 0 {
					break
				}
				need = a.ask(t, c.name, need)
			}
		}
		t = next
		for _, a := range accts {
			for !a.reset.IsZero() && !a.reset.After(t) {
				a.balance = a.e.Capacity.USD
				a.reset = a.reset.Add(week)
			}
		}
	}

	for _, id := range pc.order {
		pc.settleSpare(pc.evals[id], byID[id], ext[id])
	}
}

// settleSpare turns the walk's result for one account into Expected, Spare
// and their explanation.
func (pc *planContext) settleSpare(e *eval, a *walkAccount, ext rateInfo) {
	unknown := func(why string) {
		e.Expected, e.Spare = math.Inf(1), math.Inf(-1)
		e.SpareWhy = why
	}
	if a == nil || e.WeeklyReset.IsZero() {
		unknown("weekly window not known yet")
		return
	}
	if !ext.Known {
		unknown(ext.Why)
		return
	}
	hours := e.hoursToReset(pc.now)
	total := a.reached[""]
	var parts []string
	if total > 0 {
		parts = append(parts, fmt.Sprintf("outside CPA $%.2f", total))
	}
	for _, client := range e.Home {
		d := pc.rate(client, "")
		if !d.Known {
			unknown(client + ": " + d.Why)
			return
		}
		got := a.reached[client]
		total += got
		parts = append(parts, fmt.Sprintf("%s $%.2f/h×%s→$%.2f", client, d.PerHour, fmtDur(time.Duration(hours*float64(time.Hour))), got))
	}
	// The account cannot serve more than it has left before it resets; the
	// rest of the need falls on other accounts. Capping changes no lending
	// decision (lending needs Expected < remaining / factor).
	e.Expected = math.Min(total, math.Max(0, e.remaining()))
	e.Spare = e.remaining() - pc.factor*e.Expected
	e.SpareWhy = fmt.Sprintf("left $%.2f − %.1f×expected $%.2f", e.remaining(), pc.factor, e.Expected)
	// The reserve shrinks toward the reset, so an account keeps more of a
	// fresh week for its home clients than of one about to expire.
	if keep := reserveAt(pc.cfg.LendReserve, time.Duration(hours*float64(time.Hour))); keep > 0 {
		reserve := keep * e.Capacity.USD
		if capped := e.remaining() - reserve; capped < e.Spare {
			e.Spare = capped
			e.SpareWhy = fmt.Sprintf("left $%.2f − reserve %.0f%% of week $%.2f (more than %.1f×expected $%.2f)",
				e.remaining(), keep*100, reserve, pc.factor, e.Expected)
		}
	}
	if total > e.Expected {
		parts = append(parts, fmt.Sprintf("need $%.2f is more than is left", total))
	}
	if len(parts) > 0 {
		e.SpareWhy += " (" + strings.Join(parts, ", ") + ")"
	}
	// The weekly sums say nothing about the 5h window. A borrower can empty
	// it in an hour and block the account for its home clients until it
	// refills, so an account whose 5h window runs low stops lending.
	if limit := pc.cfg.LendMinShort; e.Spare > 0 && e.ShortRemaining < limit {
		e.SpareWhy = fmt.Sprintf("5h window %.0f%% left, under lend_min_short %.0f%%, until %s; otherwise spare $%.2f: %s",
			e.ShortRemaining*100, limit*100, fmtTime(e.ShortReset), e.Spare, e.SpareWhy)
		e.Spare = 0
	}
}
