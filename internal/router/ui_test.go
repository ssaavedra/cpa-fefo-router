package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// uiFixture is what ui.html fetches: GET /fefo/status?decisions=200 and GET
// /fefo/config, through the same management handler CPA calls.
func uiFixture(t *testing.T, h *harness) map[string]json.RawMessage {
	t.Helper()
	hd := &Handler{Router: h.r}
	get := func(path string, query url.Values) json.RawMessage {
		resp := hd.management(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management" + path, Query: query})
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, resp.Body)
		}
		return resp.Body
	}
	return map[string]json.RawMessage{
		"status": get(pathStatus, url.Values{"decisions": {"200"}}),
		"config": get(pathConfig, nil),
	}
}

// observeShortOnly feeds a response that carries the 5h window but no weekly one.
func (h *harness) observeShortOnly(client, account string, shortUsed float64, shortReset time.Time) {
	hdr := http.Header{}
	set := func(k, v string) { hdr.Set("Anthropic-Ratelimit-Unified-"+k, v) }
	set("Status", "allowed")
	set("5h-Utilization", fmt.Sprint(shortUsed))
	set("5h-Reset", fmt.Sprint(shortReset.Unix()))
	set("5h-Status", "allowed")
	h.r.ObserveUsage(pluginapi.UsageRecord{Provider: "claude", AuthID: account, APIKey: keys[client], ResponseHeaders: hdr, Detail: pluginapi.UsageDetail{TotalTokens: 10}})
}

// uiScenarios builds the router states the management page must render.
func uiScenarios(t *testing.T) map[string]*harness {
	t.Helper()
	out := map[string]*harness{}

	// No history and no quota data: every account is a probe, spare and
	// expected use are unknown.
	fresh := newHarness(t, "")
	fresh.pick("pni", "s1")
	fresh.pick("bot", "s2")
	out["fresh"] = fresh

	// Quota data, but no weekly window on d and i yet: the projection leaves
	// them out.
	noWeek := newHarness(t, "")
	noWeek.observeShortOnly("pni", acctD, 0.3, noWeek.now.Add(2*time.Hour))
	noWeek.observeShortOnly("pni", acctI, 0.1, noWeek.now.Add(4*time.Hour))
	noWeek.observe("connect", acctP, 0.2, noWeek.now.Add(48*time.Hour), 0.1, noWeek.now.Add(3*time.Hour), 100)
	noWeek.pick("pni", "s1")
	noWeek.pick("connect", "s2")
	out["no_weekly_window"] = noWeek

	// Demand known and zero, every account used up: use, waste, shortfall,
	// demand and utilisation are all zero.
	zero := newHarness(t, "")
	start := zero.now
	zero.buildHistory(7, 0, 0, start.Add(80*time.Hour), start.Add(30*time.Hour), start.Add(60*time.Hour), 1, 1, 1)
	zero.pick("pni", "s1")
	out["zero_projection"] = zero

	// Lending: i resets soon with quota pni does not need, so connect and pni
	// burn it first; an unknown key falls back to last-resort; overrides, a
	// runtime config and a config.yaml backup are active.
	path := filepath.Join(t.TempDir(), "state.json")
	lend := newHarness(t, "state_file: "+path)
	start = lend.now
	lend.buildHistory(7, 0.004, 0.01, start.Add(80*time.Hour), start.Add(11*time.Hour), start.Add(60*time.Hour), 0.2, 0.3, 0.2)
	lend.pick("connect", "s1")
	lend.pick("pni", "s2")
	lend.pick("ssdev", "s3")
	lend.pick("bot", "s4")
	for _, body := range []string{
		`{"client":"connect","policy":"any","for":"2h","note":"smoke"}`,
		`{"client":"ssdev","policy":"fail"}`,
		`{"spare_factor":2,"for":"30m"}`,
	} {
		if _, err := lend.r.ApplyOverride([]byte(body)); err != nil {
			t.Fatalf("override %s: %v", body, err)
		}
	}
	doc := lend.r.Config().Doc()
	doc.SpareFactor = 1.8
	runtimeConfig, _ := json.Marshal(map[string]any{"config": doc, "note": "smoke runtime config"})
	if _, err := lend.r.UpdateConfig(runtimeConfig); err != nil {
		t.Fatal(err)
	}
	promote, _ := json.Marshal(map[string]any{"promote": map[string]any{"config_yaml": fmt.Sprintf(cpaConfigYAML, path)}})
	if _, err := lend.r.UpdateConfig(promote); err != nil {
		t.Fatal(err)
	}
	out["lending"] = lend

	// A Fable allowance from the usage report, part spent since the poll and
	// one account's allowance used up.
	week := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC).Add(72 * time.Hour)
	fab, _, _ := pollingHarness(t, "capacity_usd: {d@example.com: 1000}\npricing: {sync: false}", map[string]string{
		"d": report(20, week, 90, week.Add(-24*time.Hour), true),
		"i": report(20, week, 0, week, false),
		"p": report(20, week, -1, week, false),
	})
	fab.seed(0.2, 0.2, 0.2, week, week, week)
	fab.refresh()
	fab.pickModel("pni", "s1", fable)
	fab.use("pni", acctD, fable, pluginapi.UsageDetail{InputTokens: 5_000_000}, 0.2, week)
	fab.pickModel("pni", "s1", fable)
	fab.pickModel("connect", "s2", fable)
	out["fable"] = fab

	return out
}

// requireScenarios checks the scenarios still show what they are named for,
// so a change to the router cannot quietly turn them into duplicates.
func requireScenarios(t *testing.T, hs map[string]*harness) {
	t.Helper()
	st := func(name string) Status { return hs[name].r.Status(200) }

	for _, a := range st("fresh").Accounts {
		if !a.NoData || a.ExpectedUSD != nil || a.SpareUSD != nil {
			t.Fatalf("fresh: account %s has data: %+v", a.Account, a)
		}
	}
	if p := st("no_weekly_window").Projections[0]; len(p.Excluded) != 2 {
		t.Fatalf("no_weekly_window: projection excludes %v", p.Excluded)
	}
	p := st("zero_projection").Projections[0]
	if !p.DemandKnown || p.DemandUSDPerHour != 0 || p.UseUSD != 0 || p.WastedUSD != 0 || p.ShortfallUSD != 0 || p.Utilisation != 0 {
		t.Fatalf("zero_projection: %+v", p)
	}

	lend := st("lending")
	roles := map[string]bool{}
	for _, c := range lend.Clients {
		for _, plan := range c.Plans {
			for _, e := range plan {
				roles[e.Role] = true
			}
		}
	}
	for _, d := range lend.Decisions {
		roles[d.Role] = true
	}
	for _, r := range []string{RoleOwn, RoleSpareFirst, RoleSpare, RoleLastResort} {
		if !roles[r] {
			t.Fatalf("lending: no %s role in plans or decisions (%v)", r, roles)
		}
	}
	if len(lend.Overrides) != 3 || len(lend.UnknownKeys) == 0 || len(lend.Flows) == 0 {
		t.Fatalf("lending: overrides %d, unknown keys %d, flows %d", len(lend.Overrides), len(lend.UnknownKeys), len(lend.Flows))
	}
	if v := hs["lending"].r.ConfigView(); v.Source != "runtime" || len(v.Backups) == 0 {
		t.Fatalf("lending: config source %s, %d backups", v.Source, len(v.Backups))
	}

	scoped := false
	for _, a := range st("fable").Accounts {
		scoped = scoped || (a.Usage != nil && len(a.Usage.Scoped) > 0)
	}
	if !scoped {
		t.Fatal("fable: no scoped allowance in the status")
	}
}

// TestUIRendersStatus renders ui.html in Node against the JSON the Go code
// produces, in both display units, and fails on anything the page cannot
// render. Node is optional locally and required in CI.
func TestUIRendersStatus(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required in CI for the UI smoke test")
		}
		t.Skip("node not installed")
	}
	hs := uiScenarios(t)
	requireScenarios(t, hs)

	dir := t.TempDir()
	for name, h := range hs {
		raw, err := json.Marshal(uiFixture(t, h))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command(node, filepath.Join("testdata", "ui_smoke.mjs"), "ui.html", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("UI smoke test failed (%v):\n%s", err, strings.TrimSpace(string(out)))
	}
	t.Logf("%s", out)
}
