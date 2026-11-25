package router

import (
	"fmt"
	"math"
	"time"
)

// LendAny as a lend_limits target stands for the clients of every other
// bucket, and for clients with no bucket.
const LendAny = "any"

// lendLimit is one bucket's cap on what it lends to one target, for one
// provider. The bucket's accounts of that provider count together: their
// weekly capacities add up, and so does what they lent.
type lendLimit struct {
	Bucket, Target string
	Points         []ReservePoint
	// CapacityUSD sums the weekly capacity of the bucket's accounts whose
	// weekly window is known; accounts without one neither add capacity nor
	// lend.
	CapacityUSD float64
	// AllowedUSD is what the bucket may lend this week: each account's
	// capacity times the limit's share at that account's time left.
	AllowedUSD float64
	// LentUSD is what the target's clients used on the bucket's accounts in
	// each account's current weekly window.
	LentUSD float64
}

func (l *lendLimit) open() bool { return l.LentUSD < l.AllowedUSD }

// covers reports whether a borrower whose client config lists these buckets
// is one of the limit's target clients.
func (l *lendLimit) covers(borrowerBuckets []string) bool {
	return l.Target == LendAny || contains(borrowerBuckets, l.Target)
}

func (l *lendLimit) targetText() string {
	if l.Target == LendAny {
		return "every other bucket"
	}
	return "bucket " + l.Target
}

// lendLimits builds this provider's bucket lending limits once per context.
func (pc *planContext) lendLimits() []*lendLimit {
	if pc.limitsBuilt {
		return pc.limits
	}
	pc.limitsBuilt = true
	for _, bucket := range sortedKeys(pc.cfg.LendLimits) {
		var members []*eval
		for _, id := range pc.order {
			if e := pc.evals[id]; contains(e.Buckets, bucket) {
				members = append(members, e)
			}
		}
		if len(members) == 0 {
			continue
		}
		for _, target := range sortedKeys(pc.cfg.LendLimits[bucket]) {
			l := &lendLimit{Bucket: bucket, Target: target, Points: pc.cfg.LendLimits[bucket][target]}
			for _, e := range members {
				if e.NoData || e.WeeklyReset.IsZero() || e.Capacity.USD <= 0 {
					continue
				}
				left := time.Duration(e.hoursToReset(pc.now) * float64(time.Hour))
				l.CapacityUSD += e.Capacity.USD
				l.AllowedUSD += e.Capacity.USD * reserveAt(l.Points, left)
				l.LentUSD += pc.lentFrom(e, l)
			}
			pc.limits = append(pc.limits, l)
		}
	}
	return pc.limits
}

// ownedBy reports whether one of the client's buckets lists the account.
func (pc *planContext) ownedBy(e *eval, cc ClientConfig) bool {
	for _, b := range cc.Buckets {
		if contains(e.Buckets, b) {
			return true
		}
	}
	return false
}

// lentFrom sums what the limit's target clients used on one account in its
// current weekly window. Usage outside CPA and the account's own clients do
// not count.
func (pc *planContext) lentFrom(e *eval, l *lendLimit) float64 {
	r := pc.r
	length := 7 * 24 * time.Hour
	if w, ok := weeklyWindow(r.accounts[e.ID].Snapshot); ok && w.Length > 0 {
		length = w.Length
	}
	start := e.WeeklyReset.Add(-length)
	home := map[string]bool{}
	for _, c := range e.Home {
		home[c] = true
	}
	borrower := map[string]bool{}
	usd := 0.0
	for h, b := range r.hours[e.ID] {
		// An hour belongs to the window its readings named; an hour without
		// readings, to the window its end falls in.
		if b.Cycle.IsZero() {
			if !time.Unix((h+1)*3600, 0).After(start) {
				continue
			}
		} else if !sameReset(b.Cycle, e.WeeklyReset) {
			continue
		}
		for _, c := range hourClients(b) {
			is, seen := borrower[c]
			if !seen {
				cc := r.clientConfigLocked(c)
				is = c != externalClient && !pc.ownedBy(e, cc) && l.covers(cc.Buckets)
				borrower[c] = is
			}
			if is {
				usd += b.billed(c, home, e.Capacity.USD)
			}
		}
	}
	return usd
}

// hourClients lists the clients with traffic in an hour.
func hourClients(b *hourBucket) []string {
	seen := map[string]bool{}
	var out []string
	add := func(c string) {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for c := range b.USDHigh {
		add(c)
	}
	for c := range b.Tokens {
		add(c)
	}
	for c := range b.Requests {
		add(c)
	}
	return out
}

// lendCheck applies the bucket lending limits to client borrowing e. It
// returns why the limits stop the loan ("" when they do not) and, when some
// limit applies, the dollars left under the tightest one. Accounts the client
// owns are never limited.
func (pc *planContext) lendCheck(e *eval, client string) (block string, room float64, limited bool) {
	cc := pc.r.clientConfigLocked(client)
	if pc.ownedBy(e, cc) {
		return "", 0, false
	}
	room = math.Inf(1)
	for _, l := range pc.lendLimits() {
		if !contains(e.Buckets, l.Bucket) || !l.covers(cc.Buckets) {
			continue
		}
		if e.NoData || e.WeeklyReset.IsZero() {
			return fmt.Sprintf("bucket %s limits lending to %s and this account's weekly window is not known yet", l.Bucket, l.targetText()), 0, true
		}
		if !l.open() {
			return fmt.Sprintf("bucket %s has lent $%.2f to %s, its limit is $%.2f for now", l.Bucket, l.LentUSD, l.targetText(), l.AllowedUSD), 0, true
		}
		limited = true
		room = math.Min(room, l.AllowedUSD-l.LentUSD)
	}
	return "", room, limited
}

// lendBlock is lendCheck's reason alone.
func (pc *planContext) lendBlock(e *eval, client string) string {
	block, _, _ := pc.lendCheck(e, client)
	return block
}

// LendLimitView is one bucket lending limit, for one provider.
type LendLimitView struct {
	Bucket   string `json:"bucket"`
	Target   string `json:"target"` // a bucket, or "any"
	Provider string `json:"provider"`
	// Share is the limit as configured: a number, or time left → share.
	Share any `json:"share"`
	// ShareNow is AllowedUSD over CapacityUSD: the limit's share across the
	// bucket's accounts at their current time left.
	ShareNow    float64 `json:"share_now"`
	CapacityUSD float64 `json:"capacity_usd"`
	AllowedUSD  float64 `json:"allowed_usd"`
	LentUSD     float64 `json:"lent_usd"`
	Open        bool    `json:"open"`
}

func (pc *planContext) lendLimitViews() []LendLimitView {
	var out []LendLimitView
	for _, l := range pc.lendLimits() {
		v := LendLimitView{
			Bucket: l.Bucket, Target: l.Target, Provider: pc.provider, Share: curveDoc(l.Points),
			CapacityUSD: l.CapacityUSD, AllowedUSD: l.AllowedUSD, LentUSD: l.LentUSD, Open: l.open(),
		}
		if l.CapacityUSD > 0 {
			v.ShareNow = l.AllowedUSD / l.CapacityUSD
		}
		out = append(out, v)
	}
	return out
}
