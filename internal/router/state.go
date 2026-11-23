package router

import (
	"crypto/sha256"
	"encoding/hex"
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
}

// hourBucket aggregates one account's usage within one clock hour.
type hourBucket struct {
	// Delta is the weekly-window share consumed in this hour by CPA traffic
	// (0..1 of a plan-week), split between clients by tokens.
	Delta float64 `json:"delta"`
	// External is weekly-window share that accrued while CPA sent the account
	// nothing (between readings more than attributionGap apart).
	External float64 `json:"external,omitempty"`
	// Tokens and Requests per client, used to split Delta between clients.
	Tokens   map[string]int64 `json:"tokens,omitempty"`
	Requests map[string]int   `json:"requests,omitempty"`
}

// split returns each client's share of Delta. Delta seen in an hour with no
// token-bearing requests is attributed to usage outside CPA.
func (b *hourBucket) split() map[string]float64 {
	out := map[string]float64{}
	if b.External > 0 {
		out[externalClient] += b.External
	}
	if b.Delta <= 0 {
		return out
	}
	var total int64
	for _, t := range b.Tokens {
		total += t
	}
	if total <= 0 {
		var reqs int
		for _, n := range b.Requests {
			reqs += n
		}
		if reqs == 0 {
			out[externalClient] = b.Delta
			return out
		}
		for c, n := range b.Requests {
			out[c] += b.Delta * float64(n) / float64(reqs)
		}
		return out
	}
	for c, t := range b.Tokens {
		out[c] += b.Delta * float64(t) / float64(total)
	}
	return out
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
