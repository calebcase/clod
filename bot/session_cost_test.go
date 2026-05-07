package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
)

// newTestStore returns a SessionStore backed by a temp file. The
// temp file is left empty (Load is a no-op for missing files), so
// the migration sidecar gets created the first time Load runs.
func newTestStore(t *testing.T) (*SessionStore, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	store, err := NewSessionStore(path, 0, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	return store, path
}

// approx asserts |a - b| < 1e-9, the precision we care about for
// summed dollars-and-cents.
func approx(t *testing.T, got, want float64, msg string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s: got %.10f, want %.10f", msg, got, want)
	}
}

// TestAddStats_FreshSession covers the fresh-session path: each
// call's claude-cumulative is treated as the new high water mark,
// and the delta against the prior observation is what gets credited.
// In particular the very first call credits the full cumulative
// (since LastClaudeCostUSD starts at 0).
func TestAddStats_FreshSession(t *testing.T) {
	store, _ := newTestStore(t)

	cost, turns := store.AddStats("C1", "T1", 0.05, 1)
	approx(t, cost, 0.05, "first result credits full cumulative")
	if turns != 1 {
		t.Fatalf("first turns: got %d, want 1", turns)
	}

	// Second result: claude reports a cumulative-since-process-start
	// of 0.18, meaning this turn cost 0.13.
	cost, turns = store.AddStats("C1", "T1", 0.18, 3)
	approx(t, cost, 0.18, "running cumulative after second result")
	if turns != 4 {
		t.Fatalf("turns after second result: got %d, want 4", turns)
	}

	cost, turns = store.AddStats("C1", "T1", 0.42, 2)
	approx(t, cost, 0.42, "running cumulative after third result")
	if turns != 6 {
		t.Fatalf("turns after third result: got %d, want 6", turns)
	}
}

// TestAddStats_ResumeContinuity simulates a process boundary: claude
// process 1 reaches a cumulative of 0.42, claude --resume starts
// process 2, which restores its internal totalCostUSD from saved
// state. The first result from process 2 reports a cumulative >=
// 0.42 — only the delta beyond the prior high water mark should
// credit. This is the case the pre-fix code got wrong: it would
// have added the full 0.50 as if it were a per-call cost, on top of
// the prior 0.42.
func TestAddStats_ResumeContinuity(t *testing.T) {
	store, _ := newTestStore(t)

	// Process 1: two results.
	store.AddStats("C1", "T1", 0.05, 1)
	store.AddStats("C1", "T1", 0.42, 4)

	// Process 2 (resumed): claude restores totalCostUSD = 0.42, runs
	// one turn that costs 0.08, emits result with total_cost_usd=0.50.
	cost, turns := store.AddStats("C1", "T1", 0.50, 2)
	approx(t, cost, 0.50, "post-resume cumulative is delta-credited, not added wholesale")
	if turns != 7 {
		t.Fatalf("turns: got %d, want 7", turns)
	}
}

// TestAddStats_BaselinePending_FromMigration covers the migration
// handoff. After the one-time reset, CumulativeCostUSD=0 and
// CostBaselinePending=true. The next observed claude cumulative
// (which carries whatever the process was at when the bot restarted)
// should NOT credit anything — we don't know the baseline, so we
// just plant a stake at this value and let future deltas accumulate
// from here.
func TestAddStats_BaselinePending_FromMigration(t *testing.T) {
	store, _ := newTestStore(t)

	// Simulate a session that's been through migration v1.
	store.Set(&SessionMapping{
		ChannelID:           "C1",
		ThreadTS:            "T1",
		CostBaselinePending: true,
		CumulativeCostUSD:   0,
		LastClaudeCostUSD:   0,
	})

	// First post-migration result: claude is mid-process at $15.00.
	// We don't know how much of that was already accounted for under
	// the buggy regime, so we credit nothing and just plant the
	// baseline.
	cost, _ := store.AddStats("C1", "T1", 15.00, 5)
	approx(t, cost, 0, "migration baseline result must not credit cost")

	store.mu.RLock()
	if store.sessions[key("C1", "T1")].CostBaselinePending {
		t.Fatalf("CostBaselinePending should be cleared after first post-migration result")
	}
	if got := store.sessions[key("C1", "T1")].LastClaudeCostUSD; got != 15.00 {
		t.Fatalf("LastClaudeCostUSD: got %.2f, want 15.00", got)
	}
	store.mu.RUnlock()

	// Subsequent result: claude cumulative is now $15.20 — this
	// turn's $0.20 should credit normally.
	cost, _ = store.AddStats("C1", "T1", 15.20, 1)
	approx(t, cost, 0.20, "post-baseline result credits the delta")
}

// TestAddStats_DecreasingCumulative_Defensive guards the defensive
// branch: if claude ever reports a cumulative lower than what we
// last observed (shouldn't happen — claude's totalCostUSD is
// monotonic within a process and resume restores it — but might if
// session-state files get rolled back or stats get associated with
// the wrong thread), don't credit a negative delta. Just reset the
// baseline.
func TestAddStats_DecreasingCumulative_Defensive(t *testing.T) {
	store, _ := newTestStore(t)

	store.AddStats("C1", "T1", 0.42, 4)

	cost, _ := store.AddStats("C1", "T1", 0.10, 1)
	approx(t, cost, 0.42, "cumulative should not decrease on a backwards observation")

	// And the next forward observation should delta off the new
	// baseline (0.10), not the prior high-water 0.42.
	cost, _ = store.AddStats("C1", "T1", 0.18, 1)
	approx(t, cost, 0.50, "forward observation credits delta off the reset baseline")
}

// TestCostMigrationV1_RunsOnce verifies the one-time migration: a
// sessions.json with inflated CumulativeCostUSD values gets zeroed
// on the first Load, the marker sidecar gets written, and a second
// Load is a no-op (no further mutation).
func TestCostMigrationV1_RunsOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	// Pre-seed sessions.json with two sessions, both inflated to
	// values matching the bug's quadratic-growth signature.
	preSessions := []*SessionMapping{
		{
			ChannelID:         "C1",
			ThreadTS:          "T1",
			CumulativeCostUSD: 60380.91,
			CumulativeTurns:   1486,
		},
		{
			ChannelID:         "C2",
			ThreadTS:          "T2",
			CumulativeCostUSD: 24.71,
			CumulativeTurns:   278,
		},
	}
	data, err := json.Marshal(preSessions)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := NewSessionStore(path, 0, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}

	// Migration ran: cost zeroed, baseline pending, marker present.
	store.mu.RLock()
	for _, s := range store.sessions {
		if s.CumulativeCostUSD != 0 {
			t.Errorf("session %s: CumulativeCostUSD = %.2f, want 0", s.ThreadTS, s.CumulativeCostUSD)
		}
		if !s.CostBaselinePending {
			t.Errorf("session %s: CostBaselinePending should be true", s.ThreadTS)
		}
		// Turns are not affected (claude reports them per-call, so
		// the bot's pre-fix `+= turns` was already correct).
		if s.ThreadTS == "T1" && s.CumulativeTurns != 1486 {
			t.Errorf("T1 turns: got %d, want 1486 (turns shouldn't be reset)", s.CumulativeTurns)
		}
	}
	store.mu.RUnlock()

	marker := path + ".cost-migration-v1.done"
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("migration marker not written: %v", err)
	}

	// Second Load: simulate by mutating in-memory then reloading from
	// disk. Re-construct a fresh store pointing at the same path.
	// The marker is present, so migration must not rerun. We can
	// detect this by setting a non-zero CumulativeCostUSD before
	// reloading and confirming it survives.
	store.mu.Lock()
	store.sessions[key("C1", "T1")].CumulativeCostUSD = 0.42
	store.sessions[key("C1", "T1")].CostBaselinePending = false
	store.mu.Unlock()
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	store2, err := NewSessionStore(path, 0, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	store2.mu.RLock()
	got := store2.sessions[key("C1", "T1")].CumulativeCostUSD
	pending := store2.sessions[key("C1", "T1")].CostBaselinePending
	store2.mu.RUnlock()
	if got != 0.42 {
		t.Errorf("second Load mutated CumulativeCostUSD: got %.2f, want 0.42", got)
	}
	if pending {
		t.Errorf("second Load re-set CostBaselinePending; migration must be one-shot")
	}
}
