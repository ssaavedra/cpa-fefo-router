package router

import (
	"fmt"
	"sort"
	"strings"
	"time"

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

// Config is the plugins.configs.fefo-router section.
type Config struct {
	Mode Mode `yaml:"mode" json:"mode"`
	// SpareFactor k: an account lends quota while
	// remaining > k * expected usage by its home clients until its reset.
	SpareFactor float64 `yaml:"spare_factor" json:"spare_factor"`
	// MinSpare is the spare (in account-weeks) needed to start lending, so
	// rounding noise does not flip accounts in and out.
	MinSpare float64 `yaml:"min_spare" json:"min_spare"`
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
	// AllowExtraUsage lets exhausted accounts with paid extra usage serve.
	AllowExtraUsage bool `yaml:"allow_extra_usage" json:"allow_extra_usage"`
	// RefreshInterval for host auth list sync and state saving.
	RefreshInterval time.Duration `yaml:"refresh_interval" json:"refresh_interval"`
	// StateFile persists quota, history and overrides. Empty keeps memory only.
	StateFile string `yaml:"state_file" json:"state_file"`
	// LogDecisions writes one line per routing decision.
	LogDecisions bool `yaml:"log_decisions" json:"log_decisions"`

	// Buckets map a bucket name to account identifiers: e-mail/label, auth
	// file name or auth ID.
	Buckets map[string][]string `yaml:"buckets" json:"buckets"`
	// Capacity weights accounts with bigger plans (default 1 = one plan-week).
	Capacity map[string]float64 `yaml:"capacity" json:"capacity"`
	// Clients by alias.
	Clients map[string]ClientConfig `yaml:"clients" json:"clients"`
	// DefaultClient applies to keys not listed in Clients.
	DefaultClient ClientConfig `yaml:"default_client" json:"default_client"`

	Pacing PacingConfig `yaml:"pacing" json:"pacing"`
	Wait   WaitConfig   `yaml:"wait" json:"wait"`

	// Host-owned keys, accepted and ignored.
	Enabled  *bool `yaml:"enabled" json:"-"`
	Priority *int  `yaml:"priority" json:"-"`
}

// DefaultConfig returns the defaults.
func DefaultConfig() Config {
	return Config{
		Mode:            ModeActive,
		SpareFactor:     1.5,
		MinSpare:        0.03,
		MinRemaining:    0.01,
		MinHistory:      6 * time.Hour,
		StaleAfter:      30 * time.Minute,
		ProbeInterval:   5 * time.Minute,
		SessionTTL:      time.Hour,
		RefreshInterval: time.Minute,
		LogDecisions:    true,
		DefaultClient:   ClientConfig{OnExhausted: PolicyAny},
		Pacing:          PacingConfig{Enabled: true, Horizon: 2 * time.Hour, MaxDelay: 30 * time.Second},
		Wait:            WaitConfig{Horizon: 2 * time.Hour, MaxWait: 30 * time.Second},
	}
}

// rawConfig mirrors Config with durations as strings so "30m" parses.
type rawConfig struct {
	Mode            *string                 `yaml:"mode"`
	SpareFactor     *float64                `yaml:"spare_factor"`
	MinSpare        *float64                `yaml:"min_spare"`
	MinRemaining    *float64                `yaml:"min_remaining"`
	MinHistory      *string                 `yaml:"min_history"`
	StaleAfter      *string                 `yaml:"stale_after"`
	ProbeInterval   *string                 `yaml:"probe_interval"`
	SessionTTL      *string                 `yaml:"session_ttl"`
	AllowExtraUsage *bool                   `yaml:"allow_extra_usage"`
	RefreshInterval *string                 `yaml:"refresh_interval"`
	StateFile       *string                 `yaml:"state_file"`
	LogDecisions    *bool                   `yaml:"log_decisions"`
	Buckets         map[string][]string     `yaml:"buckets"`
	Capacity        map[string]float64      `yaml:"capacity"`
	Clients         map[string]ClientConfig `yaml:"clients"`
	DefaultClient   *ClientConfig           `yaml:"default_client"`
	Pacing          *struct {
		Enabled  *bool   `yaml:"enabled"`
		Horizon  *string `yaml:"horizon"`
		MaxDelay *string `yaml:"max_delay"`
	} `yaml:"pacing"`
	Wait *struct {
		Horizon *string `yaml:"horizon"`
		MaxWait *string `yaml:"max_wait"`
	} `yaml:"wait"`
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
		d, err := time.ParseDuration(strings.TrimSpace(*v))
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
	if rc.AllowExtraUsage != nil {
		cfg.AllowExtraUsage = *rc.AllowExtraUsage
	}
	if rc.StateFile != nil {
		cfg.StateFile = strings.TrimSpace(*rc.StateFile)
	}
	if rc.LogDecisions != nil {
		cfg.LogDecisions = *rc.LogDecisions
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
	cfg.Buckets = map[string][]string{}
	for name, members := range rc.Buckets {
		name = strings.TrimSpace(name)
		for _, m := range members {
			if m = strings.TrimSpace(m); m != "" {
				cfg.Buckets[name] = append(cfg.Buckets[name], m)
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

// capacityOf returns an account's plan weight.
func (c Config) capacityOf(a accountInfo) float64 {
	for k, v := range c.Capacity {
		if a.matches(k) {
			return v
		}
	}
	return 1
}
