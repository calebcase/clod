package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/calebcase/oops"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"
)

// SchedulingRegistry owns the bot-side scheduling state: recurring
// `cron_*` jobs claude (or any MCP-speaking agent) registers via the
// scheduling MCP shim. The registry persists to `crons.json` next to
// `sessions.json`, and drives per-entry cron tickers that fire the
// pre-declared shell command outside the docker container.
//
// See `mcp-shim.md` (repo root) for the full design. Phase 1: cron
// command-mode only — `cron_create`, `cron_list`, `cron_delete`.
// Background-job (`bg_*`) tools land in phase 2.
//
// Concurrency: internal state guarded by mu; the cron.Cron goroutine
// runs on its own, coordinated via the runner map.
type SchedulingRegistry struct {
	path   string // absolute path to crons.json
	logger zerolog.Logger

	mu      sync.RWMutex
	crons   map[string]*CronEntry     // by CronID
	engine  *cron.Cron                // nil until Start
	entries map[string]cron.EntryID   // CronID → engine handle for cancellation
}

// CronEntry is one persisted cron schedule. Kept small on purpose so
// crons.json stays legible and the disk write is cheap.
type CronEntry struct {
	ID          string `json:"id"`           // cron_<8-hex>
	SessionKey  string `json:"session_key"`  // channel:thread_ts — matches SessionStore keying
	Spec        string `json:"spec"`         // cron expression or "@every 5m"
	Mode        string `json:"mode"`         // "command" (v1 only supports this)
	Command     string `json:"command"`      // shell command; run via `bash -c`
	CWD         string `json:"cwd"`          // absolute; must resolve under the session's domain dir
	Description string `json:"description"`  // human label

	CreatedAt    time.Time `json:"created_at"`
	LastFireAt   time.Time `json:"last_fire_at,omitempty"`
	LastExitCode int       `json:"last_exit_code,omitempty"`
	LastError    string    `json:"last_error,omitempty"`   // capture of a spawn error, not stderr
	LastOutputPath string  `json:"last_output_path,omitempty"` // rolling log file

	NextFireAt time.Time `json:"next_fire_at,omitempty"` // best-effort; recomputed on load
}

// Persistence envelope — top-level object with a `crons` array keeps
// the door open for future top-level fields (e.g. bg jobs; see
// mcp-shim.md phase 2) without changing the on-disk shape.
type cronsFile struct {
	Crons []*CronEntry `json:"crons"`
}

// MaxCronsPerSession caps how many concurrent schedules a single
// thread can carry. Prevents a runaway model from filling the
// registry with `cron_create` calls. See mcp-shim.md §4.6.
const MaxCronsPerSession = 16

// MinCronInterval is the floor for `@every` and cron-expression
// intervals. Anything shorter is either a poll (should use `nohup`
// + a loop) or a bug. See mcp-shim.md §4.6.
const MinCronInterval = 30 * time.Second

// NewSchedulingRegistry loads crons.json (or starts empty if it
// doesn't exist yet) and returns a ready registry. The cron.Cron
// ticker is not started here — call Start once the containing bot
// process is ready to accept ticks.
func NewSchedulingRegistry(path string, logger zerolog.Logger) (*SchedulingRegistry, error) {
	r := &SchedulingRegistry{
		path:    path,
		crons:   make(map[string]*CronEntry),
		entries: make(map[string]cron.EntryID),
		logger:  logger.With().Str("component", "scheduling").Logger(),
	}

	if err := r.load(); err != nil && !os.IsNotExist(err) {
		return nil, oops.Trace(err)
	}

	return r, nil
}

// load reads crons.json into memory. Missing file is not an error —
// treated as an empty registry, matching SessionStore.Load semantics.
func (r *SchedulingRegistry) load() error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return err
	}
	var f cronsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return oops.Trace(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range f.Crons {
		if entry == nil || entry.ID == "" {
			continue
		}
		r.crons[entry.ID] = entry
	}
	r.logger.Info().Int("count", len(r.crons)).Str("path", r.path).Msg("loaded crons")
	return nil
}

// Save persists the current registry to crons.json via
// write-tmp-then-rename. Callers don't need to hold any lock.
func (r *SchedulingRegistry) Save() error {
	r.mu.RLock()
	entries := make([]*CronEntry, 0, len(r.crons))
	for _, e := range r.crons {
		entries = append(entries, e)
	}
	r.mu.RUnlock()

	// Stable ordering on disk so diffs during incidents are legible.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].SessionKey != entries[j].SessionKey {
			return entries[i].SessionKey < entries[j].SessionKey
		}
		return entries[i].ID < entries[j].ID
	})

	data, err := json.MarshalIndent(cronsFile{Crons: entries}, "", "  ")
	if err != nil {
		return oops.Trace(err)
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return oops.Trace(err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return oops.Trace(err)
	}
	return nil
}

// Get returns a cron entry by ID, or nil if not found.
func (r *SchedulingRegistry) Get(id string) *CronEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.crons[id]
}

// ListForSession returns crons owned by the given session_key.
// The returned slice is a fresh copy — callers can iterate without
// holding the lock.
func (r *SchedulingRegistry) ListForSession(sessionKey string) []*CronEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*CronEntry
	for _, e := range r.crons {
		if e.SessionKey == sessionKey {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// countForSessionLocked returns the number of crons owned by
// sessionKey. Caller must hold r.mu.
func (r *SchedulingRegistry) countForSessionLocked(sessionKey string) int {
	n := 0
	for _, e := range r.crons {
		if e.SessionKey == sessionKey {
			n++
		}
	}
	return n
}

// Add inserts a new cron entry. Validates the session cap and
// generates the ID. Does not start the ticker — that's the caller's
// job once phase-1 ticker plumbing lands. Returns ErrCronCapExceeded
// when the session is already at MaxCronsPerSession.
func (r *SchedulingRegistry) Add(entry *CronEntry) error {
	if entry == nil {
		return oops.Trace(fmt.Errorf("nil entry"))
	}
	if entry.SessionKey == "" {
		return oops.Trace(fmt.Errorf("session_key required"))
	}
	if entry.Spec == "" {
		return oops.Trace(fmt.Errorf("spec required"))
	}
	if entry.Command == "" {
		return oops.Trace(fmt.Errorf("command required"))
	}
	if entry.Description == "" {
		return oops.Trace(fmt.Errorf("description required"))
	}
	if entry.Mode == "" {
		entry.Mode = "command"
	}
	if entry.Mode != "command" {
		return oops.Trace(fmt.Errorf("unsupported mode %q (v1 supports 'command' only)", entry.Mode))
	}

	// Validate spec up front so we don't persist unschedulable entries.
	if err := validateSpec(entry.Spec); err != nil {
		return err
	}

	r.mu.Lock()
	if r.countForSessionLocked(entry.SessionKey) >= MaxCronsPerSession {
		r.mu.Unlock()
		return ErrCronCapExceeded
	}
	if entry.ID == "" {
		entry.ID = newCronID()
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	// Default log path if the caller didn't specify one. Puts logs
	// under the session's own .clod/schedbridge/ so they ride along
	// with domain state (bind-mounted into the container, visible to
	// the agent via Read).
	if entry.LastOutputPath == "" && entry.CWD != "" {
		entry.LastOutputPath = filepath.Join(entry.CWD, ".clod", "schedbridge", entry.ID+".log")
	}
	r.crons[entry.ID] = entry
	r.mu.Unlock()

	// Schedule with the engine (no-op if Start hasn't run yet;
	// Start's own pass will pick it up).
	if err := r.scheduleByID(entry.ID); err != nil {
		// Roll back the in-memory add — otherwise we've persisted an
		// entry the engine refused.
		r.mu.Lock()
		delete(r.crons, entry.ID)
		r.mu.Unlock()
		return err
	}

	return nil
}

// Delete removes a cron entry by ID and unschedules its ticker.
// Returns whether an entry was removed; callers persist via Save.
func (r *SchedulingRegistry) Delete(id string) bool {
	r.mu.Lock()
	_, ok := r.crons[id]
	if ok {
		delete(r.crons, id)
	}
	r.mu.Unlock()
	if ok {
		r.unscheduleByID(id)
	}
	return ok
}

// DeleteSession removes every cron entry owned by sessionKey and
// unschedules matching tickers. Returns the removed IDs. Used by
// @bot close and session archival.
func (r *SchedulingRegistry) DeleteSession(sessionKey string) []string {
	r.mu.Lock()
	var removed []string
	for id, e := range r.crons {
		if e.SessionKey == sessionKey {
			removed = append(removed, id)
			delete(r.crons, id)
		}
	}
	r.mu.Unlock()
	sort.Strings(removed)
	for _, id := range removed {
		r.unscheduleByID(id)
	}
	return removed
}

// UpdateFireResult records the outcome of the most recent tick for
// entry id. Persists to disk. Best-effort — a save failure is logged
// but not returned to the tick goroutine (missing one persist
// doesn't warrant killing the schedule).
func (r *SchedulingRegistry) UpdateFireResult(id string, fireAt time.Time, exitCode int, spawnErr string, outputPath string) {
	r.mu.Lock()
	entry := r.crons[id]
	if entry == nil {
		r.mu.Unlock()
		return
	}
	entry.LastFireAt = fireAt
	entry.LastExitCode = exitCode
	entry.LastError = spawnErr
	entry.LastOutputPath = outputPath
	r.mu.Unlock()
	if err := r.Save(); err != nil {
		r.logger.Warn().Err(err).Str("cron_id", id).Msg("failed to persist tick result")
	}
}

// Start boots the internal cron engine and schedules every persisted
// entry. Safe to call once at bot startup after NewSchedulingRegistry.
// A double Start is a no-op — the engine is created on first call.
func (r *SchedulingRegistry) Start() {
	r.mu.Lock()
	if r.engine != nil {
		r.mu.Unlock()
		return
	}
	// Use standard cron parsing with @every / @hourly / @daily descriptors.
	// cron.SkipIfStillRunning drops overlapping ticks per mcp-shim.md §8.
	r.engine = cron.New(cron.WithParser(cron.NewParser(
		cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
	)))
	r.mu.Unlock()

	// Schedule persisted entries. Any parse errors get logged but don't
	// halt startup — one bad row shouldn't take out the whole registry.
	// Snapshot IDs under the lock, then schedule outside; each schedule
	// call takes the lock itself.
	r.mu.RLock()
	ids := make([]string, 0, len(r.crons))
	for id := range r.crons {
		ids = append(ids, id)
	}
	r.mu.RUnlock()

	for _, id := range ids {
		if err := r.scheduleByID(id); err != nil {
			r.logger.Warn().Err(err).Str("cron_id", id).Msg("failed to schedule persisted cron on Start")
		}
	}

	r.engine.Start()
	r.logger.Info().Int("scheduled", len(r.entries)).Msg("scheduling engine started")
}

// Stop halts the cron engine. Any in-flight ticks complete (cron.Stop
// returns a context that finishes when they do — we don't wait on it
// here; bot shutdown handles that at a higher level).
func (r *SchedulingRegistry) Stop() {
	r.mu.Lock()
	engine := r.engine
	r.mu.Unlock()
	if engine == nil {
		return
	}
	engine.Stop()
	r.logger.Info().Msg("scheduling engine stopped")
}

// scheduleByID looks up entry id, parses its Spec, and registers it
// with the engine. Any prior scheduling for this ID is removed first
// (Add re-schedules on update paths, and Start does the initial pass).
// Not exported — callers use Add/Delete which handle the lifecycle.
func (r *SchedulingRegistry) scheduleByID(id string) error {
	r.mu.Lock()
	entry := r.crons[id]
	engine := r.engine
	oldEntryID, hadOld := r.entries[id]
	r.mu.Unlock()

	if entry == nil {
		return oops.Trace(fmt.Errorf("no cron entry with id %q", id))
	}
	if engine == nil {
		// Engine not started yet — Start will pick it up in its own
		// scheduling pass. Not an error.
		return nil
	}

	if err := validateSpec(entry.Spec); err != nil {
		return oops.Trace(err)
	}

	jobFunc := func() { r.runOnce(id) }
	// SkipIfStillRunning: overlapping ticks are dropped rather than
	// queued. Matches the choice recorded in mcp-shim.md §8.
	wrapped := cron.NewChain(cron.SkipIfStillRunning(cronLoggerForID(r.logger, id))).Then(cron.FuncJob(jobFunc))
	engineEntryID, err := engine.AddJob(entry.Spec, wrapped)
	if err != nil {
		return oops.Trace(err)
	}

	r.mu.Lock()
	if hadOld {
		engine.Remove(oldEntryID)
	}
	r.entries[id] = engineEntryID
	// Best-effort next-fire projection — the engine populates this
	// after Start; we only overwrite once the entry exists.
	if next := engine.Entry(engineEntryID).Next; !next.IsZero() {
		entry.NextFireAt = next
	}
	r.mu.Unlock()

	return nil
}

// unscheduleByID removes entry id from the engine. Called by Delete
// and DeleteSession. Safe to call for an ID that was never scheduled.
func (r *SchedulingRegistry) unscheduleByID(id string) {
	r.mu.Lock()
	engine := r.engine
	engineEntryID, ok := r.entries[id]
	if ok {
		delete(r.entries, id)
	}
	r.mu.Unlock()
	if engine != nil && ok {
		engine.Remove(engineEntryID)
	}
}

// runOnce executes one tick of the cron entry with the given ID.
// Captures stdout+stderr to entry.LastOutputPath (creating the
// containing dir if needed), records exit code, persists the result.
// Runs the command via `bash -c` in entry.CWD; the caller (Add) has
// already validated CWD is under the session's domain dir.
func (r *SchedulingRegistry) runOnce(id string) {
	r.mu.RLock()
	entry := r.crons[id]
	r.mu.RUnlock()
	if entry == nil {
		return
	}

	ts := time.Now().UTC()
	logPath := entry.LastOutputPath
	if logPath == "" {
		// Should have been set at Add-time, but fall back defensively.
		logPath = filepath.Join(entry.CWD, ".clod", "schedbridge", entry.ID+".log")
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		r.logger.Warn().Err(err).Str("cron_id", id).Str("log_path", logPath).Msg("failed to create log dir")
		r.UpdateFireResult(id, ts, -1, "mkdir: "+err.Error(), logPath)
		return
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		r.logger.Warn().Err(err).Str("cron_id", id).Str("log_path", logPath).Msg("failed to open log")
		r.UpdateFireResult(id, ts, -1, "open: "+err.Error(), logPath)
		return
	}
	defer f.Close()

	// Header line per tick so grep/tail is meaningful when several
	// runs pile up in the log.
	fmt.Fprintf(f, "\n===== %s cron %s (%s) =====\n", ts.Format(time.RFC3339), id, entry.Description)

	cmd := exec.Command("bash", "-c", entry.Command)
	cmd.Dir = entry.CWD
	cmd.Stdout = f
	cmd.Stderr = f

	err = cmd.Run()
	exitCode := 0
	spawnErr := ""
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			spawnErr = err.Error()
			exitCode = -1
		}
	}

	r.logger.Debug().
		Str("cron_id", id).
		Int("exit", exitCode).
		Str("description", entry.Description).
		Msg("cron tick complete")

	r.UpdateFireResult(id, ts, exitCode, spawnErr, logPath)
}

// cronLoggerForID returns a robfig cron.Logger backed by zerolog so
// SkipIfStillRunning warnings flow into the same log stream as the
// rest of the bot.
func cronLoggerForID(base zerolog.Logger, id string) cron.Logger {
	return &zerologCronAdapter{
		logger: base.With().Str("component", "scheduling").Str("cron_id", id).Logger(),
	}
}

type zerologCronAdapter struct {
	logger zerolog.Logger
}

func (z *zerologCronAdapter) Info(msg string, keysAndValues ...interface{}) {
	z.logger.Info().Fields(cronKV(keysAndValues)).Msg(msg)
}

func (z *zerologCronAdapter) Error(err error, msg string, keysAndValues ...interface{}) {
	z.logger.Error().Err(err).Fields(cronKV(keysAndValues)).Msg(msg)
}

// cronKV flattens the robfig alternating key/value slice into a map
// zerolog can consume. Missing values default to nil.
func cronKV(kv []interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			continue
		}
		out[key] = kv[i+1]
	}
	return out
}

// validateSpec checks that spec parses AND (for @every) that the
// interval is at least MinCronInterval. Standard 5-field cron passes
// through since its minimum granularity is 1m > 30s already.
func validateSpec(spec string) error {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	if _, err := parser.Parse(spec); err != nil {
		return oops.Trace(fmt.Errorf("invalid cron spec %q: %w", spec, err))
	}
	// Cheap tighter check for @every: enforce the min-interval floor.
	if strings.HasPrefix(strings.ToLower(spec), "@every ") {
		durStr := strings.TrimSpace(spec[len("@every "):])
		dur, err := time.ParseDuration(durStr)
		if err == nil && dur < MinCronInterval {
			return oops.Trace(fmt.Errorf("interval %s below floor %s", dur, MinCronInterval))
		}
	}
	return nil
}

// ErrCronCapExceeded is returned by Add when the session already has
// MaxCronsPerSession entries.
var ErrCronCapExceeded = fmt.Errorf("cron cap exceeded (max %d per session)", MaxCronsPerSession)

// newCronID returns "cron_<8 lowercase hex>". 4 bytes of randomness
// gives 2^32 possible IDs; collisions across a workspace are
// vanishingly unlikely even before the per-session cap.
func newCronID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Shouldn't happen on Linux; fall back to time-based so we
		// still return a syntactically-valid ID and let the caller
		// notice via logs if this pattern ever recurs.
		return fmt.Sprintf("cron_%08x", time.Now().UnixNano()&0xffffffff)
	}
	return "cron_" + hex.EncodeToString(b[:])
}

// deriveCronsPath places crons.json alongside sessions.json, matching
// the deriveUsagePath convention. A single "storage root" implied by
// the sessions path keeps deployment simple.
func deriveCronsPath(sessionsPath string) string {
	dir := filepath.Dir(sessionsPath)
	return filepath.Join(dir, "crons.json")
}

// ValidateCWDUnderDomain resolves candidate to a canonical absolute
// path and verifies it sits at or below domainDir. Returns the
// canonical form on success, an error on any escape attempt
// (`..` traversal, symlink out of tree, absolute path elsewhere).
// The caller passes an already-cleaned domainDir.
//
// Model input flows through this before it becomes an exec cwd,
// so it MUST be strict — reject on the slightest doubt.
func ValidateCWDUnderDomain(candidate, domainDir string) (string, error) {
	if candidate == "" {
		return domainDir, nil
	}
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(domainDir, candidate)
	}
	canon, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		// If the path doesn't exist yet, EvalSymlinks fails — fall
		// back to Clean; the exec will fail loudly if the dir is
		// bogus, and the containment check still applies.
		canon = filepath.Clean(candidate)
	}
	domainCanon, err := filepath.EvalSymlinks(domainDir)
	if err != nil {
		domainCanon = filepath.Clean(domainDir)
	}
	// Add trailing separator so "domainDir" doesn't match
	// "domainDir-evil-suffix".
	sep := string(filepath.Separator)
	if !strings.HasPrefix(canon+sep, domainCanon+sep) {
		return "", oops.Trace(fmt.Errorf("cwd %q escapes domain dir %q", canon, domainCanon))
	}
	return canon, nil
}
