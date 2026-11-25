package router

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ssaavedra/cpa-fefo-router/internal/pricing"
	"gopkg.in/yaml.v3"
)

// Mode selects whether the plugin routes traffic or only observes it.
type Mode string

const (
	// ModeActive registers the scheduler and request interceptor.
	ModeActive Mode = "active"
	// ModeObserve only records usage and serves diagnostics. Another scheduler
	// (or CPA's built-in one) keeps routing. Changing mode needs a CPA restart
	// because capabilities are declared at registration.
	ModeObserve Mode = "observe"
)

// Policy is what a client does when none of its own accounts and no spare
// account of another bucket can serve it.
type Policy string

const (
	// PolicyAny uses any usable account, preferring the one with most spare.
	PolicyAny Policy = "any"
	// PolicyFail rejects the request with a quota error.
	PolicyFail Policy = "fail"
	// PolicyWait holds the request (up to wait.max_wait) and then answers 429
	// with Retry-After set to the next refill, so the client backs off.
	PolicyWait Policy = "wait"
)

// ClientConfig is the routing policy of one client (one or more API keys).
type ClientConfig struct {
	// Keys identify the client's API keys, as the CPA caller scope
	// (sha256("cli-proxy-api:caller-scope:v1\x00"+key)) or as plain sha256(key)
	// (the format CPA-Manager-Plus uses for key aliases). Hex, any case.
	Keys []string `yaml:"keys" json:"-"`
	// Buckets in order of preference.
	Buckets []string `yaml:"buckets" json:"buckets"`
	// OnExhausted applies when no own or spare account is usable.
	OnExhausted Policy `yaml:"on_exhausted" json:"on_exhausted"`
	// Borrow allows using spare accounts of other buckets. Default true.
	Borrow *bool `yaml:"borrow" json:"borrow"`
}

// ReservePoint is one point of a share-of-the-week curve: with Left until the
// weekly reset, the share is Keep. In lend_reserve it is the share an account
// keeps for its home clients; in lend_limits the share a bucket may lend.
type ReservePoint struct {
	Left time.Duration
	Keep float64
}

// reserveAt returns a curve's share with left until the weekly reset: linear
// between points, the nearest point's share outside them, 0 without points.
func reserveAt(points []ReservePoint, left time.Duration) float64 {
	if len(points) == 0 {
		return 0
	}
	if left <= points[0].Left {
		return points[0].Keep
	}
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
		if left <= b.Left {
			f := float64(left-a.Left) / float64(b.Left-a.Left)
			return a.Keep + f*(b.Keep-a.Keep)
		}
	}
	return points[len(points)-1].Keep
}

// parseDuration reads a Go duration, or a whole or fractional number of
// days such as "7d" or "1.5d".
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(s)
}

// parseCurve reads a share of the week that may change toward the weekly
// reset: a number (the same share all week) or a map from time left to share
// such as {7d: 0.7, 8h: 0.01}. Points come back sorted by time left.
func parseCurve(name string, v any) ([]ReservePoint, []string) {
	var errs []string
	share := func(key string, x any) (float64, bool) {
		f, ok := toFloat(x)
		if !ok || f < 0 || f > 1 {
			errs = append(errs, fmt.Sprintf("%s%s: share must be a number in [0, 1], got %v", name, key, x))
			return 0, false
		}
		return f, true
	}
	var m map[string]any
	switch x := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		m = x
	case map[any]any:
		m = map[string]any{}
		for k, y := range x {
			m[fmt.Sprint(k)] = y
		}
	default:
		f, ok := share("", v)
		if !ok {
			return nil, errs
		}
		return []ReservePoint{{Left: 0, Keep: f}}, nil
	}
	var pts []ReservePoint
	for left, x := range m {
		d, err := parseDuration(left)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Sprintf("%s: want a time left such as 7d or 8h, got %q", name, left))
			continue
		}
		f, ok := share("."+left, x)
		if !ok {
			continue
		}
		for _, p := range pts {
			if p.Left == d {
				errs = append(errs, fmt.Sprintf("%s.%s: time left given twice", name, left))
			}
		}
		pts = append(pts, ReservePoint{Left: d, Keep: f})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].Left < pts[j].Left })
	return pts, errs
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	}
	return 0, false
}

// BorrowEnabled reports whether the client may use other buckets' spare.
func (c ClientConfig) BorrowEnabled() bool { return c.Borrow == nil || *c.Borrow }

// PacingConfig slows a client down when it will run out before a refill
// that is close.
type PacingConfig struct {
	Enabled  bool          `yaml:"enabled" json:"enabled"`
	Horizon  time.Duration `yaml:"horizon" json:"horizon"`
	MaxDelay time.Duration `yaml:"max_delay" json:"max_delay"`
}

// WaitConfig bounds the "wait" exhaustion policy.
type WaitConfig struct {
	Horizon time.Duration `yaml:"horizon" json:"horizon"`
	MaxWait time.Duration `yaml:"max_wait" json:"max_wait"`
}

// PricingConfig controls the models.dev price sync.
type PricingConfig struct {
	Sync         bool          `yaml:"sync" json:"sync"`
	SyncInterval time.Duration `yaml:"sync_interval" json:"sync_interval"`
	URL          string        `yaml:"url" json:"url"`
}

// Config is the plugins.configs.fefo-router section.
type Config struct {
	Mode Mode `yaml:"mode" json:"mode"`
	// SpareFactor k: an account lends quota while
	// remaining > k * expected usage by its home clients until its reset.
	SpareFactor float64 `yaml:"spare_factor" json:"spare_factor"`
	// MinSpare is the spare needed to start lending, as a share of the
	// account's weekly capacity, so rounding noise does not flip accounts in
	// and out.
	MinSpare float64 `yaml:"min_spare" json:"min_spare"`
	// LendReserve is the share of its weekly window an account keeps for its
	// home clients, by time left until the weekly reset, sorted by Left.
	// Shares between two points are interpolated linearly. Empty keeps none.
	LendReserve []ReservePoint `yaml:"-" json:"-"`
	// LendLimits caps what a bucket lends: lending bucket → target (another
	// bucket, or LendAny) → the share of the bucket's week (its accounts'
	// capacity summed) that the target's clients may use this weekly window,
	// by time left until each account's reset.
	LendLimits map[string]map[string][]ReservePoint `yaml:"-" json:"-"`
	// LendMinShort stops an account lending while its 5h window has less than
	// this share left, so borrowers cannot empty it under its home clients.
	LendMinShort float64 `yaml:"lend_min_short" json:"lend_min_short"`
	// MinRemaining blocks an account whose 5h or weekly window has less left.
	MinRemaining float64 `yaml:"min_remaining" json:"min_remaining"`
	// MinHistory is how much usage history a client needs before its demand
	// is trusted. Accounts whose home clients have less are never lent.
	MinHistory time.Duration `yaml:"min_history" json:"min_history"`
	// StaleAfter: quota windows reported longer ago are re-probed.
	StaleAfter time.Duration `yaml:"stale_after" json:"stale_after"`
	// ProbeInterval is the minimum gap between probes of one account.
	ProbeInterval time.Duration `yaml:"probe_interval" json:"probe_interval"`
	// SessionTTL is how long a (key, session) binding lasts without traffic.
	SessionTTL time.Duration `yaml:"session_ttl" json:"session_ttl"`
	// ScopedFamilies are model families (as in Anthropic's usage report,
	// "fable") that are limited by their own allowance. On an account whose
	// usage report has no allowance for one of them, its requests would bill
	// extra usage, so they are kept off that account.
	ScopedFamilies []string `yaml:"scoped_families" json:"scoped_families"`
	// AllowExtraUsage lets exhausted accounts with paid extra usage serve.
	AllowExtraUsage bool `yaml:"allow_extra_usage" json:"allow_extra_usage"`
	// UsagePollInterval is how often each Claude account's usage report is
	// fetched. Zero disables polling.
	UsagePollInterval time.Duration `yaml:"usage_poll_interval" json:"usage_poll_interval"`
	// RefreshInterval for host auth list sync and state saving.
	RefreshInterval time.Duration `yaml:"refresh_interval" json:"refresh_interval"`
	// StateFile persists quota, history and overrides. Empty keeps memory only.
	StateFile string `yaml:"state_file" json:"state_file"`
	// DisplayUnits is what the status text and UI show by default: usd or tokens.
	DisplayUnits string `yaml:"display_units" json:"display_units"`
	// LogDecisions writes one line per routing decision.
	LogDecisions bool `yaml:"log_decisions" json:"log_decisions"`

	// Buckets map a bucket name to account identifiers: e-mail/label, auth
	// file name or auth ID.
	Buckets map[string][]string `yaml:"buckets" json:"buckets"`
	// Capacity is each account's plan size relative to a Pro plan (default
	// 1). It is the prior for accounts whose dollar capacity is not learned yet.
	Capacity map[string]float64 `yaml:"capacity" json:"capacity"`
	// CapacityUSD pins an account's weekly capacity in API-equivalent dollars.
	CapacityUSD map[string]float64 `yaml:"capacity_usd" json:"capacity_usd"`
	// DefaultUnitUSD is the dollars per unit of Capacity before any account
	// of the provider has learned its capacity.
	DefaultUnitUSD float64 `yaml:"default_unit_usd" json:"default_unit_usd"`
	// LearnMinDelta is the weekly share a cycle must have seen before its
	// dollars-per-week estimate is used.
	LearnMinDelta float64 `yaml:"learn_min_delta" json:"learn_min_delta"`
	// Clients by alias.
	Clients map[string]ClientConfig `yaml:"clients" json:"clients"`
	// DefaultClient applies to keys not listed in Clients.
	DefaultClient ClientConfig `yaml:"default_client" json:"default_client"`

	Pacing PacingConfig `yaml:"pacing" json:"pacing"`
	Wait   WaitConfig   `yaml:"wait" json:"wait"`

	Pricing PricingConfig `yaml:"pricing" json:"pricing"`
	// Prices override the price table per model id, USD per 1M tokens.
	Prices map[string]pricing.Rates `yaml:"prices" json:"prices"`

	// Host-owned keys, accepted and ignored.
	Enabled  *bool `yaml:"enabled" json:"-"`
	Priority *int  `yaml:"priority" json:"-"`
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{
		Mode:              ModeActive,
		SpareFactor:       1.5,
		MinSpare:          0.03,
		LendMinShort:      0.3,
		MinRemaining:      0.01,
		MinHistory:        6 * time.Hour,
		StaleAfter:        30 * time.Minute,
		ProbeInterval:     5 * time.Minute,
		SessionTTL:        time.Hour,
		RefreshInterval:   time.Minute,
		UsagePollInterval: 5 * time.Minute,
		LogDecisions:      true,
		DisplayUnits:      UnitsUSD,
		DefaultUnitUSD:    220,
		ScopedFamilies:    []string{"fable"},
		LearnMinDelta:     0.05,
		DefaultClient:     ClientConfig{OnExhausted: PolicyAny},
		Pacing:            PacingConfig{Enabled: true, Horizon: 2 * time.Hour, MaxDelay: 30 * time.Second},
		Wait:              WaitConfig{Horizon: 2 * time.Hour, MaxWait: 30 * time.Second},
		Pricing:           PricingConfig{Sync: true, SyncInterval: 24 * time.Hour, URL: "https://models.dev/api.json"},
	}
}

// rawConfig mirrors Config with durations as strings so "30m" parses.
type rawConfig struct {
	Mode              *string                   `yaml:"mode"`
	SpareFactor       *float64                  `yaml:"spare_factor"`
	MinSpare          *float64                  `yaml:"min_spare"`
	LendReserve       any                       `yaml:"lend_reserve"`
	LendLimits        map[string]map[string]any `yaml:"lend_limits"`
	LendMinShort      *float64                  `yaml:"lend_min_short"`
	MinRemaining      *float64                  `yaml:"min_remaining"`
	MinHistory        *string                   `yaml:"min_history"`
	StaleAfter        *string                   `yaml:"stale_after"`
	ProbeInterval     *string                   `yaml:"probe_interval"`
	SessionTTL        *string                   `yaml:"session_ttl"`
	AllowExtraUsage   *bool                     `yaml:"allow_extra_usage"`
	ScopedFamilies    *[]string                 `yaml:"scoped_families"`
	RefreshInterval   *string                   `yaml:"refresh_interval"`
	UsagePollInterval *string                   `yaml:"usage_poll_interval"`
	StateFile         *string                   `yaml:"state_file"`
	LogDecisions      *bool                     `yaml:"log_decisions"`
	DisplayUnits      *string                   `yaml:"display_units"`
	Buckets           map[string][]string       `yaml:"buckets"`
	Capacity          map[string]float64        `yaml:"capacity"`
	CapacityUSD       map[string]float64        `yaml:"capacity_usd"`
	DefaultUnitUSD    *float64                  `yaml:"default_unit_usd"`
	LearnMinDelta     *float64                  `yaml:"learn_min_delta"`
	Clients           map[string]ClientConfig   `yaml:"clients"`
	DefaultClient     *ClientConfig             `yaml:"default_client"`
	Pacing            *struct {
		Enabled  *bool   `yaml:"enabled"`
		Horizon  *string `yaml:"horizon"`
		MaxDelay *string `yaml:"max_delay"`
	} `yaml:"pacing"`
	Wait *struct {
		Horizon *string `yaml:"horizon"`
		MaxWait *string `yaml:"max_wait"`
	} `yaml:"wait"`
	Pricing *struct {
		Sync         *bool   `yaml:"sync"`
		SyncInterval *string `yaml:"sync_interval"`
		URL          *string `yaml:"url"`
	} `yaml:"pricing"`
	Prices map[string]struct {
		Input      float64 `yaml:"input"`
		Output     float64 `yaml:"output"`
		CacheRead  float64 `yaml:"cache_read"`
		CacheWrite float64 `yaml:"cache_write"`
	} `yaml:"prices"`
	Enabled  any `yaml:"enabled"`
	Priority any `yaml:"priority"`
}

// ParseConfig decodes and validates the plugin's YAML section.
func ParseConfig(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(strings.TrimSpace(string(raw))) == 0 {
		return cfg, nil
	}
	var rc rawConfig
	if err := yaml.Unmarshal(raw, &rc); err != nil {
		return cfg, fmt.Errorf("decode config: %w", err)
	}
	var errs []string
	dur := func(name string, v *string, dst *time.Duration, min time.Duration) {
		if v == nil {
			return
		}
		d, err := parseDuration(*v)
		if err != nil || d < min {
			errs = append(errs, fmt.Sprintf("%s: want a duration >= %s, got %q", name, min, *v))
			return
		}
		*dst = d
	}
	if rc.Mode != nil {
		switch m := Mode(strings.ToLower(strings.TrimSpace(*rc.Mode))); m {
		case ModeActive, ModeObserve:
			cfg.Mode = m
		default:
			errs = append(errs, fmt.Sprintf("mode: want active or observe, got %q", *rc.Mode))
		}
	}
	if rc.SpareFactor != nil {
		if *rc.SpareFactor < 1 {
			errs = append(errs, "spare_factor: must be >= 1")
		} else {
			cfg.SpareFactor = *rc.SpareFactor
		}
	}
	if rc.MinSpare != nil {
		cfg.MinSpare = *rc.MinSpare
	}
	pts, perrs := parseCurve("lend_reserve", rc.LendReserve)
	cfg.LendReserve, errs = pts, append(errs, perrs...)
	if rc.LendMinShort != nil {
		if *rc.LendMinShort < 0 || *rc.LendMinShort > 1 {
			errs = append(errs, "lend_min_short: must be in [0, 1]")
		} else {
			cfg.LendMinShort = *rc.LendMinShort
		}
	}
	if rc.MinRemaining != nil {
		if *rc.MinRemaining < 0 || *rc.MinRemaining >= 1 {
			errs = append(errs, "min_remaining: must be in [0, 1)")
		} else {
			cfg.MinRemaining = *rc.MinRemaining
		}
	}
	dur("min_history", rc.MinHistory, &cfg.MinHistory, 0)
	dur("stale_after", rc.StaleAfter, &cfg.StaleAfter, time.Minute)
	dur("probe_interval", rc.ProbeInterval, &cfg.ProbeInterval, 10*time.Second)
	dur("session_ttl", rc.SessionTTL, &cfg.SessionTTL, time.Minute)
	dur("refresh_interval", rc.RefreshInterval, &cfg.RefreshInterval, 10*time.Second)
	if v := rc.UsagePollInterval; v != nil {
		if d, err := parseDuration(*v); err == nil && d == 0 {
			cfg.UsagePollInterval = 0
		} else {
			dur("usage_poll_interval", v, &cfg.UsagePollInterval, time.Minute)
		}
	}
	if rc.AllowExtraUsage != nil {
		cfg.AllowExtraUsage = *rc.AllowExtraUsage
	}
	if rc.ScopedFamilies != nil {
		cfg.ScopedFamilies = []string{}
		for _, f := range *rc.ScopedFamilies {
			f = strings.ToLower(strings.TrimSpace(f))
			if f == "" || strings.ContainsAny(f, " \t") {
				errs = append(errs, fmt.Sprintf("scoped_families: invalid family %q", f))
				continue
			}
			cfg.ScopedFamilies = append(cfg.ScopedFamilies, f)
		}
	}
	if rc.StateFile != nil {
		cfg.StateFile = strings.TrimSpace(*rc.StateFile)
	}
	if rc.LogDecisions != nil {
		cfg.LogDecisions = *rc.LogDecisions
	}
	if rc.DisplayUnits != nil {
		if u, ok := parseUnits(*rc.DisplayUnits); ok {
			cfg.DisplayUnits = u
		} else {
			errs = append(errs, fmt.Sprintf("display_units: want usd, tokens, pct_relative or pct_absolute, got %q", *rc.DisplayUnits))
		}
	}
	if rc.Pacing != nil {
		if rc.Pacing.Enabled != nil {
			cfg.Pacing.Enabled = *rc.Pacing.Enabled
		}
		dur("pacing.horizon", rc.Pacing.Horizon, &cfg.Pacing.Horizon, 0)
		dur("pacing.max_delay", rc.Pacing.MaxDelay, &cfg.Pacing.MaxDelay, 0)
	}
	if rc.Wait != nil {
		dur("wait.horizon", rc.Wait.Horizon, &cfg.Wait.Horizon, 0)
		dur("wait.max_wait", rc.Wait.MaxWait, &cfg.Wait.MaxWait, 0)
	}
	if rc.Pricing != nil {
		if rc.Pricing.Sync != nil {
			cfg.Pricing.Sync = *rc.Pricing.Sync
		}
		dur("pricing.sync_interval", rc.Pricing.SyncInterval, &cfg.Pricing.SyncInterval, time.Hour)
		if rc.Pricing.URL != nil {
			u := strings.TrimSpace(*rc.Pricing.URL)
			if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
				errs = append(errs, fmt.Sprintf("pricing.url: want an http(s) URL, got %q", u))
			} else {
				cfg.Pricing.URL = u
			}
		}
	}
	cfg.Prices = map[string]pricing.Rates{}
	for k, v := range rc.Prices {
		rates := pricing.Rates{Input: v.Input, Output: v.Output, CacheRead: v.CacheRead, CacheWrite: v.CacheWrite}
		if rates.Input < 0 || rates.Output < 0 || rates.CacheRead < 0 || rates.CacheWrite < 0 || rates.Input+rates.Output == 0 {
			errs = append(errs, fmt.Sprintf("prices.%s: rates must be >= 0 with input or output > 0", k))
			continue
		}
		cfg.Prices[strings.ToLower(strings.TrimSpace(k))] = rates
	}
	cfg.Buckets = map[string][]string{}
	for name, members := range rc.Buckets {
		name = strings.TrimSpace(name)
		for _, m := range members {
			if m = strings.TrimSpace(m); m != "" {
				cfg.Buckets[name] = append(cfg.Buckets[name], m)
			}
		}
	}
	cfg.LendLimits = map[string]map[string][]ReservePoint{}
	for bucket, targets := range rc.LendLimits {
		bucket = strings.TrimSpace(bucket)
		if _, ok := cfg.Buckets[bucket]; !ok {
			errs = append(errs, fmt.Sprintf("lend_limits.%s: unknown bucket", bucket))
			continue
		}
		for target, v := range targets {
			target = strings.TrimSpace(target)
			name := "lend_limits." + bucket + "." + target
			_, known := cfg.Buckets[target]
			switch {
			case target == bucket:
				errs = append(errs, name+": a bucket does not lend to itself")
			case target != LendAny && !known:
				errs = append(errs, fmt.Sprintf("%s: unknown bucket %q (name a bucket, or %s for every other bucket)", name, target, LendAny))
			default:
				pts, perrs := parseCurve(name, v)
				errs = append(errs, perrs...)
				if len(perrs) == 0 && len(pts) > 0 {
					if cfg.LendLimits[bucket] == nil {
						cfg.LendLimits[bucket] = map[string][]ReservePoint{}
					}
					cfg.LendLimits[bucket][target] = pts
				}
			}
		}
	}
	cfg.Capacity = map[string]float64{}
	for k, v := range rc.Capacity {
		if v <= 0 {
			errs = append(errs, fmt.Sprintf("capacity.%s: must be > 0", k))
			continue
		}
		cfg.Capacity[strings.ToLower(strings.TrimSpace(k))] = v
	}
	cfg.CapacityUSD = map[string]float64{}
	for k, v := range rc.CapacityUSD {
		if v <= 0 {
			errs = append(errs, fmt.Sprintf("capacity_usd.%s: must be > 0", k))
			continue
		}
		cfg.CapacityUSD[strings.ToLower(strings.TrimSpace(k))] = v
	}
	if rc.DefaultUnitUSD != nil {
		if *rc.DefaultUnitUSD <= 0 {
			errs = append(errs, "default_unit_usd: must be > 0")
		} else {
			cfg.DefaultUnitUSD = *rc.DefaultUnitUSD
		}
	}
	if rc.LearnMinDelta != nil {
		if *rc.LearnMinDelta <= 0 || *rc.LearnMinDelta > 1 {
			errs = append(errs, "learn_min_delta: must be in (0, 1]")
		} else {
			cfg.LearnMinDelta = *rc.LearnMinDelta
		}
	}
	checkClient := func(name string, c ClientConfig) ClientConfig {
		if c.OnExhausted == "" {
			c.OnExhausted = PolicyAny
		}
		c.OnExhausted = Policy(strings.ToLower(string(c.OnExhausted)))
		switch c.OnExhausted {
		case PolicyAny, PolicyFail, PolicyWait:
		default:
			errs = append(errs, fmt.Sprintf("%s.on_exhausted: want any, fail or wait, got %q", name, c.OnExhausted))
		}
		for _, b := range c.Buckets {
			if _, ok := cfg.Buckets[b]; !ok {
				errs = append(errs, fmt.Sprintf("%s.buckets: unknown bucket %q", name, b))
			}
		}
		for i, k := range c.Keys {
			c.Keys[i] = strings.ToLower(strings.TrimSpace(k))
		}
		return c
	}
	cfg.Clients = map[string]ClientConfig{}
	for name, c := range rc.Clients {
		cfg.Clients[name] = checkClient("clients."+name, c)
	}
	if rc.DefaultClient != nil {
		cfg.DefaultClient = checkClient("default_client", *rc.DefaultClient)
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return DefaultConfig(), fmt.Errorf("invalid config: %s", strings.Join(errs, "; "))
	}
	return cfg, nil
}

// Display units for status text and the UI. The two percent units show
// API-equivalent dollars as a share of a week: of the thing shown (an
// account, a pool, a bucket) for pct_relative, of the provider's largest
// account for pct_absolute.
const (
	UnitsUSD         = "usd"
	UnitsTokens      = "tokens"
	UnitsPctRelative = "pct_relative"
	UnitsPctAbsolute = "pct_absolute"
)

func parseUnits(s string) (string, bool) {
	switch u := strings.ToLower(strings.TrimSpace(s)); u {
	case UnitsUSD, UnitsTokens, UnitsPctRelative, UnitsPctAbsolute:
		return u, true
	}
	return "", false
}

// bucketsOf returns, for an account, the buckets that list it.
func (c Config) bucketsOf(a accountInfo) []string {
	var out []string
	for name, members := range c.Buckets {
		for _, m := range members {
			if a.matches(m) {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// capacityUSDOf returns an account's pinned dollar capacity.
func (c Config) capacityUSDOf(a accountInfo) (float64, bool) {
	for k, v := range c.CapacityUSD {
		if a.matches(k) {
			return v, true
		}
	}
	return 0, false
}

// capacityOf returns an account's plan weight.
func (c Config) capacityOf(a accountInfo) float64 {
	for k, v := range c.Capacity {
		if a.matches(k) {
			return v
		}
	}
	return 1
}
