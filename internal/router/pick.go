package router

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const maxDecisions = 200

// Reject codes returned to the host.
const (
	RejectExhausted = "quota_exhausted"
	RejectWait      = "rate_limited"
)

// Decision records one routing decision with the full ranking.
type Decision struct {
	Time     time.Time `json:"time"`
	Client   string    `json:"client"`
	Provider string    `json:"provider"`
	Model    string    `json:"model,omitempty"`
	Session  string    `json:"session"` // "pinned", "new" or "none"
	Outcome  string    `json:"outcome"` // selected, rejected, unhandled
	Account  string    `json:"account,omitempty"`
	Buckets  []string  `json:"buckets,omitempty"`
	Role     string    `json:"role,omitempty"`
	Reason   string    `json:"reason"`
	Policy   string    `json:"policy,omitempty"`
	Ranking  []string  `json:"ranking,omitempty"`
	Excluded []string  `json:"excluded,omitempty"`
}

// sessionMetadataKeys are host-populated session identities, strongest first.
var sessionMetadataKeys = []string{
	"canonical_session_id",
	"execution_session_id",
	"lcp_affinity_session_id",
	"derived_session_id",
}

var sessionHeaders = []string{
	"X-Claude-Code-Session-Id", "Session-Id", "Session_id", "X-Session-Id",
	"X-Session-Affinity", "Thread-Id", "X-Thread-Id", "X-Conversation-Id",
}

// sessionID derives an opaque session identity; raw IDs are hashed at once.
func sessionID(headers map[string][]string, metadata map[string]any) string {
	raw := ""
	for _, k := range sessionMetadataKeys {
		if v, ok := metadata[k].(string); ok && strings.TrimSpace(v) != "" {
			raw = "m:" + strings.TrimSpace(v)
			break
		}
	}
	if raw == "" {
		for _, name := range sessionHeaders {
			if v := headerFirst(headers, name); v != "" {
				raw = "h:" + v
				break
			}
		}
	}
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:12])
}

// Pick chooses a credential for one request.
func (r *Router) Pick(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	now := r.now()
	r.mu.Lock()
	resp, d := r.pickLocked(req, now)
	r.decisions = append(r.decisions, d)
	if len(r.decisions) > maxDecisions {
		r.decisions = r.decisions[len(r.decisions)-maxDecisions:]
	}
	r.metrics.pick(d)
	logIt := r.cfg.LogDecisions
	r.mu.Unlock()
	if logIt {
		r.logDecision(d)
	}
	return resp
}

func (r *Router) pickLocked(req pluginapi.SchedulerPickRequest, now time.Time) (pluginapi.SchedulerPickResponse, Decision) {
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	scope, _ := req.Options.Metadata["caller_scope"].(string)
	client := r.clientForScopeLocked(scope)
	d := Decision{Time: now, Client: client, Provider: provider, Model: req.Model, Session: "none"}

	if len(req.Candidates) == 0 {
		d.Outcome, d.Reason = "unhandled", "no candidates"
		return pluginapi.SchedulerPickResponse{}, d
	}
	cand := map[string]pluginapi.SchedulerAuthCandidate{}
	for _, c := range req.Candidates {
		cand[c.ID] = c
		st := r.accountLocked(c.ID, c.Provider)
		if k := c.Attributes["auth_kind"]; k != "" {
			st.Info.AuthKind = k
		}
		if provider == "" {
			provider = st.Info.Provider
		}
	}
	d.Provider = provider

	pc := r.newPlanContextLocked(provider, now)
	order, policy, policySrc := pc.plan(client, r.cfg.MinSpare)
	d.Policy = string(policy) + " (" + policySrc + ")"
	for _, p := range order {
		mark := ""
		if _, ok := cand[p.ID]; !ok {
			mark = " [not offered by CPA]"
		}
		d.Ranking = append(d.Ranking, fmt.Sprintf("%s %s: %s%s", p.Role, pc.evals[p.ID].Name, p.Why, mark))
	}
	for _, id := range pc.order {
		if e := pc.evals[id]; !e.Usable {
			d.Excluded = append(d.Excluded, e.Name+": "+e.Block)
		}
	}

	sess := sessionID(req.Options.Headers, req.Options.Metadata)
	bindKey := ""
	if scope != "" || sess != "" {
		bindKey = scope + "|" + sess
	}
	if sess != "" {
		d.Session = "new"
	}
	choose := func(id, role, why string) (pluginapi.SchedulerPickResponse, Decision) {
		d.Outcome, d.Account, d.Role, d.Reason = "selected", pc.evals[id].Name, role, why
		d.Buckets = pc.evals[id].Buckets
		if bindKey != "" {
			r.bindings[bindKey] = binding{AuthID: id, Expires: now.Add(r.cfg.SessionTTL)}
		}
		if role == RoleProbe {
			r.accounts[id].ProbedAt = now
		}
		return pluginapi.SchedulerPickResponse{AuthID: id, Handled: true}, d
	}

	// A pinned session stays while its account is still in the client's plan
	// as own or spare. A last-resort pin is kept only while nothing better is
	// offered, so a borrowed session leaves the lender once its spare is gone.
	// Borrowed accounts keep sessions until their spare drops to zero.
	betterThanLastResort := false
	for _, p := range order {
		if _, ok := cand[p.ID]; ok && p.Role != RoleLastResort {
			betterThanLastResort = true
			break
		}
	}
	if b, ok := r.bindings[bindKey]; ok && bindKey != "" && now.Before(b.Expires) {
		if _, offered := cand[b.AuthID]; offered {
			if sess != "" {
				d.Session = "pinned"
			}
			// Spare hysteresis first: a borrowed account keeps its sessions while
			// spare > 0, even if it dropped below min_spare for new ones.
			if e := pc.evals[b.AuthID]; e.Usable && !e.NoData && e.Spare > 0 && !contains(r.ownIDsLocked(pc, client), b.AuthID) {
				return choose(b.AuthID, RoleSpare, fmt.Sprintf("pinned: spare %.2f still above 0", e.Spare))
			}
			for _, p := range order {
				if p.ID == b.AuthID && (p.Role != RoleLastResort || !betterThanLastResort) {
					return choose(p.ID, p.Role, "pinned: "+p.Why)
				}
			}
			if sess != "" {
				d.Session = "moved"
			}
		}
	}

	for _, p := range order {
		if _, ok := cand[p.ID]; ok {
			return choose(p.ID, p.Role, p.Why)
		}
	}

	// Everything offered is blocked in our view (exhausted, cooling down, or
	// only serving from paid extra usage). Never let CPA pick one of those.
	var blocked []string
	for id := range cand {
		if e := pc.evals[id]; e != nil && !e.Usable {
			blocked = append(blocked, id)
		}
	}
	sort.Strings(blocked)
	if len(blocked) == len(cand) || policy != PolicyAny {
		return r.rejectLocked(d, policy, client)
	}
	// Policy any, and some offered credential is unknown to us: let CPA
	// fill-first among the ones not known to be blocked.
	d.Outcome, d.Reason = "delegated", "nothing in plan offered; CPA fill-first among unblocked"
	return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst, ExcludeCandidates: blocked}, d
}

// ownIDsLocked lists a client's own account IDs for this provider.
func (r *Router) ownIDsLocked(pc *planContext, client string) []string {
	var out []string
	for _, e := range pc.ownList(r.clientConfigLocked(client)) {
		out = append(out, e.ID)
	}
	return out
}

func (r *Router) rejectLocked(d Decision, policy Policy, client string) (pluginapi.SchedulerPickResponse, Decision) {
	if policy == PolicyWait {
		d.Outcome, d.Reason = "rejected", "no usable account; policy wait"
		return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: RejectWait,
			RejectReason: "fefo-router: client " + client + " is waiting for quota to refill"}, d
	}
	d.Outcome, d.Reason = "rejected", "no usable account; policy "+string(policy)
	return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: RejectExhausted,
		RejectReason: "fefo-router: client " + client + " has no usable account (policy " + string(policy) + ")"}, d
}

func (r *Router) logDecision(d Decision) {
	if r.log == nil {
		return
	}
	next := ""
	if len(d.Ranking) > 1 {
		next = d.Ranking[1]
	}
	msg := fmt.Sprintf("fefo %s client=%s %s → %s [%s] %s", d.Outcome, d.Client, d.Provider, d.Account, d.Role, d.Reason)
	r.log("info", msg, map[string]any{
		"client": d.Client, "provider": d.Provider, "model": d.Model, "session": d.Session,
		"outcome": d.Outcome, "account": d.Account, "role": d.Role, "reason": d.Reason,
		"policy": d.Policy, "runner_up": next,
	})
}

// Decisions returns recent decisions, oldest first.
func (r *Router) Decisions() []Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Decision(nil), r.decisions...)
}
