package pricing

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func r(in, out, cr, cw float64) Rates { return Rates{in, out, cr, cw} }

func testTable() Table {
	return Table{Models: map[string]Price{
		"claude-opus-5-5":   {Rates: r(4, 20, 0.2, 5)},
		"claude-fable-5-2":  {Rates: r(8, 40, 0.2, 10)},
		"claude-sonnet-5-1": {Rates: r(3, 15, 0.3, 3.75)},
		"gpt-6.1-terra": {Rates: r(1, 5, 0.05, 1.25), Tiers: []Tier{
			{AboveContext: 272000, Rates: r(2, 7.5, 0.1, 2.5)},
		}},
		"gpt-6": {Rates: r(1.5, 6, 0.15, 0)},
		"o9":    {Rates: r(2, 8, 0.5, 0)},
	}}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCost(t *testing.T) {
	tab := testTable()
	tests := []struct {
		name  string
		model string
		usage Usage
		want  float64
	}{
		{"claude with cache read and write", "claude-opus-5-5",
			Usage{Uncached: 1000, Output: 500, CacheRead: 100000, CacheWrite: 2000},
			(1000*4 + 500*20 + 100000*0.2 + 2000*5) / 1e6},
		{"below tier boundary", "gpt-6.1-terra",
			Usage{Uncached: 272000, Output: 1000},
			(272000*1 + 1000*5) / 1e6},
		{"above tier boundary uses tier rates", "gpt-6.1-terra",
			Usage{Uncached: 72001, CacheRead: 200000, Output: 1000},
			(72001*2 + 200000*0.1 + 1000*7.5) / 1e6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, m := tab.Cost(tc.model, tc.usage)
			if !near(got, tc.want) || m != Exact {
				t.Errorf("Cost = %v (%s), want %v (exact)", got, m, tc.want)
			}
		})
	}
}

func TestCostCodexCachedSubsetOfInput(t *testing.T) {
	u := NormalizeUsage("codex", pluginapi.UsageDetail{InputTokens: 10000, CachedTokens: 8000, OutputTokens: 100, ReasoningTokens: 40})
	got, _ := testTable().Cost("gpt-6.1-terra", u)
	want := (2000*1 + 8000*0.05 + 100*5) / 1e6
	if !near(got, want) {
		t.Errorf("Cost = %v, want %v", got, want)
	}
}

func TestNormalizeUsage(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		d        pluginapi.UsageDetail
		want     Usage
	}{
		{"claude cache is separate from input", "claude",
			pluginapi.UsageDetail{InputTokens: 50, OutputTokens: 10, CacheReadTokens: 900, CacheCreationTokens: 30},
			Usage{Uncached: 50, Output: 10, CacheRead: 900, CacheWrite: 30}},
		{"claude cached total that includes writes", "claude",
			pluginapi.UsageDetail{InputTokens: 50, CachedTokens: 930, CacheCreationTokens: 30},
			Usage{Uncached: 50, CacheRead: 900, CacheWrite: 30}},
		{"claude reasoning bills as output", "anthropic",
			pluginapi.UsageDetail{InputTokens: 5, OutputTokens: 10, ReasoningTokens: 7},
			Usage{Uncached: 5, Output: 17}},
		{"codex input includes cached", "codex",
			pluginapi.UsageDetail{InputTokens: 1000, CachedTokens: 600, OutputTokens: 20},
			Usage{Uncached: 400, CacheRead: 600, Output: 20}},
		{"openai reasoning already inside output", "openai",
			pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 30, ReasoningTokens: 20},
			Usage{Uncached: 10, Output: 30}},
		{"gemini reasoning is separate from output", "gemini",
			pluginapi.UsageDetail{InputTokens: 100, CacheReadTokens: 40, OutputTokens: 30, ReasoningTokens: 20},
			Usage{Uncached: 60, CacheRead: 40, Output: 50}},
		{"inconsistent cache larger than input never goes negative", "codex",
			pluginapi.UsageDetail{InputTokens: 10, CachedTokens: 50},
			Usage{CacheRead: 50}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeUsage(tc.provider, tc.d); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	tab := testTable()
	tab.Models["gpt-6.1"] = Price{Rates: r(9, 9, 9, 9)}
	tests := []struct {
		id        string
		wantInput float64
		wantMatch Match
	}{
		{"claude-opus-5-5", 4, Exact},
		{"claude-opus-5-5-20260101", 4, Dated},
		{"claude-opus-5-5-thinking", 4, Suffix},
		{"claude-opus-5-5(high)", 4, Suffix},
		{"claude-opus-5-5[1m]", 4, Suffix},
		{"claude-opus-5-5-20260101-thinking", 4, Suffix},
		{"claude-opus-5-5-future", 4, Prefix},
		{"gpt-6.1-terra-mini", 1, Prefix}, // longest prefix beats gpt-6.1 and gpt-6
		{"  Claude-Opus-5-5 ", 4, Exact},
		{"totally-unknown", 0, Unknown},
	}
	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			p, m := tab.Resolve(tc.id)
			if m != tc.wantMatch {
				t.Fatalf("match = %s, want %s", m, tc.wantMatch)
			}
			if m != Unknown && p.Input != tc.wantInput {
				t.Errorf("input = %v, want %v", p.Input, tc.wantInput)
			}
		})
	}
}

func TestUnknownModelIsProtective(t *testing.T) {
	tab := testTable()
	big := Usage{Uncached: 500000, Output: 1000, CacheRead: 1000, CacheWrite: 1000}
	small := Usage{Uncached: 1000, Output: 1000, CacheRead: 1000, CacheWrite: 1000}
	tests := []struct {
		unknown string
		fam     string
	}{
		{"claude-brand-new-9", "claude-"},
		{"gpt-99", "gpt-"},
		{"o11-pro", "gpt-"},
		{"codex-whatever", "gpt-"},
		{"mystery-model", ""},
	}
	for _, tc := range tests {
		t.Run(tc.unknown, func(t *testing.T) {
			for _, u := range []Usage{small, big} {
				got, m := tab.Cost(tc.unknown, u)
				if m != Unknown || got <= 0 {
					t.Fatalf("Cost = %v (%s), want positive unknown", got, m)
				}
				for id := range tab.Models {
					if tc.fam != "" && family(id) != family(tc.unknown) {
						continue
					}
					if known, _ := tab.Cost(id, u); got < known-1e-12 {
						t.Errorf("unknown %.6f < known %s %.6f", got, id, known)
					}
				}
			}
		})
	}
	if got, m := (Table{}).Cost("anything", small); got <= 0 || m != Unknown {
		t.Errorf("empty table Cost = %v (%s), want positive unknown", got, m)
	}
}

const fixture = `{
 "anthropic": {"models": {
   "claude-opus-5-5": {"name": "x", "cost": {"input": 4, "output": 20, "cache_read": 0.2, "cache_write": 5}},
   "claude-free": {"name": "no cost"}
 }},
 "openai": {"models": {
   "gpt-6.1-terra": {"cost": {"input": 1, "output": 5, "cache_read": 0.05, "cache_write": 1.25,
     "tiers": [{"input": 2, "output": 7.5, "cache_read": 0.1, "cache_write": 2.5, "tier": {"type": "context", "size": 272000}},
               {"input": 99, "tier": {"type": "other", "size": 1}}]}}
 }},
 "other": {"models": {"llama": {"cost": {"input": 1, "output": 1}}}}
}`

func TestParseModelsDev(t *testing.T) {
	tab, err := ParseModelsDev([]byte(fixture), "anthropic", "openai")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tab.Models["claude-free"]; ok {
		t.Error("model without cost should be skipped")
	}
	if _, ok := tab.Models["llama"]; ok {
		t.Error("unlisted provider should be skipped")
	}
	if got := tab.Models["claude-opus-5-5"]; got.Rates != r(4, 20, 0.2, 5) {
		t.Errorf("claude-opus-5-5 = %+v", got)
	}
	terra := tab.Models["gpt-6.1-terra"]
	if len(terra.Tiers) != 1 || terra.Tiers[0].AboveContext != 272000 || terra.Tiers[0].Rates != r(2, 7.5, 0.1, 2.5) {
		t.Errorf("terra tiers = %+v", terra.Tiers)
	}
	all, err := ParseModelsDev([]byte(fixture))
	if err != nil || all.Models["llama"].Input != 1 {
		t.Errorf("no provider filter should read all: %v %+v", err, all.Models["llama"])
	}
	if _, err := ParseModelsDev([]byte("not json")); err == nil {
		t.Error("want error for invalid json")
	}
}

func TestMergePrecedence(t *testing.T) {
	embedded := Table{Source: "embedded", Models: map[string]Price{
		"a": {Rates: r(1, 1, 1, 1)}, "b": {Rates: r(1, 1, 1, 1)}, "c": {Rates: r(1, 1, 1, 1)},
	}}
	synced := Table{Source: "models.dev", SyncedAt: time.Unix(100, 0), Models: map[string]Price{
		"b": {Rates: r(2, 2, 2, 2)}, "c": {Rates: r(2, 2, 2, 2)},
	}}
	over := map[string]Price{"c": {Rates: r(3, 3, 3, 3)}}
	merged := Merge(Table{}.WithOverrides(over), synced, embedded)
	tests := []struct {
		id        string
		wantInput float64
		wantMatch Match
	}{
		{"a", 1, Exact},
		{"b", 2, Exact},
		{"c", 3, Override},
	}
	for _, tc := range tests {
		p, m := merged.Resolve(tc.id)
		if p.Input != tc.wantInput || m != tc.wantMatch {
			t.Errorf("%s: input %v match %s, want %v %s", tc.id, p.Input, m, tc.wantInput, tc.wantMatch)
		}
	}
	if merged.Source != "models.dev" || !merged.SyncedAt.Equal(time.Unix(100, 0)) {
		t.Errorf("metadata = %q %v", merged.Source, merged.SyncedAt)
	}
}

func TestTableJSONRoundTrip(t *testing.T) {
	in := Merge(testTable().WithOverrides(map[string]Price{"x": {Rates: r(1, 2, 3, 4)}}))
	in.Source, in.SyncedAt = "models.dev", time.Unix(1700000000, 0).UTC()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Table
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	u := Usage{Uncached: 300000, Output: 10, CacheRead: 5}
	a, _ := in.Cost("gpt-6.1-terra", u)
	b, _ := out.Cost("gpt-6.1-terra", u)
	if a != b || out.Source != in.Source || !out.SyncedAt.Equal(in.SyncedAt) || !reflect.DeepEqual(out.Overrides, in.Overrides) {
		t.Errorf("round trip mismatch: %+v vs %+v", in, out)
	}
}

func TestEmbedded(t *testing.T) {
	tab := Embedded()
	p, m := tab.Resolve("claude-opus-5-5")
	if m != Exact || p.Rates != r(4, 20, 0.2, 5) {
		t.Errorf("claude-opus-5-5 = %+v (%s)", p, m)
	}
	if tab.Source != "embedded" {
		t.Errorf("source = %q", tab.Source)
	}
}
