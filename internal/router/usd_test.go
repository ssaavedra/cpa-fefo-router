package router

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/ssaavedra/cpa-fefo-router/internal/pricing"
)

// use feeds one priced response: model and token detail plus a weekly reading.
func (h *harness) use(client, account, model string, d pluginapi.UsageDetail, weeklyUsed float64, weeklyReset time.Time) {
	hdr := http.Header{}
	set := func(k, v string) { hdr.Set("Anthropic-Ratelimit-Unified-"+k, v) }
	set("Status", "allowed")
	set("7d-Utilization", fmt.Sprint(weeklyUsed))
	set("7d-Reset", fmt.Sprint(weeklyReset.Unix()))
	set("7d-Status", "allowed")
	h.r.ObserveUsage(pluginapi.UsageRecord{
		Provider: "claude", AuthID: account, APIKey: keys[client], Model: model,
		ResponseHeaders: hdr, Detail: d,
	})
}

func (h *harness) demand(client string) rateInfo {
	h.t.Helper()
	for _, c := range h.r.Status(0).Clients {
		if c.Client == client {
			return c.Demand["claude"]
		}
	}
	h.t.Fatalf("client %s not in status", client)
	return rateInfo{}
}

func closeTo(a, b, tol float64) bool { return math.Abs(a-b) <= tol*math.Max(math.Abs(a), math.Abs(b)) }

func TestWeeklyUseIsSplitByCostNotTokens(t *testing.T) {
	h := newHarness(t, "")
	h.r.observeStart = h.now.Add(-24 * time.Hour)
	reset := h.now.Add(100 * time.Hour)
	h.use("pni", acctD, "claude-opus-5-5", pluginapi.UsageDetail{}, 0.10, reset)
	h.advance(time.Minute)
	// 1M cache-read tokens of Opus 5.5 cost $0.20; 20k output tokens cost $0.40.
	h.use("pni", acctD, "claude-opus-5-5", pluginapi.UsageDetail{CacheReadTokens: 1_000_000}, 0.10, reset)
	h.advance(time.Minute)
	h.use("connect", acctD, "claude-opus-5-5", pluginapi.UsageDetail{OutputTokens: 20_000}, 0.16, reset)

	pni, conn := h.demand("pni"), h.demand("connect")
	if !pni.Known || pni.PerHour <= 0 || !closeTo(conn.PerHour/pni.PerHour, 2, 1e-9) {
		t.Fatalf("output-heavy client should carry 2x the cache-heavy one: pni %+v connect %+v", pni, conn)
	}
}

func TestUnknownModelChargesHomeClientHighAndBorrowerLow(t *testing.T) {
	h := newHarness(t, "")
	h.r.observeStart = h.now.Add(-24 * time.Hour)
	reset := h.now.Add(100 * time.Hour)
	const model = "claude-fable-9"
	detail := pluginapi.UsageDetail{InputTokens: 10_000, OutputTokens: 5_000}
	q := embeddedPrices.Cost(model, pricing.NormalizeUsage("claude", detail))
	if q.Match != pricing.Unknown || !(q.Low < q.High) {
		t.Fatalf("precondition: %s should be quoted as a range, got %+v", model, q)
	}
	h.use("pni", acctD, "claude-opus-5-5", pluginapi.UsageDetail{}, 0.10, reset)
	h.advance(time.Minute)
	h.use("pni", acctD, model, detail, 0.10, reset) // pni's bucket includes d: home
	h.advance(time.Minute)
	h.use("connect", acctD, model, detail, 0.20, reset) // connect borrows d

	pni, conn := h.demand("pni"), h.demand("connect")
	if !closeTo(pni.PerHour/conn.PerHour, q.High/q.Low, 1e-9) {
		t.Fatalf("home/borrower ratio %.4f, want High/Low %.4f", pni.PerHour/conn.PerHour, q.High/q.Low)
	}
}

func (h *harness) account(name string) AccountView {
	h.t.Helper()
	for _, a := range h.r.Status(0).Accounts {
		if a.Account == name {
			return a
		}
	}
	h.t.Fatalf("account %s not in status", name)
	return AccountView{}
}

func TestCapacityIsLearnedPerWeeklyCycle(t *testing.T) {
	h := newHarness(t, "")
	used, reset := 0.0, h.now.Add(100*time.Hour)
	at := func(clock time.Time) {
		h.now = clock
		h.use("pni", acctD, "claude-opus-5-5", pluginapi.UsageDetail{}, used, reset)
	}
	burn := func(n int, model string, input int64, step float64) {
		for i := 0; i < n; i++ {
			h.advance(5 * time.Minute)
			used += step
			h.use("pni", acctD, model, pluginapi.UsageDetail{InputTokens: input}, used, reset)
		}
	}
	expect := func(usd float64, source string) {
		t.Helper()
		a := h.account("d@example.com")
		if !closeTo(a.CapacityUSD, usd, 1e-9) || a.CapacitySource != source {
			t.Fatalf("capacity $%.2f (%s: %s), want $%.2f (%s)", a.CapacityUSD, a.CapacitySource, a.CapacityWhy, usd, source)
		}
	}
	start := h.now
	at(start)
	expect(220, CapacityPrior)
	// Opus 5.5 input is $4/M: 11 requests of 1M tokens, 1% of the week each.
	burn(11, "claude-opus-5-5", 1_000_000, 0.01)
	at(start.Add(time.Hour)) // the hour is complete
	expect(400, CapacityCurrent)

	// An hour with an unpriced model is left out, however much it used.
	burn(5, "claude-fable-9", 1_000_000, 0.05)
	at(start.Add(2 * time.Hour))
	expect(400, CapacityCurrent)

	// New weekly cycle at twice the dollars per percent. Until it has seen
	// learn_min_delta (5%) the last cycle's estimate stays in use.
	used, reset = 0, reset.Add(7*24*time.Hour)
	at(start.Add(3 * time.Hour))
	burn(3, "claude-opus-5-5", 2_000_000, 0.01)
	at(start.Add(4 * time.Hour))
	expect(400, CapacityPrevious)
	burn(3, "claude-opus-5-5", 2_000_000, 0.01)
	at(start.Add(5 * time.Hour))
	expect(800, CapacityCurrent)
}

func TestPriceSyncAddsModelsAndKeepsTheTableOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	h := newHarness(t, "state_file: "+path)
	host := newFakeHost()
	h.r.fetcher = host
	host.respond(modelsDevURL, 200, `{"anthropic":{"models":{"claude-zeta-1":{"family":"claude-zeta","cost":{"input":2,"output":10}}}},
		"mistral":{"models":{"claude-zeta-1":{"cost":{"input":99,"output":99}}}}}`)
	reset := h.now.Add(100 * time.Hour)
	zeta := pluginapi.UsageDetail{InputTokens: 1_000_000}
	const series = `fefo_cost_usd_total{client="pni",account="d@example.com"}`

	h.refresh()
	if p := h.r.Status(0).Pricing; p.Source != "models.dev" || p.SyncError != "" || p.SyncedAt.IsZero() {
		t.Fatalf("after sync: %+v", p)
	}
	h.use("pni", acctD, "claude-zeta-1", zeta, 0.1, reset)
	requireMetric(t, h, series, "2")

	h.advance(25 * time.Hour)
	host.respond(modelsDevURL, 500, "upstream down")
	h.refresh()
	if p := h.r.Status(0).Pricing; p.SyncError == "" || p.Source != "models.dev" {
		t.Fatalf("after failed sync: %+v", p)
	}
	h.use("pni", acctD, "claude-zeta-1", zeta, 0.1, reset)
	requireMetric(t, h, series, "4")
	if !strings.Contains(h.r.Status(0).Text(), "price sync error") {
		t.Fatal("text status does not show the sync error")
	}

	// The synced table survives a restart, with syncing off.
	h.r.saveState()
	h2 := newHarness(t, "state_file: "+path+"\npricing: {sync: false}")
	h2.use("pni", acctD, "claude-zeta-1", zeta, 0.1, reset)
	requireMetric(t, h2, series, "2")
}

func TestPriceOverridesWin(t *testing.T) {
	h := newHarness(t, "prices: {claude-opus-5-5: {input: 10, output: 50}}")
	h.use("pni", acctD, "claude-opus-5-5", pluginapi.UsageDetail{InputTokens: 1_000_000}, 0.1, h.now.Add(time.Hour))
	requireMetric(t, h, `fefo_cost_usd_total{client="pni",account="d@example.com"}`, "10")
	if _, err := ParseConfig([]byte("prices: {x: {input: -1}}")); err == nil {
		t.Fatal("negative price accepted")
	}
}

func TestStatusUnitsAndUnknownModelWarning(t *testing.T) {
	h := newHarness(t, "display_units: tokens")
	h.r.observeStart = h.now.Add(-24 * time.Hour)
	reset := h.now.Add(100 * time.Hour)
	h.use("pni", acctD, "claude-opus-5-5", pluginapi.UsageDetail{}, 0.10, reset)
	h.advance(time.Minute)
	h.use("pni", acctD, "claude-opus-5-5", pluginapi.UsageDetail{InputTokens: 48_000}, 0.11, reset)
	h.use("pni", acctD, "claude-fable-9", pluginapi.UsageDetail{InputTokens: 1_000}, 0.11, reset)
	hd := &Handler{Router: h.r}
	text := func(query string) string {
		q, _ := url.ParseQuery(query)
		return string(hd.management(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/fefo/status", Query: q}).Body)
	}
	for _, tc := range []struct {
		query     string
		want, not []string
	}{
		{"format=text", []string{"tokens 24h", "demand 2.0k tok/h"}, []string{"demand $"}},
		{"format=text&units=usd", []string{"remaining", "demand $"}, []string{"tok/h"}},
		{"format=text&units=tokens", []string{"49.0k", "tok/h"}, []string{"demand $"}},
	} {
		got := text(tc.query)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Fatalf("%s: missing %q:\n%s", tc.query, w, got)
			}
		}
		for _, w := range tc.not {
			if strings.Contains(got, w) {
				t.Fatalf("%s: unexpected %q:\n%s", tc.query, w, got)
			}
		}
	}
	var st Status
	if err := json.Unmarshal([]byte(text("")), &st); err != nil {
		t.Fatal(err)
	}
	if d := st.Clients[1].Demand["claude"]; st.Clients[1].Client != "pni" || d.PerHour <= 0 || d.TokensPerHour != 49_000.0/24 {
		t.Fatalf("status JSON should carry dollars and tokens: %+v", st.Clients[1])
	}
	warned := false
	for _, w := range st.Warnings {
		warned = warned || strings.Contains(w, "claude-fable-9 is not in the price table: 1 request(s) priced as the claude-fable range, $0.01–$0.01")
	}
	if !warned {
		t.Fatalf("no unknown-model warning: %v", st.Warnings)
	}
	requireMetric(t, h, `fefo_unknown_model_requests_total{model="claude-fable-9"}`, "1")
	requireMetric(t, h, `fefo_client_demand_per_hour{client="pni",provider="claude",window="24h",unit="tokens"}`, fmt.Sprint(49_000.0/24))
	if h.metric(`fefo_account_capacity_usd{account="d@example.com",provider="claude",source="prior"}`) != "220" {
		t.Fatal(h.r.Prometheus())
	}
}

func TestPinnedCapacityAndSpareInDollars(t *testing.T) {
	for _, tc := range []struct {
		minSpare float64
		lends    bool
	}{{0.6, true}, {0.8, false}} {
		h := newHarness(t, fmt.Sprintf("capacity_usd: {i@example.com: 1000}\nmin_spare: %g", tc.minSpare))
		start := h.now
		h.buildHistory(7, 0.01, 0.01, start.Add(80*time.Hour), start.Add(11*time.Hour), start.Add(60*time.Hour), 0.2, 0.3, 0.2)
		i := h.account("i@example.com")
		if i.CapacityUSD != 1000 || i.CapacitySource != CapacityPinned || !closeTo(i.RemainingUSD, 700, 1e-9) {
			t.Fatalf("i: capacity $%.2f (%s), remaining $%.2f", i.CapacityUSD, i.CapacitySource, i.RemainingUSD)
		}
		// pni's demand is absorbed by d before i resets, so all $700 is spare.
		if i.SpareUSD == nil || !closeTo(*i.SpareUSD, 700, 1e-9) {
			t.Fatalf("i spare %v (%s)", i.SpareUSD, i.SpareWhy)
		}
		// min_spare is a share of the account's capacity: $600 or $800 of $1000.
		_, d := h.pick("connect", "s1")
		if lent := d.Account == "i@example.com"; lent != tc.lends {
			t.Fatalf("min_spare %g: connect picked %s (%s), want lending=%v", tc.minSpare, d.Account, d.Reason, tc.lends)
		}
	}
}

func TestHourCarriedOverFromBeforePricingSplitsByTokens(t *testing.T) {
	h := newHarness(t, "")
	h.r.observeStart = h.now.Add(-24 * time.Hour)
	reset := h.now.Add(100 * time.Hour)
	h.use("pni", acctD, "claude-opus-5-5", pluginapi.UsageDetail{OutputTokens: 1000}, 0.10, reset)
	h.r.mu.Lock()
	b := h.r.hours[acctD][hourOf(h.now)]
	b.USDLow, b.USDHigh, b.Priced = nil, nil, false
	b.Tokens = map[string]int64{"pni": 3000}
	h.r.mu.Unlock()
	h.advance(time.Minute)
	h.use("connect", acctD, "claude-opus-5-5", pluginapi.UsageDetail{OutputTokens: 1000}, 0.14, reset)

	pni, conn := h.demand("pni"), h.demand("connect")
	if !closeTo(pni.PerHour/conn.PerHour, 3, 1e-9) {
		t.Fatalf("an hour recorded before pricing must split by tokens, not hand it all to the priced client: pni %+v connect %+v", pni, conn)
	}
}

// Demand is what a client's requests cost, so usage the weekly window does
// not show still counts against its own accounts.
func TestDemandBillsUsageTheWeeklyWindowMisses(t *testing.T) {
	const model = "claude-opus-5-5"
	detail := pluginapi.UsageDetail{OutputTokens: 20_000} // $0.40 of Opus 5.5
	usd := embeddedPrices.Cost(model, pricing.NormalizeUsage("claude", detail)).High
	reset := func(h *harness) time.Time { return h.now.Add(100 * time.Hour) }

	t.Run("first reading after an idle gap on a borrowed account", func(t *testing.T) {
		h := newHarness(t, "")
		h.r.observeStart = h.now.Add(-24 * time.Hour)
		r := reset(h)
		h.use("pni", acctI, model, pluginapi.UsageDetail{}, 0.10, r)
		h.advance(time.Hour) // i idle, then connect borrows it
		h.use("connect", acctI, model, detail, 0.12, r)
		if got := h.demand("connect"); !closeTo(got.Day, usd/24, 1e-9) {
			t.Fatalf("connect demand %+v, want $%.2f/24h", got, usd)
		}
		h.r.mu.Lock()
		pc := h.r.newPlanContextLocked("claude", h.now)
		ext := pc.rate(externalClient, acctI)
		capUSD := pc.evals[acctI].Capacity.USD
		h.r.mu.Unlock()
		if want := (0.02*capUSD - usd) / 24; !closeTo(ext.Day, want, 1e-9) {
			t.Fatalf("outside CPA %.4f/h, want %.4f/h: connect's request must not count twice", ext.Day, want)
		}
	})

	t.Run("paid extra usage", func(t *testing.T) {
		h := newHarness(t, "allow_extra_usage: true")
		h.r.observeStart = h.now.Add(-24 * time.Hour)
		r := reset(h)
		h.use("connect", acctP, model, pluginapi.UsageDetail{}, 1.0, r)
		h.advance(time.Minute)
		h.use("connect", acctP, model, detail, 1.0, r) // window full: no delta
		if got := h.demand("connect"); !closeTo(got.Day, usd/24, 1e-9) {
			t.Fatalf("connect demand %+v, want $%.2f/24h", got, usd)
		}
	})

	t.Run("API-key account", func(t *testing.T) {
		h := newHarness(t, "")
		h.r.observeStart = h.now.Add(-24 * time.Hour)
		h.r.ObserveUsage(pluginapi.UsageRecord{
			Provider: "claude", AuthID: "key-account", APIKey: keys["connect"], Model: model, Detail: detail,
		})
		if got := h.demand("connect"); !closeTo(got.Day, usd/24, 1e-9) {
			t.Fatalf("connect demand %+v, want $%.2f/24h", got, usd)
		}
	})
}
