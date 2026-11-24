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
	// Family is the models.dev family (claude-fable, gpt-terra); empty means derive it from the id.
	Family string `json:"family,omitempty"`
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

// Quote is a cost interval in USD. Low == High for a known model. For an
// unknown one it spans the cheapest to the dearest model of Family.
type Quote struct {
	Low, High float64
	Match     Match
	Family    string
}

func (r Rates) usd(u Usage) float64 {
	return (float64(u.Uncached)*r.Input + float64(u.Output)*r.Output +
		float64(u.CacheRead)*r.CacheRead + float64(u.CacheWrite)*r.CacheWrite) / 1e6
}

// Cost prices u at the given model's rates. A model that does not resolve is
// quoted as an interval over its family (see pool), never at zero.
func (t Table) Cost(model string, u Usage) Quote {
	id := normID(model)
	p, m := t.Resolve(id)
	if m != Unknown {
		usd := p.ratesAt(u.InputTotal()).usd(u)
		return Quote{Low: usd, High: usd, Match: m, Family: p.familyOr(stripSuffixes(id))}
	}
	fam, members := t.pool(id)
	lo, hi := spread(members, u.InputTotal())
	return Quote{Low: lo.usd(u), High: hi.usd(u), Match: Unknown, Family: fam}
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

func normID(model string) string { return strings.ToLower(strings.TrimSpace(model)) }

// Resolve finds the price for a model id. For an unknown id it returns a zero
// Price and Unknown; use Cost to price such a model.
func (t Table) Resolve(model string) (Price, Match) {
	id := normID(model)
	if p, _, ok := lookup(t.Overrides, id); ok {
		return p, Override
	}
	if p, m, ok := lookup(t.Models, id); ok {
		return p, m
	}
	return Price{}, Unknown
}

// familyOf drops version tokens (digits and dots) from a model id:
// claude-fable-5-2 -> claude-fable, gpt-6.1-terra -> gpt-terra.
func familyOf(id string) string {
	var keep []string
	for _, tok := range strings.Split(id, "-") {
		if strings.Trim(tok, "0123456789.") != "" {
			keep = append(keep, tok)
		}
	}
	return strings.Join(keep, "-")
}

func (p Price) familyOr(id string) string {
	if p.Family != "" {
		return p.Family
	}
	return familyOf(id)
}

// provider groups model ids by vendor; "" means unrecognised.
func provider(id string) string {
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
var fallback = Rates{Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75}

// pool picks the known models an unknown id is priced against, narrowest
// first: its inferred model family, then its provider, then the whole table.
func (t Table) pool(id string) (string, []Price) {
	inferred := familyOf(stripSuffixes(id))
	vendor := provider(id)
	var byFamily, byVendor, all []Price
	for _, models := range []map[string]Price{t.Models, t.Overrides} {
		for k, p := range models {
			all = append(all, p)
			if inferred != "" && p.familyOr(k) == inferred {
				byFamily = append(byFamily, p)
			}
			if vendor != "" && provider(k) == vendor {
				byVendor = append(byVendor, p)
			}
		}
	}
	switch {
	case len(byFamily) > 0:
		return inferred, byFamily
	case len(byVendor) > 0:
		return vendor, byVendor
	}
	return "", all
}

// spread is the per-component min and max over pool at the given context size.
// A zero minimum (a model that omits that rate) is raised to the maximum, so
// no component of an unknown model's price is free.
func spread(pool []Price, input int64) (lo, hi Rates) {
	if len(pool) == 0 {
		return fallback, fallback
	}
	lo = pool[0].ratesAt(input)
	for _, p := range pool {
		r := p.ratesAt(input)
		lo.Input, hi.Input = min(lo.Input, r.Input), max(hi.Input, r.Input)
		lo.Output, hi.Output = min(lo.Output, r.Output), max(hi.Output, r.Output)
		lo.CacheRead, hi.CacheRead = min(lo.CacheRead, r.CacheRead), max(hi.CacheRead, r.CacheRead)
		lo.CacheWrite, hi.CacheWrite = min(lo.CacheWrite, r.CacheWrite), max(hi.CacheWrite, r.CacheWrite)
	}
	for _, c := range []struct{ lo, hi *float64 }{
		{&lo.Input, &hi.Input}, {&lo.Output, &hi.Output},
		{&lo.CacheRead, &hi.CacheRead}, {&lo.CacheWrite, &hi.CacheWrite},
	} {
		if *c.lo == 0 {
			*c.lo = *c.hi
		}
	}
	return lo, hi
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
