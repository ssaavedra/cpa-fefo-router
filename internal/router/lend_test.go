package router

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestLendLimitsConfig(t *testing.T) {
	cfg := testConfig(t, "lend_limits: {i: {any: 0.2, p: {7d: 0.1, 8h: 0.6}}}\nlend_reserve: 0.3\nstale_after: 1d")
	want := map[string][]ReservePoint{"any": {{0, 0.2}}, "p": {{8 * time.Hour, 0.6}, {7 * 24 * time.Hour, 0.1}}}
	if fmt.Sprint(cfg.LendLimits["i"]) != fmt.Sprint(want) {
		t.Fatalf("limits %v, want %v", cfg.LendLimits, want)
	}
	if fmt.Sprint(cfg.LendReserve) != fmt.Sprint([]ReservePoint{{0, 0.3}}) || cfg.StaleAfter != 24*time.Hour {
		t.Fatalf("reserve %v, stale_after %s", cfg.LendReserve, cfg.StaleAfter)
	}
	raw, _ := json.Marshal(cfg.Doc())
	for _, w := range []string{`"lend_limits":{"i":{"any":0.2,"p":{"7d":0.1,"8h":0.6}}}`, `"lend_reserve":0.3`} {
		if !strings.Contains(string(raw), w) {
			t.Fatalf("doc lacks %s: %s", w, raw)
		}
	}
	back, err := ParseConfig(raw)
	if err != nil || fmt.Sprint(back.LendLimits) != fmt.Sprint(cfg.LendLimits) || fmt.Sprint(back.LendReserve) != fmt.Sprint(cfg.LendReserve) {
		t.Fatalf("round trip: %v %v %v", err, back.LendLimits, back.LendReserve)
	}
	for bad, msg := range map[string]string{
		"lend_limits: {x: {any: 0.2}}":        "lend_limits.x: unknown bucket",
		"lend_limits: {i: {i: 0.2}}":          "does not lend to itself",
		"lend_limits: {i: {zz: 0.2}}":         `unknown bucket "zz"`,
		"lend_limits: {i: {any: 1.2}}":        "lend_limits.i.any: share must be",
		"lend_limits: {i: {any: {7x: 0.2}}}":  "want a time left",
		"lend_limits: {i: {any: {7d: -0.1}}}": "lend_limits.i.any.7d: share must be",
		"lend_reserve: yes":                   "lend_reserve: share must be",
		"display_units: pct":                  "display_units",
	} {
		y := "buckets: {i: [i@example.com], p: [p@example.com]}\n" + bad
		if _, err := ParseConfig([]byte(y)); err == nil || !strings.Contains(err.Error(), msg) {
			t.Fatalf("%s: err %v, want %q", bad, err, msg)
		}
	}
}

// lendingSetup is TestSpareThatExpiresFirstIsUsedBeforeOwnBucket's world with
// i's capacity pinned at $1000: i resets in 4h with 70% left, so connect
// borrows it first.
func lendingSetup(t *testing.T, extra string) (*harness, time.Time) {
	h := newHarness(t, "capacity_usd: {i@example.com: 1000}\n"+extra)
	start := h.now
	iReset := start.Add(11 * time.Hour)
	h.buildHistory(7, 0.01, 0.01, start.Add(80*time.Hour), iReset, start.Add(60*time.Hour), 0.2, 0.3, 0.2)
	return h, iReset
}

func TestBucketLendLimitStopsLendingOnceReached(t *testing.T) {
	h, iReset := lendingSetup(t, "lend_limits: {i: {any: 0.05}}")
	d := h.expectPick("connect", "s1", acctI)
	if d.Role != RoleSpareFirst || !strings.Contains(d.Reason, "bucket limit lets it lend $50.00 more") {
		t.Fatalf("role %s: %s", d.Role, d.Reason)
	}
	// connect spends $60 of output on i ($20 per 1M): past the $50 the limit
	// allows.
	h.advance(time.Minute)
	h.use("connect", acctI, "claude-opus-5-5", pluginapi.UsageDetail{OutputTokens: 3_000_000}, 0.36, iReset)

	var lim LendLimitView
	for _, l := range h.r.Status(0).LendLimits {
		if l.Bucket == "i" && l.Target == LendAny {
			lim = l
		}
	}
	if lim.Open || !closeTo(lim.AllowedUSD, 50, 1e-9) || !closeTo(lim.CapacityUSD, 1000, 1e-9) || lim.LentUSD < 50 {
		t.Fatalf("limit %+v", lim)
	}
	// The pinned borrower moves to its own bucket, new sessions too.
	for _, s := range []string{"s1", "s2"} {
		_, d = h.pick("connect", s)
		if d.Account != "p@example.com" || !strings.Contains(strings.Join(d.Excluded, ";"), "bucket i has lent") {
			t.Fatalf("%s: connect got %s (%s), excluded %v", s, d.Account, d.Reason, d.Excluded)
		}
	}
	// i's own client is not limited.
	if d := h.expectPick("pni", "s3", acctI); d.Role != RoleSpareFirst {
		t.Fatalf("pni role %s", d.Role)
	}
	requireMetric(t, h, `fefo_bucket_lend_allowed_usd{bucket="i",target="any",provider="claude"}`, "50")
}

func TestBucketLendLimitPerTarget(t *testing.T) {
	// connect's bucket is p: a limit towards d leaves it alone, one towards p
	// at 0 stops it.
	h, _ := lendingSetup(t, "lend_limits: {i: {d: 0}}")
	h.expectPick("connect", "s1", acctI)

	h, _ = lendingSetup(t, "lend_limits: {i: {p: 0}}")
	h.expectPick("connect", "s1", acctP)
}

func TestBucketLendLimitFollowsTimeLeft(t *testing.T) {
	// i has 4h left: below the 1d point, so half its week may be lent.
	h, _ := lendingSetup(t, "lend_limits: {i: {any: {7d: 0, 1d: 0.5}}}")
	ls := h.r.Status(0).LendLimits
	if len(ls) != 1 || !closeTo(ls[0].AllowedUSD, 500, 1e-9) || !closeTo(ls[0].ShareNow, 0.5, 1e-9) || ls[0].LentUSD != 0 {
		t.Fatalf("limits %+v", ls)
	}
}

func TestBucketLendLimitHoldsForLastResort(t *testing.T) {
	// pni's own d and i are exhausted; under policy any it would fall back to
	// p, but p lends nothing.
	for _, tc := range []struct {
		extra string
		want  string
	}{{"", acctP}, {"lend_limits: {p: {any: 0}}", ""}} {
		h := newHarness(t, tc.extra)
		h.seed(1.0, 1.0, 0.2, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(48*time.Hour))
		resp, d := h.pick("pni", "s1")
		if resp.AuthID != tc.want || (tc.want == "" && !resp.Reject) {
			t.Fatalf("%q: picked %q (%s %s)", tc.extra, resp.AuthID, d.Outcome, d.Reason)
		}
	}
}

func TestStatusTextInPercentUnits(t *testing.T) {
	h, _ := lendingSetup(t, "lend_limits: {i: {any: 0.05}}")
	hd := &Handler{Router: h.r}
	text := func(units string) string {
		q, _ := url.ParseQuery("format=text&units=" + units)
		return string(hd.management(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/fefo/status", Query: q}).Body)
	}
	rel, abs := text(UnitsPctRelative), text(UnitsPctAbsolute)
	for _, w := range []string{"% of each account's own week", "of the own accounts' week per week", "share of the pool's week", "LEND LIMITS", "i → every other bucket (claude): lent 0% of 5% allowed now"} {
		if !strings.Contains(rel, w) {
			t.Fatalf("relative text lacks %q:\n%s", w, rel)
		}
	}
	// i ($1000) is the largest account; d and p have the $220 prior.
	for _, w := range []string{"largest account's week", "lent 0% of 5% allowed now"} {
		if !strings.Contains(abs, w) {
			t.Fatalf("absolute text lacks %q:\n%s", w, abs)
		}
	}
	if !strings.Contains(abs, "22%") {
		t.Fatalf("absolute text should scale d's $220 week to 22%% of i's:\n%s", abs)
	}
}
