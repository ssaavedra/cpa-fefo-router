package router

import (
	"fmt"
	"math"
	"net/http"
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
