package router

import (
	"strings"
	"testing"
	"time"
)

func TestPoolServesSoonestResetFirstAndCountsWaste(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	accts := []poolAccount{
		{balance: 100, capacity: 100, reset: now.Add(100 * time.Hour), from: now},
		{balance: 50, capacity: 100, reset: now.Add(10 * time.Hour), from: now}, // spare that expires first
	}
	// $1/h: the soon account serves 0-10h and resets with $40 unused; then
	// the far one (reset at 100h) is soonest and serves 10-100h, resetting
	// with $10 unused; then the refilled soon one (178h) serves 100-168h.
	use, wasted, short, dry := simulatePool(now, accts, 1, week)
	if !closeTo(use, 168, 1e-9) || short != 0 || !dry.IsZero() {
		t.Fatalf("use %.2f short %.2f dry %v", use, short, dry)
	}
	if !closeTo(wasted, 50, 1e-9) {
		t.Fatalf("wasted %.2f, want 50", wasted)
	}

	// $2/h exhausts the pool before anything resets.
	accts = []poolAccount{{balance: 10, capacity: 100, reset: now.Add(50 * time.Hour), from: now}}
	use, _, short, dry = simulatePool(now, accts, 2, 24*time.Hour)
	if !closeTo(use, 10, 1e-9) || !closeTo(short, 38, 1e-9) || !dry.Equal(now.Add(5*time.Hour)) {
		t.Fatalf("use %.2f short %.2f dry %v", use, short, dry)
	}
}

func TestStatusProjectsThePool(t *testing.T) {
	h := newHarness(t, "")
	start := h.now
	h.buildHistory(7, 0.01, 0.01, start.Add(80*time.Hour), start.Add(11*time.Hour), start.Add(60*time.Hour), 0.2, 0.3, 0.2)
	st := h.r.Status(0)
	if len(st.Projections) != 1 {
		t.Fatalf("projections %+v", st.Projections)
	}
	p := st.Projections[0]
	if p.Provider != "claude" || p.Accounts != 3 || !p.DemandKnown || p.DemandUSDPerHour <= 0 {
		t.Fatalf("projection %+v", p)
	}
	var cap, rem float64
	for _, a := range st.Accounts {
		cap += a.CapacityUSD
		rem += a.RemainingUSD
	}
	if !closeTo(p.CapacityUSD, cap, 1e-9) || !closeTo(p.RemainingUSD, rem, 1e-9) {
		t.Fatalf("capacity %.2f/%.2f remaining %.2f/%.2f", p.CapacityUSD, cap, p.RemainingUSD, rem)
	}
	// Spare: i resets in 4h with 70% left and nobody needs it.
	if p.SpareUSD <= 0 || p.ShortfallUSD != 0 || p.UseUSD <= 0 {
		t.Fatalf("projection %+v", p)
	}
	if !strings.Contains(st.Text(), "PROJECTION") {
		t.Fatal("text status lacks the projection")
	}
}

func TestProjectionWaitsForDemand(t *testing.T) {
	h := newHarness(t, "")
	h.seed(0.2, 0.2, 0.2, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(48*time.Hour))
	p := h.r.Status(0).Projections[0]
	if p.DemandKnown || !strings.Contains(p.Why, "demand unknown") || p.Accounts != 3 {
		t.Fatalf("projection %+v", p)
	}
}
