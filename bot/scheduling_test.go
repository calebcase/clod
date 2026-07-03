package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// newTestRegistry wires up a SchedulingRegistry in a fresh tmpdir so
// each test starts from a clean crons.json. Returns the registry and
// the dir so tests can peek at the on-disk state.
func newTestRegistry(t *testing.T) (*SchedulingRegistry, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "crons.json")
	r, err := NewSchedulingRegistry(path, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewSchedulingRegistry: %v", err)
	}
	return r, dir
}

func TestValidateSpec(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		wantErr bool
	}{
		{"5-field cron", "*/5 * * * *", false},
		{"every 30s (floor)", "@every 30s", false},
		{"every 1m", "@every 1m", false},
		{"every 10s (below floor)", "@every 10s", true},
		{"every 1s (below floor)", "@every 1s", true},
		{"hourly", "@hourly", false},
		{"daily", "@daily", false},
		{"midnight", "@midnight", false},
		{"empty", "", true},
		{"garbage", "not a cron spec", true},
		{"almost-cron", "*/5 * * *", true}, // 4 fields — invalid standard cron
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSpec(c.spec)
			if c.wantErr && err == nil {
				t.Errorf("validateSpec(%q) = nil; want error", c.spec)
			}
			if !c.wantErr && err != nil {
				t.Errorf("validateSpec(%q) = %v; want nil", c.spec, err)
			}
		})
	}
}

// TestAddPersistAndLoad exercises the crons.json roundtrip: add an
// entry, save, load with a fresh registry, and confirm the entry
// re-materializes with all fields intact.
func TestAddPersistAndLoad(t *testing.T) {
	r, dir := newTestRegistry(t)
	// Create the domain dir the entry claims to live under so
	// ValidateCWDUnderDomain has something real to compare against.
	domainDir := filepath.Join(dir, "domain")
	if err := os.MkdirAll(domainDir, 0755); err != nil {
		t.Fatal(err)
	}

	entry := &CronEntry{
		SessionKey:  "C123:456.789",
		Spec:        "@every 30s",
		Command:     "echo hello",
		CWD:         domainDir,
		Description: "smoke test",
	}
	if err := r.Add(entry); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if entry.ID == "" || !strings.HasPrefix(entry.ID, "cron_") {
		t.Fatalf("Add did not populate ID; got %q", entry.ID)
	}
	if entry.LastOutputPath == "" {
		t.Fatalf("Add did not populate LastOutputPath")
	}

	if err := r.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Fresh registry, same path — simulates bot restart.
	r2, err := NewSchedulingRegistry(filepath.Join(dir, "crons.json"), zerolog.Nop())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := r2.Get(entry.ID)
	if got == nil {
		t.Fatalf("Get(%q) = nil after reload", entry.ID)
	}
	if got.Spec != entry.Spec || got.Command != entry.Command || got.Description != entry.Description {
		t.Errorf("reloaded entry differs: got %+v", got)
	}
	if got.SessionKey != entry.SessionKey {
		t.Errorf("reloaded session_key = %q; want %q", got.SessionKey, entry.SessionKey)
	}
	if got.CWD != entry.CWD {
		t.Errorf("reloaded cwd = %q; want %q", got.CWD, entry.CWD)
	}
}

// TestCronCapExceeded verifies MaxCronsPerSession is enforced.
func TestCronCapExceeded(t *testing.T) {
	r, dir := newTestRegistry(t)
	domainDir := filepath.Join(dir, "domain")
	if err := os.MkdirAll(domainDir, 0755); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < MaxCronsPerSession; i++ {
		if err := r.Add(&CronEntry{
			SessionKey:  "S1",
			Spec:        "@every 1m",
			Command:     "true",
			CWD:         domainDir,
			Description: "fill",
		}); err != nil {
			t.Fatalf("add #%d: %v", i, err)
		}
	}
	if err := r.Add(&CronEntry{
		SessionKey:  "S1",
		Spec:        "@every 1m",
		Command:     "true",
		CWD:         domainDir,
		Description: "over cap",
	}); err != ErrCronCapExceeded {
		t.Errorf("Add past cap = %v; want ErrCronCapExceeded", err)
	}
	// Different session shouldn't be affected.
	if err := r.Add(&CronEntry{
		SessionKey:  "S2",
		Spec:        "@every 1m",
		Command:     "true",
		CWD:         domainDir,
		Description: "other session",
	}); err != nil {
		t.Errorf("Add for different session = %v; want nil", err)
	}
}

// TestDeleteSessionClearsAll checks that DeleteSession removes every
// entry for one sessionKey without touching others.
func TestDeleteSessionClearsAll(t *testing.T) {
	r, dir := newTestRegistry(t)
	domainDir := filepath.Join(dir, "domain")
	if err := os.MkdirAll(domainDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, sk := range []string{"S1", "S1", "S1", "S2"} {
		if err := r.Add(&CronEntry{
			SessionKey:  sk,
			Spec:        "@every 1m",
			Command:     "true",
			CWD:         domainDir,
			Description: sk,
		}); err != nil {
			t.Fatal(err)
		}
	}
	removed := r.DeleteSession("S1")
	if len(removed) != 3 {
		t.Errorf("DeleteSession removed %d; want 3", len(removed))
	}
	if got := r.ListForSession("S1"); len(got) != 0 {
		t.Errorf("S1 still has %d entries after DeleteSession", len(got))
	}
	if got := r.ListForSession("S2"); len(got) != 1 {
		t.Errorf("S2 has %d entries; want 1", len(got))
	}
}

// TestValidateCWDUnderDomain covers containment checks.
func TestValidateCWDUnderDomain(t *testing.T) {
	base := t.TempDir()
	domain := filepath.Join(base, "domain")
	inside := filepath.Join(domain, "sub")
	if err := os.MkdirAll(inside, 0755); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(base, "sibling")
	if err := os.MkdirAll(sibling, 0755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		candidate string
		wantErr   bool
	}{
		{"empty defaults to domain", "", false},
		{"domain root", domain, false},
		{"subdir", inside, false},
		{"relative resolves under domain", "sub", false},
		{"sibling (escape via absolute)", sibling, true},
		{"parent traversal", filepath.Join(domain, "..", "sibling"), true},
		{"prefix-only match not sufficient", domain + "-evil", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The `prefix-only match not sufficient` case needs the
			// evil-suffix dir to actually exist so EvalSymlinks
			// doesn't fall back to Clean.
			if c.candidate == domain+"-evil" {
				if err := os.MkdirAll(domain+"-evil", 0755); err != nil {
					t.Fatal(err)
				}
			}
			_, err := ValidateCWDUnderDomain(c.candidate, domain)
			if c.wantErr && err == nil {
				t.Errorf("ValidateCWDUnderDomain(%q, %q) = nil; want error", c.candidate, domain)
			}
			if !c.wantErr && err != nil {
				t.Errorf("ValidateCWDUnderDomain(%q, %q) = %v; want nil", c.candidate, domain, err)
			}
		})
	}
}

// TestAddInvalidSpecRolledBack confirms a bad spec doesn't leave a
// half-persisted entry in the registry.
func TestAddInvalidSpecRolledBack(t *testing.T) {
	r, dir := newTestRegistry(t)
	domainDir := filepath.Join(dir, "domain")
	if err := os.MkdirAll(domainDir, 0755); err != nil {
		t.Fatal(err)
	}
	err := r.Add(&CronEntry{
		SessionKey:  "S1",
		Spec:        "@every 1s", // below floor
		Command:     "true",
		CWD:         domainDir,
		Description: "bad",
	})
	if err == nil {
		t.Fatal("Add with sub-floor spec unexpectedly succeeded")
	}
	if got := r.ListForSession("S1"); len(got) != 0 {
		t.Errorf("rollback failed; S1 has %d entries", len(got))
	}
}

// TestSchedulingRunOnceWritesLog is a tick-of-real-work smoke test.
// Starts the engine, adds a "@every 30s" cron whose command is
// something we can inspect, calls runOnce directly (bypassing the
// wait for the next tick), and asserts the log file exists with the
// expected content and the entry's LastFireAt has been set.
func TestSchedulingRunOnceWritesLog(t *testing.T) {
	r, dir := newTestRegistry(t)
	r.Start()
	defer r.Stop()

	domainDir := filepath.Join(dir, "domain")
	if err := os.MkdirAll(domainDir, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dir, "sentinel.txt")

	entry := &CronEntry{
		SessionKey:  "S1",
		Spec:        "@every 1m", // engine spec — we call runOnce manually below
		Command:     "echo tick > " + sentinel,
		CWD:         domainDir,
		Description: "tick test",
	}
	if err := r.Add(entry); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Manually run one tick. Fires immediately instead of waiting a
	// minute for the engine's own schedule.
	r.runOnce(entry.ID)

	got := r.Get(entry.ID)
	if got == nil {
		t.Fatalf("entry disappeared after runOnce")
	}
	if got.LastFireAt.IsZero() {
		t.Errorf("LastFireAt not set after runOnce")
	}
	if got.LastExitCode != 0 {
		t.Errorf("exit code = %d; want 0", got.LastExitCode)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("sentinel file not created: %v", err)
	}
	data, err := os.ReadFile(got.LastOutputPath)
	if err != nil {
		t.Fatalf("read log %q: %v", got.LastOutputPath, err)
	}
	if !strings.Contains(string(data), "===== ") {
		t.Errorf("log missing header line; got:\n%s", string(data))
	}

	// Give the engine's Next-fire projection a moment to populate.
	// This is best-effort — we don't want to fail on flake.
	time.Sleep(50 * time.Millisecond)
}

// TestOnDiskShapeStable pins the top-level file shape so accidental
// schema drift shows up as a test failure rather than a silent
// migration surprise.
func TestOnDiskShapeStable(t *testing.T) {
	r, dir := newTestRegistry(t)
	domainDir := filepath.Join(dir, "domain")
	if err := os.MkdirAll(domainDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(&CronEntry{
		SessionKey:  "S1",
		Spec:        "@every 1m",
		Command:     "true",
		CWD:         domainDir,
		Description: "shape check",
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "crons.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Crons []map[string]any `json:"crons"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("crons.json parse: %v\n%s", err, string(raw))
	}
	if len(f.Crons) != 1 {
		t.Fatalf("expected 1 cron on disk, got %d", len(f.Crons))
	}
	// Required-field spot check — the specific set is documented in
	// mcp-shim.md §4.3.
	for _, k := range []string{"id", "session_key", "spec", "mode", "command", "cwd", "description", "created_at"} {
		if _, ok := f.Crons[0][k]; !ok {
			t.Errorf("crons.json entry missing required key %q", k)
		}
	}
}
