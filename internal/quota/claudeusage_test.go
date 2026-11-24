package quota

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var usageObserved = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func parseFixture(t *testing.T, name string) UsageReport {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := ParseClaudeUsage(raw, usageObserved)
	if err != nil {
		t.Fatalf("ParseClaudeUsage: %v", err)
	}
	return rep
}

func windowByName(t *testing.T, ws []Window, name string) Window {
	t.Helper()
	for _, w := range ws {
		if w.Name == name {
			return w
		}
	}
	t.Fatalf("no window %q in %+v", name, ws)
	return Window{}
}

func TestClaudeUsageProExhaustedHasNoFableAllowance(t *testing.T) {
	rep := parseFixture(t, "claude_usage_pro_exhausted.json")
	if !rep.HasReport {
		t.Fatal("HasReport = false")
	}
	if _, ok := rep.Scoped("Fable"); ok {
		t.Fatal("Pro plan must report no Fable allowance")
	}
	week := windowByName(t, rep.Windows, "7d")
	if week.Remaining != 0 {
		t.Fatalf("7d remaining = %v, want 0", week.Remaining)
	}
	if want := time.Date(2026, 11, 25, 22, 59, 59, 756594000, time.UTC); !week.ResetAt.Equal(want) {
		t.Fatalf("7d reset = %v, want %v", week.ResetAt, want)
	}
	session := windowByName(t, rep.Windows, "5h")
	if session.Remaining != 1 || !session.ResetAt.IsZero() {
		t.Fatalf("idle session = %+v, want full with unknown reset", session)
	}
	if !rep.ExtraUsageHasState || rep.ExtraUsageEnabled || rep.SpendLimitReached {
		t.Fatalf("extra usage = %+v", rep)
	}
}

func TestClaudeUsageUltraScopedAllowanceAndExtraUsage(t *testing.T) {
	rep := parseFixture(t, "claude_usage_ultra_extra.json")
	fable, ok := rep.Scoped("fable")
	if !ok {
		t.Fatal("no fable window")
	}
	if fable.Name != "7d:fable" || fable.Remaining != 1 || fable.Allowance != 0.5 {
		t.Fatalf("fable = %+v", fable)
	}
	if want := time.Date(2026, 12, 1, 7, 0, 0, 0, time.UTC); !fable.ResetAt.Equal(want) {
		t.Fatalf("fable reset = %v", fable.ResetAt)
	}
	if got := windowByName(t, rep.Windows, "7d").Remaining; got != 0.93 {
		t.Fatalf("7d remaining = %v, want 0.93", got)
	}
	if len(rep.Windows) != 1 {
		t.Fatalf("windows = %+v, want only 7d", rep.Windows)
	}
	if !rep.ExtraUsageEnabled || rep.SpendLimitReached {
		t.Fatalf("extra usage = %+v", rep)
	}
}

func TestClaudeUsageTeamPremiumMatchesShareByLimitIndex(t *testing.T) {
	rep := parseFixture(t, "claude_usage_team_premium.json")
	fable, ok := rep.Scoped("FABLE")
	if !ok {
		t.Fatal("no fable window")
	}
	if fable.Allowance != 0.5 || fable.Remaining != 0.93 {
		t.Fatalf("fable = %+v", fable)
	}
	if got := windowByName(t, rep.Windows, "5h").Remaining; got != 0 {
		t.Fatalf("5h remaining = %v, want 0", got)
	}
	if got := windowByName(t, rep.Windows, "7d").Remaining; got != 0.24 {
		t.Fatalf("7d remaining = %v, want 0.24", got)
	}
	if rep.ExtraUsageEnabled {
		t.Fatal("extra usage must be off")
	}
}

func TestClaudeUsageSnapshotMergesIntoHeaderSnapshot(t *testing.T) {
	rep := parseFixture(t, "claude_usage_ultra_extra.json")
	snap := rep.Snapshot()
	if snap.Provider != "claude" || !snap.ExtraUsage || snap.ExtraUsageObservedAt != usageObserved {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Name != "7d" {
		t.Fatalf("snapshot windows = %+v", snap.Windows)
	}
	headers := Snapshot{Provider: "claude", ObservedAt: usageObserved.Add(-time.Minute),
		Windows: []Window{{Name: "5h", Remaining: 0.4, ObservedAt: usageObserved.Add(-time.Minute)}}}
	merged := headers.Merge(snap)
	if len(merged.Windows) != 2 || !merged.ExtraUsage {
		t.Fatalf("merged = %+v", merged)
	}
}

func TestClaudeUsageLegacyFallbackOnlyWithoutLimits(t *testing.T) {
	rep, err := ParseClaudeUsage([]byte(`{"five_hour":{"utilization":25,"resets_at":"2026-11-24T14:00:00+00:00"},"seven_day":{"utilization":50,"resets_at":null}}`), usageObserved)
	if err != nil {
		t.Fatal(err)
	}
	if got := windowByName(t, rep.Windows, "5h").Remaining; got != 0.75 {
		t.Fatalf("5h remaining = %v", got)
	}
	if got := windowByName(t, rep.Windows, "7d"); got.Remaining != 0.5 || !got.ResetAt.IsZero() {
		t.Fatalf("7d = %+v", got)
	}
	if rep.ExtraUsageHasState {
		t.Fatal("no extra_usage object, no state")
	}

	rep, err = ParseClaudeUsage([]byte(`{"limits":[],"seven_day":{"utilization":50}}`), usageObserved)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Windows) != 0 || !rep.HasReport {
		t.Fatalf("empty limits must ignore legacy keys: %+v", rep)
	}
}

func TestClaudeUsageMalformed(t *testing.T) {
	for _, raw := range []string{`{"limits":`, `[]`, `{"limits":{"a":1}}`} {
		if _, err := ParseClaudeUsage([]byte(raw), usageObserved); err == nil {
			t.Fatalf("expected error for %q", raw)
		}
	}
}

func TestUnpolledReportHasNoScopedWindow(t *testing.T) {
	var rep UsageReport
	if rep.HasReport {
		t.Fatal("zero report must not claim to be polled")
	}
	if _, ok := rep.Scoped("fable"); ok {
		t.Fatal("zero report has no windows")
	}
}
