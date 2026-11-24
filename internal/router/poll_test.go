package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "quota", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func token(account string) string { return "sk-ant-oat01-SECRET-" + account }

// pollingHarness serves a usage report per account through a fake host and
// captures every log line.
func pollingHarness(t *testing.T, extra string, reports map[string]string) (*harness, *fakeHost, *[]string) {
	h := newHarness(t, extra)
	host := newFakeHost()
	for _, a := range []string{"d", "i", "p"} {
		host.creds["idx-"+a] = fmt.Sprintf(`{"type":"claude","access_token":%q,"refresh_token":"sk-ant-ort01-REFRESH"}`, token(a))
	}
	host.routes[claudeUsageURL] = func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		for a, body := range reports {
			if req.Headers.Get("Authorization") == "Bearer "+token(a) {
				return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}, nil
			}
		}
		return pluginapi.HTTPResponse{StatusCode: 404}, nil
	}
	var logs []string
	h.r.log = func(level, msg string, fields map[string]any) {
		raw, _ := json.Marshal(fields)
		logs = append(logs, level+" "+msg+" "+string(raw))
	}
	h.r.fetcher = host
	h.r.jitter = func() float64 { return 0.5 }
	return h, host, &logs
}

func TestUsagePollerReadsReportsAndNeverLeaksTheToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	h, host, logs := pollingHarness(t, "state_file: "+path+"\npricing: {sync: false}",
		map[string]string{"d": fixture(t, "claude_usage_ultra_extra.json")})
	usage := host.routes[claudeUsageURL]
	host.routes[claudeUsageURL] = func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch req.Headers.Get("Authorization") {
		case "Bearer " + token("i"):
			return pluginapi.HTTPResponse{StatusCode: 401, Body: []byte(`{"error":"invalid token ` + token("i") + `"}`)}, nil
		case "Bearer " + token("p"):
			return pluginapi.HTTPResponse{}, errors.New("host.http.do: proxy refused GET with Authorization: Bearer " + token("p"))
		}
		return usage(req)
	}
	h.refresh()

	reqs := host.sent(claudeUsageURL)
	if len(reqs) != 3 {
		t.Fatalf("polled %d accounts, want 3", len(reqs))
	}
	if r := reqs[0]; r.Method != "GET" || r.Headers.Get("Anthropic-Beta") != "oauth-2025-04-20" || r.Headers.Get("Authorization") != "Bearer "+token("d") {
		t.Fatalf("request %s %v", r.Method, r.Headers)
	}
	d := h.account("d@example.com")
	if d.NoData || !closeTo(d.WeeklyRemaining, 0.93, 1e-9) {
		t.Fatalf("report not merged into the snapshot: %+v", d)
	}
	u := d.Usage
	if u == nil || u.ExtraUsageEnabled == nil || !*u.ExtraUsageEnabled || len(u.Scoped) != 1 || u.Scoped[0].Family != "fable" || u.Scoped[0].Allowance != 0.5 {
		t.Fatalf("usage view %+v", u)
	}
	if i := h.account("i@example.com"); i.Usage == nil || !strings.Contains(i.Usage.Error, "401") {
		t.Fatalf("401 not recorded: %+v", i.Usage)
	}
	if p := h.account("p@example.com"); p.Usage == nil || !strings.Contains(p.Usage.Error, "[redacted]") {
		t.Fatalf("transport error not recorded: %+v", p.Usage)
	}

	// Not polled again before usage_poll_interval; polled again after it.
	h.advance(time.Minute)
	h.refresh()
	if n := len(host.sent(claudeUsageURL)); n != 3 {
		t.Fatalf("polled again after 1m: %d requests", n)
	}
	h.advance(5 * time.Minute)
	h.refresh()
	if n := len(host.sent(claudeUsageURL)); n != 6 {
		t.Fatalf("not polled after the interval: %d requests", n)
	}

	h.r.saveState()
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := json.Marshal(h.r.Status(50))
	surfaces := map[string]string{
		"state file":  string(saved),
		"status JSON": string(status),
		"status text": h.r.Status(50).Text(),
		"metrics":     h.r.Prometheus(),
		"logs":        strings.Join(*logs, "\n"),
	}
	if !strings.Contains(surfaces["logs"], "usage poll failed") {
		t.Fatalf("poll failures were not logged:\n%s", surfaces["logs"])
	}
	for name, text := range surfaces {
		for _, a := range []string{"d", "i", "p"} {
			if strings.Contains(text, token(a)) || strings.Contains(text, "sk-ant-ort01") {
				t.Fatalf("%s leaks a credential:\n%s", name, text)
			}
		}
	}
}

func TestUsagePollsAreJitteredWithinThirtyPercent(t *testing.T) {
	for _, tc := range []struct {
		draw          float64
		early, onTime time.Duration
	}{
		{0, 3*time.Minute + 29*time.Second, 3*time.Minute + 30*time.Second},
		{0.5, 4*time.Minute + 59*time.Second, 5 * time.Minute},
		{0.9999999, 6*time.Minute + 29*time.Second, 6*time.Minute + 30*time.Second},
	} {
		t.Run(fmt.Sprint(tc.draw), func(t *testing.T) {
			h, host, _ := pollingHarness(t, "pricing: {sync: false}\nusage_poll_interval: 5m", map[string]string{})
			h.r.jitter = func() float64 { return tc.draw }
			h.refresh()
			first := len(host.sent(claudeUsageURL))
			h.advance(tc.early)
			h.refresh()
			if n := len(host.sent(claudeUsageURL)); n != first {
				t.Fatalf("polled again after %s with draw %v", tc.early, tc.draw)
			}
			h.advance(tc.onTime - tc.early)
			h.refresh()
			if n := len(host.sent(claudeUsageURL)); n != 2*first {
				t.Fatalf("not polled again after %s with draw %v (%d requests, want %d)", tc.onTime, tc.draw, n, 2*first)
			}
		})
	}
}
