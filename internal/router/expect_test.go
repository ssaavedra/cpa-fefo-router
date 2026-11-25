package router

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const thousandEach = "capacity_usd: {d@example.com: 1000, i@example.com: 1000, p@example.com: 1000}"

// walkWith reruns the expected-use walk with fixed demand in dollars per
// hour; clients left out demand nothing, and nothing runs outside CPA.
func walkWith(h *harness, demand map[string]float64) *planContext {
	h.r.mu.Lock()
	defer h.r.mu.Unlock()
	pc := h.r.newPlanContextLocked("claude", h.now)
	for _, id := range pc.order {
		pc.demand[externalClient+"|"+id] = rateInfo{Known: true}
	}
	for c := range pc.cfg.Clients {
		pc.demand[c+"|"] = rateInfo{Known: true, PerHour: demand[c]}
	}
	pc.expectAll()
	return pc
}

// An earlier account that refills partway through this account's week only
// covers the client after it refills; until then the demand lands here. Its
// last min_remaining share serves nobody.
func TestExpectedWaitsForEarlierAccountToRefill(t *testing.T) {
	h := newHarness(t, thousandEach)
	// d: $30 left, $10 of it under min_remaining. It resets in 50h, i in 100h.
	h.seed(0.97, 0.3, 0.2, h.now.Add(50*time.Hour), h.now.Add(100*time.Hour), h.now.Add(120*time.Hour))
	pc := walkWith(h, map[string]float64{"pni": 10})
	i := pc.evals[acctI]
	// d serves 2h, then i carries pni for the 48h until d refills, which
	// then covers the rest of i's week. Adding up d's $30 and its $1000
	// refill would say i needs nothing.
	if !closeTo(i.Expected, 480, 1e-6) || !closeTo(i.Spare, 700-1.5*480, 1e-6) {
		t.Fatalf("i expected %.2f spare %.2f (%s)", i.Expected, i.Spare, i.SpareWhy)
	}
	if d := pc.evals[acctD]; !closeTo(d.Expected, 30, 1e-6) || !strings.Contains(d.SpareWhy, "more than is left") {
		t.Fatalf("d expected %.2f (%s)", d.Expected, d.SpareWhy)
	}
}

// An earlier account blocked on its 5h window serves nothing until the
// window refills; that stretch lands on the next account.
func TestExpectedCoversEarlierAccountWhileBlocked(t *testing.T) {
	h := newHarness(t, thousandEach)
	h.seed(0.2, 0.3, 0.2, h.now.Add(50*time.Hour), h.now.Add(100*time.Hour), h.now.Add(120*time.Hour))
	h.observe("pni", acctD, 0.2, h.now.Add(50*time.Hour), 1.0, h.now.Add(5*time.Hour), 0)
	pc := walkWith(h, map[string]float64{"pni": 10})
	if i := pc.evals[acctI]; !closeTo(i.Expected, 50, 1e-6) {
		t.Fatalf("i expected %.2f (%s)", i.Expected, i.SpareWhy)
	}
}

// Clients that share an earlier account share what it has left: each one
// alone would fit, together they overflow.
func TestClientsShareWhatAnAccountHasLeft(t *testing.T) {
	h := newHarness(t, "")
	cfg, err := ParseConfig([]byte(fmt.Sprintf(`
buckets: {d: [d@example.com], i: [i@example.com], p: [p@example.com]}
%s
clients:
  a: {keys: [%s], buckets: [d, i]}
  b: {keys: [%s], buckets: [d, i]}
`, thousandEach, callerScope("sk-a"), callerScope("sk-b"))))
	if err != nil {
		t.Fatal(err)
	}
	h.r.SetConfig(cfg)
	// d: $100 it can serve, resets after i. a and b each need $100 before i
	// resets.
	h.seed(0.89, 0.3, 0.2, h.now.Add(150*time.Hour), h.now.Add(100*time.Hour), h.now.Add(120*time.Hour))
	pc := walkWith(h, map[string]float64{"a": 1, "b": 1})
	i := pc.evals[acctI]
	if !closeTo(i.Expected, 100, 1e-6) || !strings.Contains(i.SpareWhy, "a $1.00/h×4d4h→$50.00") {
		t.Fatalf("i expected %.2f (%s)", i.Expected, i.SpareWhy)
	}
}

// An account whose 5h window runs low stops lending, sessions it already
// lent included, and lends again once the window refills.
func TestNoLendingWhileShortWindowIsLow(t *testing.T) {
	h := newHarness(t, "")
	start := h.now
	dReset, iReset, pReset := start.Add(80*time.Hour), start.Add(30*time.Hour), start.Add(60*time.Hour)
	h.buildHistory(7, 0.01, 0.01, dReset, iReset, pReset, 0.2, 0.3, 0.2)
	h.expectPick("connect", "s1", acctI) // i spare, resets before p

	h.observe("pni", acctI, 0.3, iReset, 0.75, h.now.Add(2*time.Hour), 0)
	if i := h.account("i@example.com"); i.SpareUSD == nil || *i.SpareUSD != 0 || !strings.Contains(i.SpareWhy, "under lend_min_short 30%") {
		t.Fatalf("i spare %v (%s)", i.SpareUSD, i.SpareWhy)
	}
	h.expectPick("connect", "s1", acctP)
	h.expectPick("connect", "s2", acctP)

	h.advance(2*time.Hour + time.Minute)
	h.expectPick("connect", "s3", acctI)
}

func TestLendMinShortConfig(t *testing.T) {
	if DefaultConfig().LendMinShort != 0.3 {
		t.Fatalf("default %g", DefaultConfig().LendMinShort)
	}
	cfg, err := ParseConfig([]byte("lend_min_short: 0"))
	if err != nil || cfg.LendMinShort != 0 {
		t.Fatalf("0 turns it off: %v %g", err, cfg.LendMinShort)
	}
	cfg, err = ParseConfig([]byte("lend_min_short: 0.5"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(cfg.Doc())
	if back, err := ParseConfig(raw); err != nil || back.LendMinShort != 0.5 {
		t.Fatalf("round trip: %v %+v (%s)", err, back.LendMinShort, raw)
	}
	if _, err := ParseConfig([]byte("lend_min_short: 1.5")); err == nil || !strings.Contains(err.Error(), "lend_min_short") {
		t.Fatalf("err %v", err)
	}
}
