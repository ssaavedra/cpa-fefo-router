package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/ssaavedra/cpa-fefo-router/internal/quota"
)

const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

// usagePoll is an account's latest Claude usage report and what CPA spent
// on it since, per scope family ("fable").
type usagePoll struct {
	Report quota.UsageReport `json:"report"`
	// SpendSince is the high-end dollars per family since Report was taken.
	SpendSince map[string]float64 `json:"spend_since,omitempty"`
	Error      string             `json:"error,omitempty"`
	ErrorAt    time.Time          `json:"error_at,omitzero"`
	NextAt     time.Time          `json:"-"`
}

// pollUsage fetches the usage report of every Claude OAuth account that is
// due. Host calls block, so r.mu is released around each one.
func (r *Router) pollUsage(ctx context.Context, now time.Time) {
	type job struct{ id, index string }
	var jobs []job
	r.mu.Lock()
	if interval := r.cfg.UsagePollInterval; r.fetcher != nil && interval > 0 {
		for _, id := range sortedKeys(r.accounts) {
			st := r.accounts[id]
			if st.Info.Provider != "claude" || st.Info.AuthIndex == "" || st.Disabled ||
				strings.EqualFold(st.Info.AuthKind, "apikey") || now.Before(st.Usage.NextAt) {
				continue
			}
			st.Usage.NextAt = now.Add(jittered(interval, r.jitter()))
			jobs = append(jobs, job{id, st.Info.AuthIndex})
		}
	}
	r.mu.Unlock()
	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		rep, err := r.fetchUsage(ctx, j.index)
		r.mu.Lock()
		r.applyUsageLocked(j.id, rep, err)
		r.mu.Unlock()
	}
}

// fetchUsage reads one account's report. The access token goes only into
// the request header: every error is scrubbed of it before it is returned.
func (r *Router) fetchUsage(ctx context.Context, authIndex string) (quota.UsageReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cred, err := r.fetcher.AuthJSON(ctx, authIndex)
	if err != nil {
		return quota.UsageReport{}, err
	}
	token, err := ClaudeAccessToken(cred)
	if err != nil {
		return quota.UsageReport{}, err
	}
	scrub := func(err error) error {
		return errors.New(strings.ReplaceAll(err.Error(), token, "[redacted]"))
	}
	resp, err := r.fetcher.HTTPDo(ctx, pluginapi.HTTPRequest{
		Method: http.MethodGet, URL: claudeUsageURL,
		Headers: http.Header{
			"Authorization":  {"Bearer " + token},
			"Anthropic-Beta": {"oauth-2025-04-20"},
			"Accept":         {"application/json"},
		},
	})
	switch {
	case err != nil:
		return quota.UsageReport{}, scrub(err)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return quota.UsageReport{}, fmt.Errorf("usage endpoint rejected the access token (%d)", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return quota.UsageReport{}, fmt.Errorf("usage endpoint answered %d", resp.StatusCode)
	}
	rep, err := quota.ParseClaudeUsage(resp.Body, r.now())
	if err != nil {
		return quota.UsageReport{}, scrub(err)
	}
	return rep, nil
}

func (r *Router) applyUsageLocked(id string, rep quota.UsageReport, err error) {
	st := r.accounts[id]
	if st == nil {
		return
	}
	r.dirty = true
	if err != nil {
		if st.Usage.Error != err.Error() && r.log != nil {
			r.log("warn", "fefo-router usage poll failed; retrying next interval", map[string]any{"account": st.Info.display(), "error": err.Error()})
		}
		st.Usage.Error, st.Usage.ErrorAt = err.Error(), r.now()
		return
	}
	st.Usage.Report, st.Usage.SpendSince, st.Usage.Error, st.Usage.ErrorAt = rep, nil, "", time.Time{}
	snap := rep.Snapshot()
	if st.HasSnapshot {
		st.Snapshot = st.Snapshot.Merge(snap)
	} else if len(snap.Windows) > 0 {
		st.Snapshot, st.HasSnapshot = snap, true
	}
}

// UsageView is an account's last Claude usage report in the status.
type UsageView struct {
	PolledAt          time.Time    `json:"polled_at,omitzero"`
	Error             string       `json:"error,omitempty"`
	ErrorAt           time.Time    `json:"error_at,omitzero"`
	ExtraUsageEnabled *bool        `json:"extra_usage_enabled,omitempty"`
	SpendLimitReached bool         `json:"spend_limit_reached,omitempty"`
	Scoped            []ScopedView `json:"scoped,omitempty"`
}

// ScopedView is one model family's sub-allowance on an account.
type ScopedView struct {
	Family             string    `json:"family"`
	Allowance          float64   `json:"allowance"` // share of the weekly window
	PolledRemaining    float64   `json:"polled_remaining"`
	SpentSinceUSD      float64   `json:"spent_since_usd"`
	EstimatedRemaining float64   `json:"estimated_remaining"`
	Reset              time.Time `json:"reset,omitzero"`
	Blocked            string    `json:"blocked,omitempty"`
}

func (r *Router) usageViewLocked(st *accountState, capacityUSD float64, now time.Time) *UsageView {
	u := st.Usage
	if !u.Report.HasReport && u.Error == "" {
		return nil
	}
	v := &UsageView{Error: u.Error, ErrorAt: u.ErrorAt}
	if rep := u.Report; rep.HasReport {
		v.PolledAt, v.SpendLimitReached = rep.ObservedAt, rep.SpendLimitReached
		if rep.ExtraUsageHasState {
			enabled := rep.ExtraUsageEnabled
			v.ExtraUsageEnabled = &enabled
		}
		for _, w := range rep.ScopedWindows {
			sv := r.scopedLocked(st, capacityUSD, w.Family, now)
			v.Scoped = append(v.Scoped, ScopedView{Family: w.Family, Allowance: w.Allowance, PolledRemaining: w.Remaining,
				SpentSinceUSD: sv.Spent, EstimatedRemaining: sv.Estimated, Reset: w.ResetAt, Blocked: sv.Block})
		}
		for _, fam := range r.cfg.ScopedFamilies {
			if sv := r.scopedLocked(st, capacityUSD, fam, now); !sv.HasWindow && sv.Block != "" {
				v.Scoped = append(v.Scoped, ScopedView{Family: fam, Blocked: sv.Block})
			}
		}
	}
	return v
}

// usagePollJitter spreads each account's polls by up to ±30 % of the interval,
// so polls do not arrive at Anthropic in lockstep while the mean gap stays
// at the configured interval.
const usagePollJitter = 0.3

func jittered(interval time.Duration, u float64) time.Duration {
	return time.Duration(float64(interval) * (1 + usagePollJitter*(2*u-1)))
}
