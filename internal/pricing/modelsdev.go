package pricing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
)

//go:embed models.json
var embeddedJSON []byte

type mdCost struct {
	Rates
	Tiers []struct {
		Rates
		Tier struct {
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"tier"`
	} `json:"tiers"`
}

type mdProvider struct {
	Models map[string]struct {
		Cost *mdCost `json:"cost"`
	} `json:"models"`
}

// ParseModelsDev reads https://models.dev/api.json. With no providers given it
// reads all of them; otherwise only the listed ones, later ones winning id
// collisions. Models without a cost are skipped.
func ParseModelsDev(raw []byte, providers ...string) (Table, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Table{}, fmt.Errorf("pricing: parse models.dev: %w", err)
	}
	if len(providers) == 0 {
		for k := range doc {
			providers = append(providers, k)
		}
		sort.Strings(providers)
	}
	t := Table{Models: map[string]Price{}, Source: "models.dev"}
	for _, name := range providers {
		rawProv, ok := doc[name]
		if !ok {
			continue
		}
		var prov mdProvider
		if err := json.Unmarshal(rawProv, &prov); err != nil {
			return Table{}, fmt.Errorf("pricing: parse models.dev provider %q: %w", name, err)
		}
		for id, m := range prov.Models {
			if m.Cost == nil {
				continue
			}
			p := Price{Rates: m.Cost.Rates}
			for _, tr := range m.Cost.Tiers {
				if tr.Tier.Type != "context" {
					continue
				}
				p.Tiers = append(p.Tiers, Tier{AboveContext: tr.Tier.Size, Rates: inherit(tr.Rates, p.Rates)})
			}
			t.Models[id] = p
		}
	}
	return t, nil
}

// inherit fills components a tier omits (models.dev often leaves out cache_write)
// from the base rates, since a missing component must not become free.
func inherit(tier, base Rates) Rates {
	if tier.Input == 0 {
		tier.Input = base.Input
	}
	if tier.Output == 0 {
		tier.Output = base.Output
	}
	if tier.CacheRead == 0 {
		tier.CacheRead = base.CacheRead
	}
	if tier.CacheWrite == 0 {
		tier.CacheWrite = base.CacheWrite
	}
	return tier
}

// Embedded is the snapshot compiled into the binary (see scripts/update-prices.sh).
func Embedded() Table {
	t, err := ParseModelsDev(embeddedJSON, "anthropic", "openai")
	if err != nil {
		panic(err)
	}
	t.Source = "embedded"
	return t
}
