// Package pricing turns token usage into API-equivalent US dollars.
package pricing

import (
	"regexp"
	"strings"
	"time"
)

// Match records which Resolve rule found a price.
type Match string

const (
	Exact    Match = "exact"
	Dated    Match = "dated"
	Suffix   Match = "suffix"
	Prefix   Match = "prefix"
	Override Match = "override"
	Unknown  Match = "unknown"
)

// Rates are USD per 1M tokens.
type Rates struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// Tier replaces the base rates for requests whose total input exceeds AboveContext tokens.
type Tier struct {
	AboveContext int64 `json:"above_context"`
	Rates
}

// Price is a model's base rates plus its context tiers.
type Price struct {
	Rates
	Tiers []Tier `json:"tiers,omitempty"`
}

// Usage is a request's token counts as non-overlapping billing buckets.
// Reasoning tokens are already folded into Output.
type Usage struct {
	Uncached   int64 `json:"uncached"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// InputTotal is the context size the request consumed.
func (u Usage) InputTotal() int64 { return u.Uncached + u.CacheRead + u.CacheWrite }

// Table maps model ids to prices. Overrides are user-configured and win over Models.
type Table struct {
	Models    map[string]Price `json:"models"`
	Overrides map[string]Price `json:"overrides,omitempty"`
	Source    string           `json:"source,omitempty"`
	SyncedAt  time.Time        `json:"synced_at,omitzero"`
}

// ratesAt returns the rates that apply to a request with the given total input.
func (p Price) ratesAt(input int64) Rates {
	best := p.Rates
	var bestAbove int64 = -1
	for _, t := range p.Tiers {
		if input > t.AboveContext && t.AboveContext > bestAbove {
			best, bestAbove = t.Rates, t.AboveContext
		}
	}
	return best
}

// Cost prices u at the given model's rates. A model that does not resolve is
// priced protectively (see protective), never at zero.
func (t Table) Cost(model string, u Usage) (float64, Match) {
	p, m := t.Resolve(model)
	r := p.ratesAt(u.InputTotal())
	usd := (float64(u.Uncached)*r.Input + float64(u.Output)*r.Output +
		float64(u.CacheRead)*r.CacheRead + float64(u.CacheWrite)*r.CacheWrite) / 1e6
	return usd, m
}

var (
	dateSuffix    = regexp.MustCompile(`-(\d{8}|\d{4}-\d{2}-\d{2})$`)
	noiseSuffixes = []*regexp.Regexp{
		regexp.MustCompile(`\s*\([^()]*\)$`),
		regexp.MustCompile(`\s*\[[^\[\]]*\]$`),
		regexp.MustCompile(`-thinking$`),
	}
)

// stripSuffixes removes noise suffixes repeatedly, so "x-thinking(high)" reduces to "x".
func stripSuffixes(id string) string {
	for changed := true; changed; {
		changed = false
		for _, re := range noiseSuffixes {
			if s := re.ReplaceAllString(id, ""); s != id {
				id, changed = s, true
			}
		}
	}
	return id
}

// lookup applies the Resolve rules against one map.
func lookup(models map[string]Price, id string) (Price, Match, bool) {
	if p, ok := models[id]; ok {
		return p, Exact, true
	}
	if s := dateSuffix.ReplaceAllString(id, ""); s != id {
		if p, ok := models[s]; ok {
			return p, Dated, true
		}
	}
	if s := stripSuffixes(id); s != id {
		for _, c := range []string{s, dateSuffix.ReplaceAllString(s, "")} {
			if p, ok := models[c]; ok {
				return p, Suffix, true
			}
		}
		id = s
	}
	longest := ""
	for k := range models {
		if len(k) > len(longest) && strings.HasPrefix(id, k) {
			longest = k
		}
	}
	if longest != "" {
		return models[longest], Prefix, true
	}
	return Price{}, Unknown, false
}

// Resolve finds the price for a model id, or the protective price with Unknown.
func (t Table) Resolve(model string) (Price, Match) {
	id := strings.ToLower(strings.TrimSpace(model))
	if p, _, ok := lookup(t.Overrides, id); ok {
		return p, Override
	}
	if p, m, ok := lookup(t.Models, id); ok {
		return p, m
	}
	return t.protective(id), Unknown
}

// family groups model ids by provider; "" means unrecognised.
func family(id string) string {
	switch {
	case strings.HasPrefix(id, "claude-"):
		return "claude"
	case strings.HasPrefix(id, "gpt-"), strings.HasPrefix(id, "codex-"),
		len(id) >= 2 && id[0] == 'o' && id[1] >= '0' && id[1] <= '9':
		return "gpt"
	}
	return ""
}

// fallback prices an unknown model against an empty table, so it is never free.
var fallback = Price{Rates: Rates{Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75}}

// protective is the per-component maximum, at every context tier boundary,
// over the known models of id's family (the whole table when the family is
// empty or has no entries). Over-pricing keeps an account's own clients from
// being starved by lending against underestimated demand.
func (t Table) protective(id string) Price {
	fam := family(id)
	var pool []Price
	collect := func(famOnly bool) {
		for _, models := range []map[string]Price{t.Models, t.Overrides} {
			for k, p := range models {
				if !famOnly || family(k) == fam {
					pool = append(pool, p)
				}
			}
		}
	}
	collect(fam != "")
	if len(pool) == 0 && fam != "" {
		collect(false)
	}
	if len(pool) == 0 {
		return fallback
	}
	bounds := map[int64]bool{}
	for _, p := range pool {
		for _, tr := range p.Tiers {
			bounds[tr.AboveContext] = true
		}
	}
	out := Price{Rates: maxRates(pool, 0)}
	for b := range bounds {
		out.Tiers = append(out.Tiers, Tier{AboveContext: b, Rates: maxRates(pool, b+1)})
	}
	return out
}

func maxRates(pool []Price, input int64) Rates {
	var m Rates
	for _, p := range pool {
		r := p.ratesAt(input)
		m.Input = max(m.Input, r.Input)
		m.Output = max(m.Output, r.Output)
		m.CacheRead = max(m.CacheRead, r.CacheRead)
		m.CacheWrite = max(m.CacheWrite, r.CacheWrite)
	}
	return m
}

// WithOverrides returns a copy of t whose Overrides are the given prices.
func (t Table) WithOverrides(o map[string]Price) Table {
	t.Overrides = o
	return t
}

// Merge layers tables, highest priority first: for each model id, Models and
// Overrides come from the first layer that has it. Source and SyncedAt come
// from the first layer that sets them.
func Merge(layers ...Table) Table {
	out := Table{Models: map[string]Price{}}
	for _, l := range layers {
		for k, p := range l.Models {
			if _, ok := out.Models[k]; !ok {
				out.Models[k] = p
			}
		}
		for k, p := range l.Overrides {
			if out.Overrides == nil {
				out.Overrides = map[string]Price{}
			}
			if _, ok := out.Overrides[k]; !ok {
				out.Overrides[k] = p
			}
		}
		if out.Source == "" {
			out.Source = l.Source
		}
		if out.SyncedAt.IsZero() {
			out.SyncedAt = l.SyncedAt
		}
	}
	return out
}
