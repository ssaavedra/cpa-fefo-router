package router

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// report builds a /api/oauth/usage body. fableUsedPct < 0 leaves the plan
// without a Fable allowance.
func report(weeklyUsedPct float64, weeklyReset time.Time, fableUsedPct float64, fableReset time.Time, extraUsage bool) string {
	limits := fmt.Sprintf(`{"kind":"weekly_all","percent":%g,"resets_at":%q}`, weeklyUsedPct, weeklyReset.Format(time.RFC3339))
	shares := "null"
	if fableUsedPct >= 0 {
		limits += fmt.Sprintf(`,{"kind":"weekly_scoped","percent":%g,"resets_at":%q,"scope":{"model":{"id":null,"display_name":"Fable"}}}`, fableUsedPct, fableReset.Format(time.RFC3339))
		shares = `[{"limit_index":1,"allowance_percent_of_weekly":50}]`
	}
	return fmt.Sprintf(`{"limits":[%s],"weekly_scoped_shares":%s,"extra_usage":{"is_enabled":%v,"spend_limit_reached":false}}`, limits, shares, extraUsage)
}

func (h *harness) pickModel(client, session, model string) (pluginapi.SchedulerPickResponse, Decision) {
	cands := []pluginapi.SchedulerAuthCandidate{}
	for _, id := range []string{acctD, acctI, acctP} {
		cands = append(cands, pluginapi.SchedulerAuthCandidate{ID: id, Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}})
	}
	md := map[string]any{"caller_scope": callerScope(keys[client]), "canonical_session_id": session}
	resp := h.r.Pick(pluginapi.SchedulerPickRequest{Provider: "claude", Model: model, Candidates: cands, Options: pluginapi.SchedulerOptions{Metadata: md}})
	ds := h.r.Decisions()
	return resp, ds[len(ds)-1]
}

func (h *harness) expectModelPick(client, session, model, want string) Decision {
	h.t.Helper()
	resp, d := h.pickModel(client, session, model)
	if resp.AuthID != want {
		h.t.Fatalf("%s %s: picked %q, want %q\n%s\nranking %v\nexcluded %v %v", client, model, resp.AuthID, want, d.Reason, d.Ranking, d.Excluded, d.ScopedExcluded)
	}
	return d
}

const fable = "claude-fable-5" // $10 per 1M input tokens

func TestFableStopsBeforeItsAllowanceRunsIntoExtraUsage(t *testing.T) {
	week := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC).Add(72 * time.Hour)
	h, _, logs := pollingHarness(t, "capacity_usd: {d@example.com: 1000}\npricing: {sync: false}", map[string]string{
		// d: 10% of its Fable allowance ($500 of $1000) left, resets first.
		"d": report(20, week, 90, week.Add(-24*time.Hour), true),
		"i": report(20, week, 0, week, true),
	})
	h.seed(0.2, 0.2, 0.2, week, week, week)
	h.refresh()

	d := h.expectModelPick("pni", "s1", fable, acctD)
	if !strings.Contains(d.Reason, "fable ~10% left") {
		t.Fatalf("reason does not show the Fable estimate: %s", d.Reason)
	}
	// $40 of Fable since the poll: ~2% of the allowance left, still above min_remaining.
	h.use("pni", acctD, fable, pluginapi.UsageDetail{InputTokens: 4_000_000}, 0.2, week)
	h.expectModelPick("pni", "s1", fable, acctD)
	// $10 more exhausts the estimate: the pinned session moves, without a new poll.
	h.use("pni", acctD, fable, pluginapi.UsageDetail{InputTokens: 1_000_000}, 0.2, week)
	d = h.expectModelPick("pni", "s1", fable, acctI)
	if d.Session != "moved" || len(d.ScopedExcluded) != 1 || !strings.Contains(d.ScopedExcluded[0], "d@example.com: fable allowance") {
		t.Fatalf("session %s, scoped exclusions %v", d.Session, d.ScopedExcluded)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "d@example.com: fable allowance ~0.0% left") {
		t.Fatalf("decision log does not say why d was excluded:\n%s", strings.Join(*logs, "\n"))
	}
	// Other models on d are not affected.
	h.expectModelPick("pni", "s2", "claude-opus-5-5", acctD)
	if v := h.account("d@example.com").Usage.Scoped[0]; v.Blocked == "" || !closeTo(v.SpentSinceUSD, 50, 1e-9) {
		t.Fatalf("status scoped view %+v", v)
	}
}

func TestFablePrefersTheAllowanceThatExpiresFirst(t *testing.T) {
	week := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC).Add(72 * time.Hour)
	h, _, _ := pollingHarness(t, "pricing: {sync: false}", map[string]string{
		"d": report(20, week, 0, week, true),
		"i": report(20, week, 0, week.Add(-48*time.Hour), true),
	})
	h.seed(0.2, 0.2, 0.2, week, week, week)
	h.refresh()
	h.expectModelPick("pni", "s1", fable, acctI)
	h.expectModelPick("pni", "s2", "claude-opus-5-5", acctD) // bucket order as before
}

func TestFableNeverBillsCreditsOnAPlanWithoutAllowance(t *testing.T) {
	week := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC).Add(72 * time.Hour)

	// Not polled yet: nothing is known, so p serves Fable.
	h := newHarness(t, "")
	h.seed(0.2, 0.2, 0.2, week, week, week)
	h.expectModelPick("connect", "s0", fable, acctP)

	h, host, _ := pollingHarness(t, "pricing: {sync: false}", map[string]string{"p": report(20, week, -1, week, true)})
	h.seed(0.2, 0.2, 0.2, week, week, week)
	h.refresh()
	resp, d := h.pickModel("connect", "s1", fable)
	if !resp.Reject || !strings.Contains(strings.Join(d.ScopedExcluded, " "), "p@example.com: no fable allowance on this plan and extra usage is enabled") {
		t.Fatalf("Fable on a plan without allowance: %+v %+v", resp, d)
	}
	h.expectModelPick("connect", "s2", "claude-opus-5-5", acctP)

	// A client with policy any is rejected too, not handed to CPA's own pick.
	resp = h.r.Pick(pluginapi.SchedulerPickRequest{Provider: "claude", Model: fable,
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: acctP, Provider: "claude"}},
		Options:    pluginapi.SchedulerOptions{Metadata: map[string]any{"caller_scope": callerScope(keys["bot"])}}})
	if !resp.Reject {
		t.Fatalf("any client may reach p for Fable: %+v", resp)
	}

	// With extra usage off nothing can be billed: p is allowed again.
	host.respond(claudeUsageURL, 200, report(20, week, -1, week, false))
	h.advance(5 * time.Minute)
	h.refresh()
	h.expectModelPick("connect", "s3", fable, acctP)
}
