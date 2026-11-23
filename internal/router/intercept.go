package router

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// gate is the interceptor's verdict for one request.
type gate struct {
	Delay      time.Duration
	Terminate  bool
	RetryAfter time.Duration
	Reason     string
}

// Intercept runs before credential selection. It implements the "wait"
// exhaustion policy and pacing: when a client's only remaining accounts would
// run out before a refill that is close, each request is delayed so the
// client's own retry/turn loop naturally stretches to the refill.
func (r *Router) Intercept(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	provider := providerOfModel(firstNonEmpty(req.RequestedModel, req.Model))
	if provider == "" {
		return pluginapi.RequestInterceptResponse{}
	}
	scope := req.CallerScope
	if scope == "" {
		scope, _ = req.Metadata["caller_scope"].(string)
	}
	g, client := r.evaluateGate(scope, provider)
	if g.Delay > 0 {
		r.sleep(g.Delay)
		r.mu.Lock()
		r.metrics.paced(client, g.Delay)
		r.mu.Unlock()
		if r.Config().LogDecisions && r.log != nil {
			r.log("info", fmt.Sprintf("burndown pace client=%s %s +%s: %s", client, provider, g.Delay.Round(time.Millisecond), g.Reason),
				map[string]any{"client": client, "provider": provider, "delay_ms": g.Delay.Milliseconds(), "reason": g.Reason})
		}
		if g.Terminate {
			// Re-check after waiting: quota may have refilled meanwhile.
			if again, _ := r.evaluateGate(scope, provider); !again.Terminate {
				return pluginapi.RequestInterceptResponse{}
			}
		}
	}
	if !g.Terminate {
		return pluginapi.RequestInterceptResponse{}
	}
	r.mu.Lock()
	r.metrics.waitRejected(client)
	r.mu.Unlock()
	secs := int(math.Ceil(g.RetryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	body, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "rate_limit_error",
			"message": fmt.Sprintf("burndown-router: %s; quota refills in %s", g.Reason, fmtDur(g.RetryAfter)),
		},
	})
	return pluginapi.RequestInterceptResponse{
		Terminate:  true,
		StatusCode: http.StatusTooManyRequests,
		ResponseHeaders: http.Header{
			"Content-Type": {"application/json"},
			"Retry-After":  {strconv.Itoa(secs)},
		},
		ResponseBody: body,
	}
}

func (r *Router) evaluateGate(scope, provider string) (gate, string) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	client := r.clientForScopeLocked(scope)
	cfg := r.cfg
	pc := r.newPlanContextLocked(provider, now)
	order, policy, _ := pc.plan(client, cfg.MinSpare)
	cc := r.clientConfigLocked(client)

	// Accounts the client can use without falling back to last resort.
	var core []*eval
	for _, p := range order {
		if p.Role != RoleLastResort {
			core = append(core, pc.evals[p.ID])
		}
	}

	if len(core) == 0 {
		if policy != PolicyWait {
			return gate{}, client
		}
		// Next refill of any account the client could use: own accounts, or
		// any blocked account if it has no buckets.
		refill := time.Time{}
		for _, id := range pc.order {
			e := pc.evals[id]
			mine := len(cc.Buckets) == 0
			for _, b := range cc.Buckets {
				mine = mine || contains(e.Buckets, b)
			}
			if !mine || e.Usable || e.BlockedUntil.IsZero() {
				continue
			}
			if refill.IsZero() || e.BlockedUntil.Before(refill) {
				refill = e.BlockedUntil
			}
		}
		if refill.IsZero() {
			return gate{Terminate: true, RetryAfter: cfg.Wait.MaxWait, Reason: "no usable account and no known refill"}, client
		}
		until := refill.Sub(now)
		g := gate{Terminate: true, RetryAfter: until, Reason: "own accounts exhausted (policy wait)"}
		if until <= cfg.Wait.Horizon {
			g.Delay = minDur(cfg.Wait.MaxWait, until)
		}
		return g, client
	}

	if !cfg.Pacing.Enabled || len(core) != 1 {
		return gate{}, client
	}
	// Exactly one account left for this client: pace if it would run out of
	// any window before that window refills, and the refill is close.
	e := core[0]
	st := r.accounts[e.ID]
	best := gate{}
	for _, w := range []struct {
		name  string
		rem   float64
		reset time.Time
	}{{"short", e.ShortRemaining, e.ShortReset}, {"weekly", e.WeeklyRemaining, e.WeeklyReset}} {
		if w.reset.IsZero() {
			continue
		}
		until := w.reset.Sub(now)
		if until <= 0 || until > cfg.Pacing.Horizon {
			continue
		}
		rate := recentRate(st, w.name == "weekly", now)
		if rate <= 0 {
			continue
		}
		hoursLeft := w.rem / rate
		if hoursLeft >= until.Hours() {
			continue
		}
		reqs := r.recentRequestsLocked(e.ID, now)
		if reqs <= 0 {
			continue
		}
		interval := time.Hour / time.Duration(reqs)
		stretch := until.Hours()/math.Max(hoursLeft, 1e-6) - 1
		delay := time.Duration(float64(interval) * stretch)
		if delay > cfg.Pacing.MaxDelay {
			delay = cfg.Pacing.MaxDelay
		}
		if delay > best.Delay {
			best = gate{Delay: delay, Reason: fmt.Sprintf("%s: %s window %.0f%% left lasts ~%s at current pace, refills in %s",
				e.Name, w.name, w.rem*100, fmtDur(time.Duration(hoursLeft*float64(time.Hour))), fmtDur(until))}
		}
	}
	return best, client
}

// recentRate is the account's consumption of a window over the last hour, in
// window-fractions per hour, from readings within the same window period.
func recentRate(st *accountState, weekly bool, now time.Time) float64 {
	var first, last *usedSample
	for i := range st.Recent {
		s := &st.Recent[i]
		isWeekly := s.Window == "7d" || s.Window == "secondary"
		if isWeekly != weekly || now.Sub(s.At) > time.Hour {
			continue
		}
		if first == nil || !sameReset(first.Reset, s.Reset) {
			first = s
		}
		last = s
	}
	if first == nil || last == nil || last == first {
		return 0
	}
	span := last.At.Sub(first.At).Hours()
	if span < 5.0/60 {
		return 0
	}
	return math.Max(0, (last.Used-first.Used)/span)
}

// recentRequestsLocked counts requests on an account in the last hour.
func (r *Router) recentRequestsLocked(authID string, now time.Time) int {
	n := 0
	h := hourOf(now)
	for _, hh := range []int64{h, h - 1} {
		b := r.hours[authID][hh]
		if b == nil {
			continue
		}
		total := 0
		for _, c := range b.Requests {
			total += c
		}
		if hh == h {
			n += total
		} else {
			// Weight the previous hour by how much of it lies within the last 60 minutes.
			frac := 1 - float64(now.Unix()%3600)/3600
			n += int(math.Round(float64(total) * frac))
		}
	}
	return n
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
