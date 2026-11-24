package quota

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ScopedWindow is a weekly window that applies to one model family only, such
// as Anthropic's Fable sub-limit.
type ScopedWindow struct {
	Window
	// Family is the lower-case model family the window covers ("fable").
	Family string
	// Allowance is the share of the weekly window's capacity the family may use,
	// in (0, 1]. Window.Remaining is relative to this allowance, not to the week.
	Allowance float64
}

// UsageReport is the parsed body of Anthropic's /api/oauth/usage endpoint. The
// zero value means "not polled yet".
type UsageReport struct {
	// HasReport is true once a report was parsed. With HasReport set, a family
	// with no entry in Scoped has no included allowance on this plan.
	HasReport  bool
	ObservedAt time.Time
	// Windows are the shared "5h" and "7d" windows. A window whose reset time
	// the report left null is kept with a zero ResetAt.
	Windows       []Window
	ScopedWindows []ScopedWindow
	// ExtraUsageHasState is false when the report carried no extra_usage object.
	ExtraUsageHasState bool
	ExtraUsageEnabled  bool
	SpendLimitReached  bool
}

type usageLimit struct {
	Kind     string   `json:"kind"`
	Percent  *float64 `json:"percent"`
	ResetsAt *string  `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

type usageShare struct {
	LimitIndex               *int     `json:"limit_index"`
	AllowancePercentOfWeekly *float64 `json:"allowance_percent_of_weekly"`
}

type usageLegacy struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type usageBody struct {
	Limits             json.RawMessage `json:"limits"`
	WeeklyScopedShares []usageShare    `json:"weekly_scoped_shares"`
	FiveHour           *usageLegacy    `json:"five_hour"`
	SevenDay           *usageLegacy    `json:"seven_day"`
	ExtraUsage         *struct {
		IsEnabled         bool `json:"is_enabled"`
		SpendLimitReached bool `json:"spend_limit_reached"`
	} `json:"extra_usage"`
}

const (
	fiveHour = 5 * time.Hour
	sevenDay = 7 * 24 * time.Hour
)

// ParseClaudeUsage parses a /api/oauth/usage response body. The legacy
// five_hour and seven_day keys are read only when the body has no limits list.
func ParseClaudeUsage(raw []byte, observedAt time.Time) (UsageReport, error) {
	var body usageBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return UsageReport{}, fmt.Errorf("decode claude usage: %w", err)
	}
	rep := UsageReport{HasReport: true, ObservedAt: observedAt}

	if s := strings.TrimSpace(string(body.Limits)); s == "" || s == "null" {
		if w, ok := legacyWindow("5h", fiveHour, body.FiveHour, observedAt); ok {
			rep.Windows = append(rep.Windows, w)
		}
		if w, ok := legacyWindow("7d", sevenDay, body.SevenDay, observedAt); ok {
			rep.Windows = append(rep.Windows, w)
		}
	} else {
		var limits []usageLimit
		if err := json.Unmarshal(body.Limits, &limits); err != nil {
			return UsageReport{}, fmt.Errorf("decode claude usage limits: %w", err)
		}
		for i, l := range limits {
			switch l.Kind {
			case "session":
				rep.Windows = append(rep.Windows, usageWindow("5h", fiveHour, l.Percent, l.ResetsAt, observedAt))
			case "weekly_all":
				rep.Windows = append(rep.Windows, usageWindow("7d", sevenDay, l.Percent, l.ResetsAt, observedAt))
			case "weekly_scoped":
				if l.Scope == nil || l.Scope.Model == nil {
					continue
				}
				family := strings.ToLower(strings.TrimSpace(l.Scope.Model.DisplayName))
				if family == "" {
					continue
				}
				rep.ScopedWindows = append(rep.ScopedWindows, ScopedWindow{
					Window:    usageWindow("7d:"+family, sevenDay, l.Percent, l.ResetsAt, observedAt),
					Family:    family,
					Allowance: scopedAllowance(body.WeeklyScopedShares, i),
				})
			}
		}
	}

	if e := body.ExtraUsage; e != nil {
		rep.ExtraUsageHasState = true
		rep.ExtraUsageEnabled = e.IsEnabled
		rep.SpendLimitReached = e.SpendLimitReached
	}
	return rep, nil
}

// scopedAllowance finds the share whose limit_index points at limits[index].
// A scoped window without a usable share is treated as the full weekly
// capacity.
func scopedAllowance(shares []usageShare, index int) float64 {
	for _, s := range shares {
		if s.LimitIndex != nil && *s.LimitIndex == index && s.AllowancePercentOfWeekly != nil {
			if a := *s.AllowancePercentOfWeekly / 100; a > 0 {
				return clamp01(a)
			}
		}
	}
	return 1
}

func usageWindow(name string, length time.Duration, percent *float64, resetsAt *string, observedAt time.Time) Window {
	var used float64
	if percent != nil {
		used = *percent
	}
	w := Window{Name: name, Remaining: clamp01((100 - used) / 100), Length: length, ObservedAt: observedAt}
	if resetsAt != nil {
		if t, err := time.Parse(time.RFC3339, *resetsAt); err == nil {
			w.ResetAt = t
		}
	}
	return w
}

func legacyWindow(name string, length time.Duration, l *usageLegacy, observedAt time.Time) (Window, bool) {
	if l == nil || l.Utilization == nil {
		return Window{}, false
	}
	return usageWindow(name, length, l.Utilization, l.ResetsAt, observedAt), true
}

// Scoped returns the window for a model family (case-insensitive).
func (r UsageReport) Scoped(family string) (ScopedWindow, bool) {
	family = strings.ToLower(strings.TrimSpace(family))
	for _, w := range r.ScopedWindows {
		if w.Family == family {
			return w, true
		}
	}
	return ScopedWindow{}, false
}

// Snapshot returns the shared part of the report for Snapshot.Merge. Scoped
// windows are not included: they would never be replaced by header-derived
// snapshots and would leak into the shared-window logic.
func (r UsageReport) Snapshot() Snapshot {
	snap := Snapshot{Provider: "claude", ObservedAt: r.ObservedAt, Windows: append([]Window(nil), r.Windows...)}
	if r.ExtraUsageHasState {
		snap.ExtraUsage = r.ExtraUsageEnabled
		snap.ExtraUsageObservedAt = r.ObservedAt
	}
	return snap
}
