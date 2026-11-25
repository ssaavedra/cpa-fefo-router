package router

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
	"time"

	"github.com/ssaavedra/cpa-fefo-router/internal/quota"
)

const (
	historyRetention = 8 * 24 * time.Hour
	recentRetention  = 3 * time.Hour
	externalClient   = "(external)"
	anonymousClient  = "(anonymous)"
	resetTolerance   = 10 * time.Minute
	// attributionGap: usage between two readings further apart than this
	// happened while CPA sent the account nothing, so it is not CPA's.
	attributionGap = 10 * time.Minute
)

// accountInfo is what the host tells us about a credential.
type accountInfo struct {
	ID       string `json:"id"`
	Label    string `json:"label,omitempty"`
	Name     string `json:"name,omitempty"`
	Provider string `json:"provider,omitempty"`
	AuthKind string `json:"auth_kind,omitempty"`
	// AuthIndex is the host's runtime credential index, for host.auth.get.
	AuthIndex string `json:"-"`
}

// matches reports whether a config identifier names this account.
func (a accountInfo) matches(ident string) bool {
	ident = strings.ToLower(strings.TrimSpace(ident))
	if ident == "" {
		return false
	}
	for _, v := range []string{a.ID, a.Label, a.Name, strings.TrimSuffix(a.Name, ".json")} {
		if v != "" && strings.ToLower(v) == ident {
			return true
		}
	}
	// CPA names OAuth files "<provider>-<hash>-<email>[-plan].json" and may
	// give no label, so an e-mail also matches as a whole segment of the ID.
	if strings.Contains(ident, "@") {
		for _, v := range []string{a.ID, a.Name} {
			v = strings.ToLower(v)
			for i := strings.Index(v, ident); i >= 0; {
				end := i + len(ident)
				before := i == 0 || v[i-1] == '-' || v[i-1] == '/' || v[i-1] == '_'
				after := end == len(v) || v[end] == '.' || v[end] == '-' || v[end] == '_'
				if before && after {
					return true
				}
				next := strings.Index(v[i+1:], ident)
				if next < 0 {
					break
				}
				i += 1 + next
			}
		}
	}
	return false
}

// display is the human name of an account: its e-mail when known.
func (a accountInfo) display() string {
	switch {
	case a.Label != "":
		return a.Label
	case a.Name != "":
		return strings.TrimSuffix(a.Name, ".json")
	default:
		return a.ID
	}
}

// usedSample is one reading of an account's window usage, for short-term rates.
type usedSample struct {
	At     time.Time `json:"at"`
	Window string    `json:"window"`
	Used   float64   `json:"used"`
	Reset  time.Time `json:"reset"`
}

// accountState is everything known about one credential.
type accountState struct {
	Info           accountInfo    `json:"info"`
	Snapshot       quota.Snapshot `json:"snapshot"`
	HasSnapshot    bool           `json:"has_snapshot"`
	Disabled       bool           `json:"-"`
	NextRetryAfter time.Time      `json:"-"`
	ProbedAt       time.Time      `json:"-"`
	// Last weekly reading, the baseline for usage deltas.
	LastWeekly *usedSample `json:"last_weekly,omitempty"`
	// Recent readings of every window, for pacing.
	Recent []usedSample `json:"recent,omitempty"`
	// Learn measures the account's weekly capacity in dollars.
	Learn capacityLearn `json:"capacity_learn"`
	// Usage is the Claude usage report (scoped windows, extra usage).
	Usage usagePoll `json:"usage_poll"`
}

// hourBucket aggregates one account's usage within one clock hour.
type hourBucket struct {
	// Delta is the weekly-window share consumed in this hour by CPA traffic
	// (0..1 of a week), split between clients by their API-equivalent cost.
	Delta float64 `json:"delta"`
	// External is weekly-window share that accrued while CPA sent the account
	// nothing (between readings more than attributionGap apart).
	External float64 `json:"external,omitempty"`
	// Tokens and Requests per client: display, and the split fallback for
	// hours recorded before requests were priced.
	Tokens   map[string]int64 `json:"tokens,omitempty"`
	Requests map[string]int   `json:"requests,omitempty"`
	// USDLow and USDHigh are each client's requests at API prices. They differ
	// only for models missing from the price table.
	USDLow  map[string]float64 `json:"usd_low,omitempty"`
	USDHigh map[string]float64 `json:"usd_high,omitempty"`
	// Cycle is the weekly reset the hour's readings belong to.
	Cycle time.Time `json:"cycle,omitzero"`
	// Tainted hours are left out of capacity learning: a request was priced
	// as an unknown model, or the weekly window reset within the hour.
	Tainted bool `json:"tainted,omitempty"`
	// Priced is set when the hour was recorded by a router that prices every
	// request. An hour carried over from before pricing has dollars for only
	// part of its traffic, so its split and capacity use fall back to tokens.
	Priced bool `json:"priced,omitempty"`
	// GapUSD is the low-end cost of requests whose reading closed an idle gap.
	// Their share of the step is in External but is billed to the client, so
	// it is taken back out of External's dollars.
	GapUSD float64 `json:"gap_usd,omitempty"`
}

// billed is the dollars a client used in this hour. In a priced hour that is
// what its requests cost, at the high end for a home client of the account
// and the low end for a borrower (as in split). Otherwise it is the client's
// share of the weekly delta times capacityUSD. The external pseudo-client
// gets its share minus GapUSD.
func (b *hourBucket) billed(client string, home map[string]bool, capacityUSD float64) float64 {
	if client == externalClient {
		return math.Max(0, b.split(home)[client]*capacityUSD-b.GapUSD)
	}
	if b.Priced && b.USDHigh[client] > 0 {
		if home[client] {
			return b.USDHigh[client]
		}
		return b.USDLow[client]
	}
	return b.split(home)[client] * capacityUSD
}

// split returns each client's share of Delta, weighted by cost. A home client
// of the account is weighted by the high end of its cost and a borrower by
// the low end, so an unpriced model never shifts blame off the home clients
// (which would make the account lend more). Hours without prices fall back to
// tokens, then requests; Delta with no requests at all happened outside CPA.
func (b *hourBucket) split(home map[string]bool) map[string]float64 {
	out := map[string]float64{}
	if b.External > 0 {
		out[externalClient] += b.External
	}
	if b.Delta <= 0 {
		return out
	}
	weights := map[string]float64{}
	for c, lo := range b.USDLow {
		w := lo
		if home[c] {
			w = b.USDHigh[c]
		}
		if w > 0 && b.Priced {
			weights[c] = w
		}
	}
	if len(weights) == 0 {
		for c, t := range b.Tokens {
			if t > 0 {
				weights[c] = float64(t)
			}
		}
	}
	if len(weights) == 0 {
		for c, n := range b.Requests {
			if n > 0 {
				weights[c] = float64(n)
			}
		}
	}
	total := 0.0
	for _, w := range weights {
		total += w
	}
	if total <= 0 {
		out[externalClient] += b.Delta
		return out
	}
	for c, w := range weights {
		out[c] += b.Delta * w / total
	}
	return out
}

func addTo[V int | int64 | float64](m *map[string]V, k string, v V) {
	if *m == nil {
		*m = map[string]V{}
	}
	(*m)[k] += v
}

func hourOf(t time.Time) int64 { return t.Unix() / 3600 }

// callerScope mirrors CPA's session.CallerScope.
func callerScope(rawKey string) string {
	rawKey = strings.TrimSpace(rawKey)
	if rawKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + rawKey))
	return hex.EncodeToString(sum[:])
}

func plainSHA(rawKey string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(rawKey)))
	return hex.EncodeToString(sum[:])
}

// weeklyWindow returns the snapshot's longest window (Claude "7d", Codex
// "secondary"), which is the one whose unused quota expires.
func weeklyWindow(s quota.Snapshot) (quota.Window, bool) {
	best, ok := quota.Window{}, false
	for _, w := range s.Windows {
		long := w.Name == "7d" || w.Length >= 6*24*time.Hour
		if !long {
			continue
		}
		if !ok || w.Length > best.Length {
			best, ok = w, true
		}
	}
	return best, ok
}

func sameReset(a, b time.Time) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d <= resetTolerance
}
