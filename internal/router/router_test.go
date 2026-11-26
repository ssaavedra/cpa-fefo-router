package router

import (
	"encoding/json"
	"fmt"
	"gopkg.in/yaml.v3"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Accounts and clients mirror the real deployment, with fake identities.
const (
	acctD = "claude-d.json" // bucket "d"
	acctI = "claude-i.json" // bucket "i"
	acctP = "claude-p.json" // bucket "p" (personal)
)

var keys = map[string]string{
	"pni":     "sk-pni",
	"connect": "sk-connect",
	"ssdev":   "sk-ssdev",
	"bot":     "sk-bot",
}

func testConfig(t *testing.T, extra string) Config {
	t.Helper()
	y := fmt.Sprintf(`
mode: active
min_history: 6h
buckets:
  d: [d@example.com]
  i: [i@example.com]
  p: [p@example.com]
clients:
  pni:     {keys: [%s], buckets: [d, i], on_exhausted: any}
  connect: {keys: [%s], buckets: [p], on_exhausted: fail}
  ssdev:   {keys: [%s], buckets: [p], on_exhausted: wait}
%s`, callerScope(keys["pni"]), plainSHA(keys["connect"]), callerScope(keys["ssdev"]), extra)
	cfg, err := ParseConfig([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type harness struct {
	t     *testing.T
	r     *Router
	now   time.Time
	slept time.Duration
}

func newHarness(t *testing.T, extra string) *harness {
	h := &harness{t: t, now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	h.r = New(WithClock(func() time.Time { return h.now }), WithSleep(func(d time.Duration) { h.slept += d }))
	h.r.SetConfig(testConfig(t, extra))
	h.r.mu.Lock()
	h.r.applyHostLocked([]pluginapi.HostAuthFileEntry{
		{ID: acctD, AuthIndex: "idx-d", Name: acctD, Label: "d@example.com", Provider: "claude"},
		{ID: acctI, AuthIndex: "idx-i", Name: acctI, Label: "i@example.com", Provider: "claude"},
		{ID: acctP, AuthIndex: "idx-p", Name: acctP, Label: "p@example.com", Provider: "claude"},
	}, h.now)
	h.r.mu.Unlock()
	return h
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

// observe feeds one response for client on account with weekly/5h usage.
func (h *harness) observe(client, account string, weeklyUsed float64, weeklyReset time.Time, shortUsed float64, shortReset time.Time, tokens int64) {
	hdr := http.Header{}
	set := func(k, v string) { hdr.Set("Anthropic-Ratelimit-Unified-"+k, v) }
	set("Status", "allowed")
	set("7d-Utilization", fmt.Sprint(weeklyUsed))
	set("7d-Reset", fmt.Sprint(weeklyReset.Unix()))
	set("7d-Status", "allowed")
	set("5h-Utilization", fmt.Sprint(shortUsed))
	set("5h-Reset", fmt.Sprint(shortReset.Unix()))
	set("5h-Status", "allowed")
	h.r.ObserveUsage(pluginapi.UsageRecord{
		Provider: "claude", AuthID: account, APIKey: keys[client],
		ResponseHeaders: hdr, Detail: pluginapi.UsageDetail{TotalTokens: tokens},
	})
}

func (h *harness) pick(client, session string, offered ...string) (pluginapi.SchedulerPickResponse, Decision) {
	if len(offered) == 0 {
		offered = []string{acctD, acctI, acctP}
	}
	var cands []pluginapi.SchedulerAuthCandidate
	for _, id := range offered {
		cands = append(cands, pluginapi.SchedulerAuthCandidate{ID: id, Provider: "claude", Attributes: map[string]string{"auth_kind": "oauth"}})
	}
	md := map[string]any{"caller_scope": callerScope(keys[client])}
	if session != "" {
		md["canonical_session_id"] = session
	}
	resp := h.r.Pick(pluginapi.SchedulerPickRequest{Provider: "claude", Model: "claude-opus-5-5", Candidates: cands, Options: pluginapi.SchedulerOptions{Metadata: md}})
	ds := h.r.Decisions()
	return resp, ds[len(ds)-1]
}

func (h *harness) expectPick(client, session, want string) Decision {
	h.t.Helper()
	resp, d := h.pick(client, session)
	if resp.AuthID != want {
		h.t.Fatalf("%s: picked %q, want %q\ndecision: %+v", client, resp.AuthID, want, d)
	}
	return d
}

// seed gives every account fresh quota data.
func (h *harness) seed(dUsed, iUsed, pUsed float64, dReset, iReset, pReset time.Time) {
	short := h.now.Add(3 * time.Hour)
	h.observe("pni", acctD, dUsed, dReset, 0.1, short, 0)
	h.observe("pni", acctI, iUsed, iReset, 0.1, short, 0)
	h.observe("connect", acctP, pUsed, pReset, 0.1, short, 0)
}

func TestClientsUseTheirOwnBucketsInOrder(t *testing.T) {
	h := newHarness(t, "")
	h.seed(0.2, 0.2, 0.2, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(48*time.Hour))
	h.expectPick("pni", "s1", acctD)
	h.expectPick("connect", "s2", acctP)
	h.expectPick("ssdev", "s3", acctP)

	// d exhausted on its weekly window: pni falls to its second bucket.
	h.observe("pni", acctD, 1.0, h.now.Add(72*time.Hour), 0.1, h.now.Add(3*time.Hour), 10)
	d := h.expectPick("pni", "s4", acctI)
	if d.Role != RoleOwn {
		t.Fatalf("role %s", d.Role)
	}
}

func TestNoLendingWithoutHistory(t *testing.T) {
	h := newHarness(t, "")
	// i resets in 4h with most quota unused, but history is too short to know
	// whether pni will need it: connect must not borrow it.
	h.seed(0.2, 0.1, 0.2, h.now.Add(72*time.Hour), h.now.Add(4*time.Hour), h.now.Add(48*time.Hour))
	h.expectPick("connect", "s1", acctP)
}

// buildHistory simulates hours of traffic: pni burns `pniRate` of a plan-week
// per hour on d; connect burns `connRate` per hour on p.
func (h *harness) buildHistory(hours int, pniRate, connRate float64, dReset, iReset, pReset time.Time, d0, i0, p0 float64) (float64, float64, float64) {
	dU, iU, pU := d0, i0, p0
	// Each client sends a request every 5 minutes, as an active agent does.
	for k := 0; k < hours*12; k++ {
		h.advance(5 * time.Minute)
		dU += pniRate / 12
		pU += connRate / 12
		short := h.now.Add(3 * time.Hour)
		h.observe("pni", acctD, dU, dReset, 0.2, short, 1000)
		h.observe("connect", acctP, pU, pReset, 0.2, short, 1000)
		if k%12 == 0 {
			h.observe("pni", acctI, iU, iReset, 0.0, short, 0) // i idle, data refreshed hourly
		}
	}
	return dU, iU, pU
}

func TestSpareThatExpiresFirstIsUsedBeforeOwnBucket(t *testing.T) {
	h := newHarness(t, "")
	start := h.now
	dReset, iReset, pReset := start.Add(80*time.Hour), start.Add(11*time.Hour), start.Add(60*time.Hour)
	h.buildHistory(7, 0.01, 0.01, dReset, iReset, pReset, 0.2, 0.3, 0.2)
	// Now i resets in 4h with 70% left. pni needs 0.01/h and d (its first
	// bucket) has plenty, so none of that reaches i: i is spare and expires
	// before p, so connect burns it first.
	d := h.expectPick("connect", "s1", acctI)
	if d.Role != RoleSpareFirst {
		t.Fatalf("role %s (%s)", d.Role, d.Reason)
	}
	// pni also burns i first: it is pni's own second bucket, its quota is not
	// needed before it resets, and it expires before d.
	d = h.expectPick("pni", "s2", acctI)
	if d.Role != RoleSpareFirst {
		t.Fatalf("pni role %s (%s)", d.Role, d.Reason)
	}
}

func TestNoLendingWhenHomeClientsNeedIt(t *testing.T) {
	h := newHarness(t, "")
	start := h.now
	// d nearly exhausted and pni burns fast: pni's demand overflows d into i
	// before i resets, so i keeps its quota for pni.
	dReset, iReset, pReset := start.Add(80*time.Hour), start.Add(20*time.Hour), start.Add(60*time.Hour)
	h.buildHistory(7, 0.12, 0.01, dReset, iReset, pReset, 0.0, 0.3, 0.2)
	_, d := h.pick("connect", "s1")
	if d.Account != "p@example.com" {
		t.Fatalf("connect borrowed %s: %s\n%v", d.Account, d.Reason, d.Ranking)
	}
}

func TestSessionsStayPinned(t *testing.T) {
	h := newHarness(t, "")
	start := h.now
	dReset, iReset, pReset := start.Add(80*time.Hour), start.Add(30*time.Hour), start.Add(60*time.Hour)
	h.buildHistory(7, 0.01, 0.01, dReset, iReset, pReset, 0.2, 0.3, 0.2)
	h.expectPick("connect", "s1", acctI) // i spare, resets before p
	// i stops being spare first: p now resets sooner. The open session stays.
	h.observe("connect", acctP, 0.3, h.now.Add(2*time.Hour), 0.2, h.now.Add(3*time.Hour), 10)
	d := h.expectPick("connect", "s1", acctI)
	if d.Session != "pinned" {
		t.Fatalf("session %s", d.Session)
	}
	// A new session follows the new order.
	h.expectPick("connect", "s2", acctP)
}

func TestExhaustionPolicies(t *testing.T) {
	h := newHarness(t, "")
	h.seed(0.2, 0.2, 1.0, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(1*time.Hour))
	// connect: fail.
	resp, d := h.pick("connect", "s1")
	if !resp.Reject || resp.RejectCode != RejectExhausted {
		t.Fatalf("connect: %+v %+v", resp, d)
	}
	// Runtime override: any account for the next 2h.
	if _, err := h.r.ApplyOverride([]byte(`{"client":"connect","policy":"any","for":"2h"}`)); err != nil {
		t.Fatal(err)
	}
	resp, d = h.pick("connect", "s2")
	if resp.AuthID == "" || d.Role != RoleLastResort {
		t.Fatalf("override: %+v %+v", resp, d)
	}
	h.advance(2*time.Hour + time.Minute)
	h.observe("pni", acctD, 0.2, h.now.Add(72*time.Hour), 0.1, h.now.Add(3*time.Hour), 0)
	h.observe("pni", acctI, 0.2, h.now.Add(96*time.Hour), 0.1, h.now.Add(3*time.Hour), 0)
	h.observe("connect", acctP, 1.0, h.now.Add(1*time.Hour), 0.1, h.now.Add(3*time.Hour), 0)
	resp, _ = h.pick("connect", "s3")
	if !resp.Reject {
		t.Fatalf("override should have expired: %+v", resp)
	}
}

func TestWaitPolicyHoldsThen429s(t *testing.T) {
	h := newHarness(t, "")
	h.seed(0.2, 0.2, 1.0, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(50*time.Minute))
	out := h.r.Intercept(pluginapi.RequestInterceptRequest{RequestedModel: "claude-opus-5-5", CallerScope: callerScope(keys["ssdev"])})
	if !out.Terminate || out.StatusCode != 429 {
		t.Fatalf("want 429, got %+v", out)
	}
	if h.slept != 30*time.Second {
		t.Fatalf("slept %s", h.slept)
	}
	if ra := out.ResponseHeaders.Get("Retry-After"); ra != "3000" {
		t.Fatalf("Retry-After %q", ra)
	}
	var body struct {
		Error struct{ Message string } `json:"error"`
	}
	if err := json.Unmarshal(out.ResponseBody, &body); err != nil || t3ResetSeconds(body.Error.Message) != 3000 {
		t.Fatalf("body %s: %v", out.ResponseBody, err)
	}
	// Other clients are untouched.
	h.slept = 0
	out = h.r.Intercept(pluginapi.RequestInterceptRequest{RequestedModel: "claude-opus-5-5", CallerScope: callerScope(keys["pni"])})
	if out.Terminate || h.slept != 0 {
		t.Fatalf("pni gated: %+v slept %s", out, h.slept)
	}
}

// t3ResetSeconds reads a retry time the way T3 Code 0.1.11 does from a
// Claude 429 that is not CLIProxyAPI's own cooldown text; -1 if it finds none.
func t3ResetSeconds(message string) int {
	m := regexp.MustCompile(`\(reset after (?:(\d+)h ?)?(?:(\d+)m ?)?(?:(\d+)s)?\)`).FindStringSubmatch(message)
	if m == nil {
		return -1
	}
	secs := 0
	for i, unit := range []int{3600, 60, 1} {
		n, _ := strconv.Atoi(m[i+1])
		secs += n * unit
	}
	return secs
}

func TestWaitRejectionCarriesResetForT3(t *testing.T) {
	wc := WaitConfig{MaxWait: 30 * time.Second}
	recheck := WaitConfig{MaxWait: 30 * time.Second, Recheck: 15 * time.Minute}
	for _, tc := range []struct {
		name       string
		g          gate
		wc         WaitConfig
		wantSecs   int
		wantT3     int
		wantPhrase string
	}{
		{"refill", gate{Refill: 4*time.Hour + 35*time.Minute + 1500*time.Millisecond, Reason: "r"}, wc, 16502, 16502, "quota refills in 4h35m"},
		{"refill past a day", gate{Refill: 26*time.Hour + 3*time.Minute, Reason: "r"}, wc, 93780, 93780, "quota refills in 26h03m"},
		{"recheck sooner", gate{Refill: 4 * time.Hour, Reason: "r"}, recheck, 900, 900, "checking again in 15m0s"},
		{"refill sooner", gate{Refill: 10 * time.Minute, Reason: "r"}, recheck, 600, 600, "quota refills in 10m0s"},
		{"unknown refill", gate{Reason: "r"}, wc, 30, -1, "fefo-router: r"},
		{"unknown refill, recheck", gate{Reason: "r"}, recheck, 900, 900, "fefo-router: r; checking again in 15m0s"},
	} {
		message, secs := waitRejection(tc.g, tc.wc)
		if secs != tc.wantSecs || t3ResetSeconds(message) != tc.wantT3 || !strings.Contains(message, tc.wantPhrase) {
			t.Errorf("%s: %q, Retry-After %d", tc.name, message, secs)
		}
		if strings.Contains(message, "checking again") && strings.Contains(tc.name, "refill sooner") {
			t.Errorf("%s: recheck later than the refill still shown: %q", tc.name, message)
		}
	}
}

func TestPacingNearRefill(t *testing.T) {
	h := newHarness(t, "")
	// connect has only p (d and i belong to pni and history is short, so no
	// spare). p's 5h window: 80% used, refills in 40m, burning 60%/h.
	h.seed(0.2, 0.2, 0.3, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(60*time.Hour))
	short := h.now.Add(70 * time.Minute)
	used := 0.5
	for i := 0; i < 30; i++ {
		h.advance(time.Minute)
		used += 0.01
		h.observe("connect", acctP, 0.3, h.now.Add(60*time.Hour), used, short, 100)
	}
	out := h.r.Intercept(pluginapi.RequestInterceptRequest{RequestedModel: "claude-opus-5-5", CallerScope: callerScope(keys["connect"])})
	if out.Terminate {
		t.Fatalf("unexpected terminate %+v", out)
	}
	if h.slept <= 0 || h.slept > 30*time.Second {
		t.Fatalf("pacing delay %s", h.slept)
	}
}

func TestUnknownKeyUsesDefaultClient(t *testing.T) {
	h := newHarness(t, "")
	h.seed(0.2, 0.2, 0.2, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(48*time.Hour))
	resp, d := h.pick("bot", "s1")
	if resp.AuthID == "" || !strings.HasPrefix(d.Client, "key:") || d.Role != RoleLastResort {
		t.Fatalf("bot: %+v %+v", resp, d)
	}
}

func TestConfigErrors(t *testing.T) {
	_, err := ParseConfig([]byte("buckets: {a: [x]}\nclients: {c: {buckets: [nope]}}\nspare_factor: 0.5\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown bucket") || !strings.Contains(err.Error(), "spare_factor") {
		t.Fatalf("err = %v", err)
	}
}

func TestStatePersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	h := newHarness(t, "state_file: "+path)
	h.seed(0.2, 0.2, 0.2, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(48*time.Hour))
	h.r.saveState()

	h2 := newHarness(t, "state_file: "+path)
	st := h2.r.Status(0)
	if len(st.Accounts) != 3 || st.Accounts[0].WeeklyRemaining > 0.81 {
		t.Fatalf("not restored: %+v", st.Accounts)
	}
	if txt := st.Text(); !strings.Contains(txt, "d@example.com") {
		t.Fatal(txt)
	}
}

func TestStatusAndMetricsRender(t *testing.T) {
	h := newHarness(t, "")
	h.seed(0.2, 0.2, 0.2, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(48*time.Hour))
	h.pick("pni", "s1")
	txt := h.r.Status(10).Text()
	for _, want := range []string{"ACCOUNTS", "CLIENTS", "pni", "[own] d@example.com", "RECENT DECISIONS"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("status missing %q:\n%s", want, txt)
		}
	}
	m := h.r.Prometheus()
	if !strings.Contains(m, `fefo_picks_total{client="pni",account="d@example.com",role="own",outcome="selected"} 1`) {
		t.Fatal(m)
	}
}

func TestRuntimeConfigOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	h := newHarness(t, "state_file: "+path)
	h.seed(0.2, 0.2, 0.2, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(48*time.Hour))
	h.expectPick("connect", "s1", acctP)

	// Move connect to bucket d from the browser.
	doc := h.r.Config().Doc()
	c := doc.Clients["connect"]
	c.Buckets = []string{"d"}
	doc.Clients["connect"] = c
	body, _ := json.Marshal(map[string]any{"config": doc, "note": "test"})
	v, err := h.r.UpdateConfig(body)
	if err != nil || v.Source != "runtime" {
		t.Fatalf("update: %v %+v", err, v)
	}
	h.expectPick("connect", "s2", acctD)

	// Invalid edits are refused and change nothing.
	if _, err := h.r.UpdateConfig([]byte(`{"yaml":"clients: {x: {buckets: [nope]}}"}`)); err == nil {
		t.Fatal("invalid config accepted")
	}
	// Survives a restart, and config.yaml edits do not clobber it.
	h.r.saveState()
	h2 := newHarness(t, "state_file: "+path)
	h2.seed(0.2, 0.2, 0.2, h2.now.Add(72*time.Hour), h2.now.Add(96*time.Hour), h2.now.Add(48*time.Hour))
	h2.expectPick("connect", "s3", acctD)
	// Revert.
	if _, err := h2.r.UpdateConfig([]byte(`{"revert":true}`)); err != nil {
		t.Fatal(err)
	}
	h2.expectPick("connect", "s4", acctP)
}

func TestAssignUnknownKey(t *testing.T) {
	h := newHarness(t, "")
	h.seed(0.2, 0.2, 0.2, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(48*time.Hour))
	h.pick("bot", "s1")
	st := h.r.Status(0)
	if len(st.UnknownKeys) != 1 || st.UnknownKeys[0].Scope != callerScope(keys["bot"]) {
		t.Fatalf("unknown keys: %+v", st.UnknownKeys)
	}
	body, _ := json.Marshal(map[string]any{"assign_key": map[string]string{"scope": st.UnknownKeys[0].Scope, "client": "bot"}})
	if _, err := h.r.UpdateConfig(body); err != nil {
		t.Fatal(err)
	}
	_, d := h.pick("bot", "s2")
	if d.Client != "bot" {
		t.Fatalf("client %s", d.Client)
	}
	if st := h.r.Status(0); len(st.UnknownKeys) != 0 {
		t.Fatalf("still unknown: %+v", st.UnknownKeys)
	}
}

func TestManagementRoutes(t *testing.T) {
	h := newHarness(t, "")
	hd := &Handler{Router: h.r}
	page := hd.management(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/fefo-router/ui"})
	if page.StatusCode != 200 || !strings.Contains(string(page.Body), "FEFO Router") {
		t.Fatalf("ui: %d", page.StatusCode)
	}
	cfg := hd.management(pluginapi.ManagementRequest{Method: "GET", Path: "/v0/management/fefo/config"})
	if cfg.StatusCode != 200 || !strings.Contains(string(cfg.Body), `"spare_factor":1.5`) {
		t.Fatalf("config: %d %s", cfg.StatusCode, cfg.Body)
	}
	bad := hd.management(pluginapi.ManagementRequest{Method: "POST", Path: "/v0/management/fefo/override", Body: []byte(`{"client":"nobody","policy":"any"}`)})
	if bad.StatusCode != 400 {
		t.Fatalf("override unknown client: %d", bad.StatusCode)
	}
}

func TestDurationRoundTrip(t *testing.T) {
	for d, want := range map[time.Duration]string{30 * time.Second: "30s", 6 * time.Hour: "6h", 90 * time.Minute: "90m", 0: "0s"} {
		if got := durStr(d); got != want {
			t.Fatalf("%s → %s, want %s", d, got, want)
		}
	}
	cfg := DefaultConfig()
	raw, _ := json.Marshal(cfg.Doc())
	back, err := ParseConfig(raw)
	if err != nil || back.MinHistory != cfg.MinHistory || back.Wait.MaxWait != cfg.Wait.MaxWait {
		t.Fatalf("round trip: %v %+v", err, back)
	}
}

func TestExampleConfigParses(t *testing.T) {
	raw, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Plugins struct {
			Configs map[string]yaml.Node `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	node := doc.Plugins.Configs[PluginName]
	section, _ := yaml.Marshal(&node)
	cfg, err := ParseConfig(section)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SpareFactor != 1.5 || len(cfg.Clients) != 2 || cfg.Clients["ci"].OnExhausted != PolicyWait {
		t.Fatalf("%+v", cfg)
	}
}

func TestIdleGapUsageIsExternal(t *testing.T) {
	h := newHarness(t, "")
	h.r.observeStart = h.now.Add(-22 * time.Hour) // 24h of observation by the end
	reset := h.now.Add(100 * time.Hour)
	short := h.now.Add(3 * time.Hour)
	h.observe("connect", acctP, 0.20, reset, 0.1, short, 100)
	h.advance(2 * time.Hour) // nothing through CPA; someone used the account elsewhere
	h.observe("connect", acctP, 0.40, reset, 0.1, short, 100)
	h.advance(2 * time.Minute)
	h.observe("connect", acctP, 0.41, reset, 0.1, short, 100)
	h.r.mu.Lock()
	pc := h.r.newPlanContextLocked("claude", h.now)
	ext, conn := pc.rate(externalClient, acctP), pc.rate("connect", "")
	h.r.mu.Unlock()
	unit := DefaultConfig().DefaultUnitUSD // no capacity learned: the prior
	if ext.Day < 0.19*unit/24 || conn.Day > 0.011*unit/24+1e-9 || conn.Day <= 0 {
		t.Fatalf("external %+v connect %+v", ext, conn)
	}
}

func TestLastResortPinYieldsToBetterAccount(t *testing.T) {
	h := newHarness(t, "")
	// pni's own buckets are both exhausted: it lands on p as last resort.
	h.seed(1.0, 1.0, 0.2, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(48*time.Hour))
	d := h.expectPick("pni", "s1", acctP)
	if d.Role != RoleLastResort {
		t.Fatalf("role %s", d.Role)
	}
	// d refills: the same session leaves the last-resort account.
	h.observe("pni", acctD, 0.0, h.now.Add(7*24*time.Hour), 0.0, h.now.Add(5*time.Hour), 10)
	d = h.expectPick("pni", "s1", acctD)
	if d.Session != "moved" {
		t.Fatalf("session %s", d.Session)
	}
}

func TestNeverDelegatesToBlockedAccounts(t *testing.T) {
	h := newHarness(t, "")
	h.seed(1.0, 1.0, 1.0, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(48*time.Hour))
	resp, d := h.pick("bot", "s1") // policy any, everything exhausted
	if !resp.Reject || resp.DelegateBuiltin != "" {
		t.Fatalf("%+v %+v", resp, d)
	}
}

func TestConfigSaveRefusesUnreachableWaitClient(t *testing.T) {
	h := newHarness(t, "")
	h.seed(0.2, 0.2, 0.2, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(48*time.Hour))
	doc := h.r.Config().Doc()
	doc.Buckets["p"] = []string{"typo@example.com"}
	body, _ := json.Marshal(map[string]any{"config": doc})
	if _, err := h.r.UpdateConfig(body); err == nil || !strings.Contains(err.Error(), "no account") {
		t.Fatalf("err = %v", err)
	}
	body, _ = json.Marshal(map[string]any{"config": doc, "force": true})
	if _, err := h.r.UpdateConfig(body); err != nil {
		t.Fatal(err)
	}
}

func TestEmailMatchesAuthFileName(t *testing.T) {
	a := accountInfo{ID: "claude-64d80597-me@example.eu.json"}
	c := accountInfo{ID: "codex-556dd76b-me@example.eu-plus.json"}
	other := accountInfo{ID: "claude-1-notme@example.eu.json"}
	if !a.matches("me@example.eu") || !c.matches("ME@example.eu") || other.matches("me@example.eu") || a.matches("e@example.eu") {
		t.Fatal("e-mail matching against auth file IDs is wrong")
	}
}

func TestHostListWithoutIDsIsNotReady(t *testing.T) {
	h := newHarness(t, "")
	r := New(WithClock(func() time.Time { return h.now }))
	r.SetConfig(testConfig(t, ""))
	r.mu.Lock()
	r.applyHostLocked([]pluginapi.HostAuthFileEntry{{Name: "claude-x-d@example.com.json"}}, h.now)
	r.mu.Unlock()
	st := r.Status(0)
	if len(st.Warnings) != 0 || st.HostError == "" {
		t.Fatalf("warnings %v host error %q", st.Warnings, st.HostError)
	}
}
