package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/calebcase/oops"
	"github.com/rs/zerolog"
)

// SessionMapping represents a Slack thread to clod session mapping.
type SessionMapping struct {
	ChannelID      string    `json:"channel_id"`
	ThreadTS       string    `json:"thread_ts"`
	TaskName       string    `json:"task_name"`
	TaskPath       string    `json:"task_path"`
	SessionID      string    `json:"session_id"`
	UserID         string    `json:"user_id"`
	VerbosityLevel int       `json:"verbosity_level"` // Per-thread verbosity: -1 (silent), 0 (summary), 1 (full)
	// Model is the Claude model to use for this thread. Empty means "bot
	// default" (whatever the bot was configured with via the CLI flag).
	// Valid values: "opus", "sonnet", "claude-haiku-4-5" (and any other
	// string claude --model accepts).
	Model string `json:"model,omitempty"`
	// ReactionAnchorTS is the Slack TS of the user's @-mention that kicked
	// off this task. It's the anchor the bot uses for the model-indicator
	// reaction so the indicator sits on the message that started the thread
	// (not on the bot's own "Starting..." status post).
	ReactionAnchorTS string `json:"reaction_anchor_ts,omitempty"`
	// Active is true whenever runClod is executing against this session.
	// Cleared only on *clean* completion; an unclean exit (shutdown, crash,
	// timeout) leaves it set so the bot can resume on next startup. The
	// flag is paired with UpdatedAt (heartbeat-bumped while running) to
	// decide whether a still-Active session is fresh enough to resume.
	Active bool `json:"active,omitempty"`
	// ActiveMonitors is the set of task_ids currently running under the
	// agent's `Monitor` tool. Populated as Monitor starts arrive and
	// drained on TaskStop; a keycap reaction on ReactionAnchorTS reflects
	// len(ActiveMonitors) so the user can see at a glance how many
	// background watchers the agent is juggling.
	ActiveMonitors []string `json:"active_monitors,omitempty"`
	// MonitorCountEmoji is the keycap reaction name currently attached to
	// ReactionAnchorTS for monitor count. Stored so we can remove the
	// previous one when swapping in a new one — Slack has no "replace".
	MonitorCountEmoji string `json:"monitor_count_emoji,omitempty"`
	// ModelReactionEmoji is the bot's own model-indicator reaction on
	// ReactionAnchorTS. Tracked separately from Model because the user's
	// own reaction (which kicks off a switch) can't be removed by the
	// bot (Slack reactions are per-user), so we have to remember exactly
	// which emoji WE added and limit our removals to that.
	ModelReactionEmoji string `json:"model_reaction_emoji,omitempty"`
	// PermissionMode is the claude --permission-mode value to use on this
	// thread's next run. Empty falls back to the bot's configured default.
	// "plan" enables plan mode (agent researches + proposes, doesn't edit
	// without approval); set by default for new tasks and toggled via
	// the plan-mode reaction on the anchor message. Mid-turn switching
	// isn't supported by claude — applies on the next process start.
	PermissionMode string `json:"permission_mode,omitempty"`
	// ExtraAllowedUsers is the set of Slack user IDs granted per-thread
	// authorization in addition to the bot-wide allowlist. Managed via
	// `@bot allow @user` / `@bot disallow @user`. Only users authorized
	// in this thread (bot-wide OR per-thread) can drive the bot here.
	ExtraAllowedUsers []string `json:"extra_allowed_users,omitempty"`
	// FileSyncDisabled turns off the task-directory-to-Slack file
	// watcher for this thread. Default (false) means sync is ON — new
	// or modified files in the top-level task dir get uploaded as
	// snippets. Toggled via `@bot set filesync=off`.
	FileSyncDisabled bool `json:"file_sync_disabled,omitempty"`
	// UseClaudeDirect runs `claude` on the host instead of `clod` (which
	// wraps claude in a docker container). Set exclusively via `@bot !:`
	// after the user confirms the risk. Sticky for the life of the
	// session so continuations keep the same execution mode.
	UseClaudeDirect bool `json:"use_claude_direct,omitempty"`
	// CumulativeCostUSD and CumulativeTurns accumulate across every clod
	// invocation within this thread. Claude's per-result stats cover only
	// the current process's lifetime, so a resume (or a crash-and-respawn)
	// would otherwise reset cost/turn counters — the user loses the big
	// picture of what the thread is spending. These fields persist the
	// running total so the stats block always shows lifetime numbers.
	CumulativeCostUSD float64 `json:"cumulative_cost_usd,omitempty"`
	CumulativeTurns   int     `json:"cumulative_turns,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// UsageSample records a per-result stats event so the Home tab can
// render per-user usage rollups over time windows (24h, 7d, 30d,
// 90d, 365d). Keys are short to keep the sidecar file small: u =
// userID, c = cost (USD), t = turns, at = timestamp. Samples older
// than 365 days are pruned at append time.
type UsageSample struct {
	UserID  string    `json:"u"`
	CostUSD float64   `json:"c"`
	Turns   int       `json:"t"`
	At      time.Time `json:"at"`
}

// UsageTotals aggregates cost + turns over a time window. Returned
// by UsageRollup, one per (user, window) pair.
type UsageTotals struct {
	CostUSD float64
	Turns   int
}

// usageTTL bounds how long samples live in memory / on disk. Matches
// the longest window UsageRollup is ever asked about; samples older
// than this can never contribute to a rollup so there's no reason to
// keep them. Kept as a var rather than const so tests can override.
var usageTTL = 365 * 24 * time.Hour

// SessionStore manages thread-to-session mappings with JSON persistence.
type SessionStore struct {
	path                  string
	sessions              map[string]*SessionMapping // key: "channelID:threadTS"
	mu                    sync.RWMutex
	defaultVerbosityLevel int
	logger                zerolog.Logger

	// Usage samples (per-result cost + turns events). Stored in a
	// sidecar `usage.json` next to the session path so the regular
	// session Save() doesn't re-serialize a growing array of
	// samples on every heartbeat. Appended under `mu` (write lock)
	// alongside the AddStats mutation that triggers them.
	usagePath string
	usage     []*UsageSample
}

// Count returns the number of stored sessions.
func (s *SessionStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// AllSessions returns a snapshot of every session as a fresh slice.
// Used by the Home tab renderer to build usage aggregates; avoids
// exposing the underlying map to callers who might forget to hold
// the lock.
func (s *SessionStore) AllSessions() []*SessionMapping {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*SessionMapping, 0, len(s.sessions))
	for _, m := range s.sessions {
		out = append(out, m)
	}
	return out
}

// NewSessionStore creates a new SessionStore and loads existing sessions.
func NewSessionStore(path string, defaultVerbosityLevel int, logger zerolog.Logger) (*SessionStore, error) {
	s := &SessionStore{
		path:                  path,
		usagePath:             deriveUsagePath(path),
		sessions:              make(map[string]*SessionMapping),
		defaultVerbosityLevel: defaultVerbosityLevel,
		logger:                logger.With().Str("component", "session_store").Logger(),
	}

	if err := s.Load(); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := s.LoadUsage(); err != nil && !os.IsNotExist(err) {
		s.logger.Warn().Err(err).Msg("failed to load usage samples; starting empty")
	}

	return s, nil
}

// deriveUsagePath places the usage sidecar alongside sessions.json,
// using a fixed `usage.json` name. Separate file so session saves
// (which fire on heartbeats + every flag mutation) don't pay the
// cost of re-serializing a growing sample array.
func deriveUsagePath(sessionsPath string) string {
	dir := filepath.Dir(sessionsPath)
	return filepath.Join(dir, "usage.json")
}

// AppendUsageSample records a single cost/turns event. Thread-safe;
// callers don't need to hold s.mu. Opportunistic pruning drops
// samples older than usageTTL so the in-memory slice stays bounded.
// Does NOT persist — callers handle that via SaveUsage so multiple
// samples (e.g. a result + the cumulative update path) can batch.
func (s *SessionStore) AppendUsageSample(userID string, costUSD float64, turns int) {
	if userID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage = append(s.usage, &UsageSample{
		UserID:  userID,
		CostUSD: costUSD,
		Turns:   turns,
		At:      time.Now(),
	})
	s.pruneUsageLocked()
}

// pruneUsageLocked drops samples older than usageTTL. Must be called
// with s.mu held. Slice is assumed to be (approximately) time-sorted
// since samples are appended in order; we scan from the front until
// we find one that's fresh enough and cut everything before it.
func (s *SessionStore) pruneUsageLocked() {
	if len(s.usage) == 0 {
		return
	}
	cutoff := time.Now().Add(-usageTTL)
	i := 0
	for i < len(s.usage) && s.usage[i].At.Before(cutoff) {
		i++
	}
	if i > 0 {
		s.usage = s.usage[i:]
	}
}

// UsageRollup returns per-user totals bucketed into each of the
// supplied time windows. Windows must be in ascending order; the
// result for each user is aligned with windows so result[user][i]
// covers `windows[i]` back from now. Samples contribute to every
// window that contains them (a sample from 2 hours ago lands in
// 24h, 7d, 30d, 90d, and 365d buckets).
func (s *SessionStore) UsageRollup(windows []time.Duration) map[string][]UsageTotals {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	result := make(map[string][]UsageTotals)
	for _, sample := range s.usage {
		age := now.Sub(sample.At)
		per, ok := result[sample.UserID]
		if !ok {
			per = make([]UsageTotals, len(windows))
			result[sample.UserID] = per
		}
		for i, w := range windows {
			if age <= w {
				per[i].CostUSD += sample.CostUSD
				per[i].Turns += sample.Turns
			}
		}
	}
	return result
}

// LoadUsage reads the sidecar file. Missing file is not an error
// (first-run condition); callers coalesce os.IsNotExist and start
// fresh.
func (s *SessionStore) LoadUsage() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.usagePath)
	if err != nil {
		return err
	}
	var wrapped struct {
		Samples []*UsageSample `json:"samples"`
	}
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return oops.Trace(err)
	}
	s.usage = wrapped.Samples
	s.pruneUsageLocked()
	return nil
}

// SaveUsage serializes samples to the sidecar. Wrapped in an object
// so we can add schema fields (e.g. a version marker) later without
// breaking the reader.
func (s *SessionStore) SaveUsage() error {
	s.mu.RLock()
	wrapped := struct {
		Samples []*UsageSample `json:"samples"`
	}{Samples: s.usage}
	data, err := json.MarshalIndent(wrapped, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return oops.Trace(err)
	}
	tmp := s.usagePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return oops.Trace(err)
	}
	if err := os.Rename(tmp, s.usagePath); err != nil {
		return oops.Trace(err)
	}
	return nil
}

// key generates the map key for a channel/thread pair.
func key(channelID, threadTS string) string {
	return channelID + ":" + threadTS
}

// Get retrieves a session mapping by channel and thread.
func (s *SessionStore) Get(channelID, threadTS string) *SessionMapping {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.sessions[key(channelID, threadTS)]
}

// Set stores a session mapping.
func (s *SessionStore) Set(mapping *SessionMapping) {
	s.mu.Lock()
	defer s.mu.Unlock()

	mapping.UpdatedAt = time.Now()
	s.sessions[key(mapping.ChannelID, mapping.ThreadTS)] = mapping
}

// SetVerbosityLevel sets the verbosity level for a thread, creating a minimal
// session entry if none exists yet.
func (s *SessionStore) SetVerbosityLevel(channelID, threadTS string, level int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(channelID, threadTS)
	session := s.sessions[k]
	if session == nil {
		// Create a minimal session entry to hold the verbosity setting for threads
		// that haven't started a task yet.
		session = &SessionMapping{
			ChannelID: channelID,
			ThreadTS:  threadTS,
			CreatedAt: time.Now(),
		}
		s.sessions[k] = session
	}
	session.VerbosityLevel = level
	session.UpdatedAt = time.Now()
}

// SetActive flips the Active flag on a session. Paired with UpdatedAt to
// drive resume-on-restart: callers set Active=true when a task starts and
// clear it only on clean completion. Touches UpdatedAt so the caller can
// treat this as a heartbeat too.
//
// Every transition is logged with caller file:line so stale-Active or
// prematurely-cleared-Active bugs are self-diagnosing on the next
// occurrence. The log level is info only when the value actually
// changes; redundant writes stay at debug so the signal-to-noise stays
// reasonable under heartbeat churn.
func (s *SessionStore) SetActive(channelID, threadTS string, active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(channelID, threadTS)
	session := s.sessions[k]
	if session == nil {
		session = &SessionMapping{
			ChannelID: channelID,
			ThreadTS:  threadTS,
			CreatedAt: time.Now(),
		}
		s.sessions[k] = session
	}
	prev := session.Active
	session.Active = active
	session.UpdatedAt = time.Now()

	var evt *zerolog.Event
	if prev != active {
		evt = s.logger.Info()
	} else {
		evt = s.logger.Debug()
	}
	caller := "?"
	if _, file, line, ok := runtime.Caller(1); ok {
		caller = shortCaller(file, line)
	}
	evt.
		Str("channel", channelID).
		Str("thread_ts", threadTS).
		Str("task", session.TaskName).
		Bool("prev", prev).
		Bool("active", active).
		Str("caller", caller).
		Msg("session active flag")
}

// shortCaller trims a full file path down to "<basename>:<line>" for
// compact log lines. A full path carries no extra information past
// the basename since this is a single-package project.
func shortCaller(file string, line int) string {
	return filepath.Base(file) + ":" + strconv.Itoa(line)
}

// Touch bumps UpdatedAt so the session's "last seen alive" timestamp stays
// fresh while work is happening. Used as a periodic heartbeat from inside
// the runClod event loop so resume-on-restart can judge whether the bot
// died "just now" (resume) or "a long time ago" (stale — skip).
func (s *SessionStore) Touch(channelID, threadTS string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(channelID, threadTS)
	if session := s.sessions[k]; session != nil {
		session.UpdatedAt = time.Now()
	}
}

// ActiveSessions returns all sessions currently flagged Active whose
// UpdatedAt is within maxAge. Older Active sessions are returned in the
// "stale" slice so the caller can clean them up (clear the flag) without
// attempting a resume.
func (s *SessionStore) ActiveSessions(maxAge time.Duration) (fresh []*SessionMapping, stale []*SessionMapping) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	cutoff := time.Now().Add(-maxAge)
	for _, session := range s.sessions {
		if !session.Active {
			continue
		}
		// Make a shallow copy so callers don't race with us on mutation.
		copied := *session
		if session.UpdatedAt.Before(cutoff) {
			stale = append(stale, &copied)
		} else {
			fresh = append(fresh, &copied)
		}
	}
	return fresh, stale
}

// AddMonitor records a new active monitor task_id on a thread, returning
// the new count. No-op (returns existing count) if the id is already
// tracked, so a replayed `Monitor started` message during a resume
// doesn't inflate the count.
func (s *SessionStore) AddMonitor(channelID, threadTS, taskID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(channelID, threadTS)
	session := s.sessions[k]
	if session == nil {
		session = &SessionMapping{
			ChannelID: channelID,
			ThreadTS:  threadTS,
			CreatedAt: time.Now(),
		}
		s.sessions[k] = session
	}
	for _, id := range session.ActiveMonitors {
		if id == taskID {
			return len(session.ActiveMonitors)
		}
	}
	session.ActiveMonitors = append(session.ActiveMonitors, taskID)
	session.UpdatedAt = time.Now()
	return len(session.ActiveMonitors)
}

// RemoveMonitor drops a monitor task_id from the active set, returning the
// new count. No-op if the id isn't tracked.
func (s *SessionStore) RemoveMonitor(channelID, threadTS, taskID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(channelID, threadTS)
	session := s.sessions[k]
	if session == nil {
		return 0
	}
	out := session.ActiveMonitors[:0]
	for _, id := range session.ActiveMonitors {
		if id != taskID {
			out = append(out, id)
		}
	}
	session.ActiveMonitors = out
	session.UpdatedAt = time.Now()
	return len(session.ActiveMonitors)
}

// ClearMonitors empties the active-monitors list. Called when the agent's
// container exits (task.Done) and on resume-after-restart — monitors
// inside the old container are gone, and the agent will re-announce any
// it re-creates.
func (s *SessionStore) ClearMonitors(channelID, threadTS string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(channelID, threadTS)
	session := s.sessions[k]
	if session == nil {
		return
	}
	if len(session.ActiveMonitors) == 0 && session.MonitorCountEmoji == "" {
		return
	}
	session.ActiveMonitors = nil
	// MonitorCountEmoji is the emoji currently on the anchor message;
	// clearing the list doesn't remove the reaction — the handler does
	// that and then calls SetMonitorCountEmoji("") to sync state.
	session.UpdatedAt = time.Now()
}

// SetMonitorCountEmoji records which keycap reaction the bot currently has
// on the anchor message, so the next update can remove the previous one.
func (s *SessionStore) SetMonitorCountEmoji(channelID, threadTS, emoji string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(channelID, threadTS)
	session := s.sessions[k]
	if session == nil {
		return
	}
	session.MonitorCountEmoji = emoji
	session.UpdatedAt = time.Now()
}

// SetModelReactionEmoji records which model-indicator reaction the bot
// currently has on the anchor message. Used so model switches remove the
// right emoji even if session.Model has drifted out of sync with what
// was actually posted.
func (s *SessionStore) SetModelReactionEmoji(channelID, threadTS, emoji string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(channelID, threadTS)
	session := s.sessions[k]
	if session == nil {
		session = &SessionMapping{
			ChannelID: channelID,
			ThreadTS:  threadTS,
			CreatedAt: time.Now(),
		}
		s.sessions[k] = session
	}
	session.ModelReactionEmoji = emoji
	session.UpdatedAt = time.Now()
}

// SetModel stores the per-thread Claude model preference. Creates a minimal
// session entry if the thread hasn't started a task yet. Empty string means
// "use bot default".
func (s *SessionStore) SetModel(channelID, threadTS, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(channelID, threadTS)
	session := s.sessions[k]
	if session == nil {
		session = &SessionMapping{
			ChannelID: channelID,
			ThreadTS:  threadTS,
			CreatedAt: time.Now(),
		}
		s.sessions[k] = session
	}
	session.Model = model
	session.UpdatedAt = time.Now()
}

// AddStats increments the thread's cumulative cost and turn counters
// and returns the new totals. Called after each clod invocation's
// `result` stream message so the stats block reflects lifetime
// numbers rather than just the current process's run.
func (s *SessionStore) AddStats(channelID, threadTS string, costUSD float64, turns int) (float64, int) {
	s.mu.Lock()
	k := key(channelID, threadTS)
	session := s.sessions[k]
	if session == nil {
		session = &SessionMapping{
			ChannelID: channelID,
			ThreadTS:  threadTS,
			CreatedAt: time.Now(),
		}
		s.sessions[k] = session
	}
	session.CumulativeCostUSD += costUSD
	session.CumulativeTurns += turns
	session.UpdatedAt = time.Now()
	cumulativeCost := session.CumulativeCostUSD
	cumulativeTurns := session.CumulativeTurns
	// Append a usage sample under the same lock so readers see a
	// consistent cumulative-plus-sample pair. UserID must be set on
	// the session for the sample to count toward the per-user
	// Workspace rollup; anonymous sessions contribute to lifetime
	// cumulatives but not to the windowed rollup.
	userID := session.UserID
	if userID != "" {
		s.usage = append(s.usage, &UsageSample{
			UserID:  userID,
			CostUSD: costUSD,
			Turns:   turns,
			At:      time.Now(),
		})
		s.pruneUsageLocked()
	}
	s.mu.Unlock()
	return cumulativeCost, cumulativeTurns
}

// AddExtraAllowedUser grants per-thread authorization to userID. Returns
// (true, count) if the entry was added, (false, count) if already present.
// Count is the new total (including any existing entries).
func (s *SessionStore) AddExtraAllowedUser(channelID, threadTS, userID string) (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(channelID, threadTS)
	session := s.sessions[k]
	if session == nil {
		return false, 0
	}
	for _, u := range session.ExtraAllowedUsers {
		if u == userID {
			return false, len(session.ExtraAllowedUsers)
		}
	}
	session.ExtraAllowedUsers = append(session.ExtraAllowedUsers, userID)
	session.UpdatedAt = time.Now()
	return true, len(session.ExtraAllowedUsers)
}

// RemoveExtraAllowedUser revokes per-thread authorization from userID.
// Returns (true, count) if the entry existed and was removed, else
// (false, count). Count is the new total.
func (s *SessionStore) RemoveExtraAllowedUser(channelID, threadTS, userID string) (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(channelID, threadTS)
	session := s.sessions[k]
	if session == nil {
		return false, 0
	}
	out := session.ExtraAllowedUsers[:0]
	removed := false
	for _, u := range session.ExtraAllowedUsers {
		if u == userID {
			removed = true
			continue
		}
		out = append(out, u)
	}
	session.ExtraAllowedUsers = out
	if removed {
		session.UpdatedAt = time.Now()
	}
	return removed, len(session.ExtraAllowedUsers)
}

// IsExtraAllowedUser reports whether userID is on this thread's
// per-thread allowlist. Check the bot-wide allowlist separately.
func (s *SessionStore) IsExtraAllowedUser(channelID, threadTS, userID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session := s.sessions[key(channelID, threadTS)]
	if session == nil {
		return false
	}
	for _, u := range session.ExtraAllowedUsers {
		if u == userID {
			return true
		}
	}
	return false
}

// SetFileSyncDisabled toggles the per-thread file-sync preference.
// disabled=true turns off the task-directory-to-Slack watcher.
func (s *SessionStore) SetFileSyncDisabled(channelID, threadTS string, disabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(channelID, threadTS)
	session := s.sessions[k]
	if session == nil {
		session = &SessionMapping{
			ChannelID: channelID,
			ThreadTS:  threadTS,
			CreatedAt: time.Now(),
		}
		s.sessions[k] = session
	}
	session.FileSyncDisabled = disabled
	session.UpdatedAt = time.Now()
}

// IsFileSyncDisabled reports whether file sync has been explicitly
// disabled on this thread. Returns false (i.e. sync enabled) when no
// session exists yet.
func (s *SessionStore) IsFileSyncDisabled(channelID, threadTS string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session := s.sessions[key(channelID, threadTS)]
	if session == nil {
		return false
	}
	return session.FileSyncDisabled
}

// SetUseClaudeDirect marks the thread as running claude directly on
// the host (bypassing clod/docker). Creates a bare session entry if
// none exists yet so the flag sticks through to runClod.
func (s *SessionStore) SetUseClaudeDirect(channelID, threadTS string, direct bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(channelID, threadTS)
	session := s.sessions[k]
	if session == nil {
		session = &SessionMapping{
			ChannelID: channelID,
			ThreadTS:  threadTS,
			CreatedAt: time.Now(),
		}
		s.sessions[k] = session
	}
	session.UseClaudeDirect = direct
	session.UpdatedAt = time.Now()
}

// IsUseClaudeDirect reports whether this thread is configured to run
// claude directly on the host. Returns false when no session exists.
func (s *SessionStore) IsUseClaudeDirect(channelID, threadTS string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session := s.sessions[key(channelID, threadTS)]
	if session == nil {
		return false
	}
	return session.UseClaudeDirect
}

// SetPermissionMode stores the per-thread claude --permission-mode
// preference. Empty string means "use bot default".
func (s *SessionStore) SetPermissionMode(channelID, threadTS, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(channelID, threadTS)
	session := s.sessions[k]
	if session == nil {
		session = &SessionMapping{
			ChannelID: channelID,
			ThreadTS:  threadTS,
			CreatedAt: time.Now(),
		}
		s.sessions[k] = session
	}
	session.PermissionMode = mode
	session.UpdatedAt = time.Now()
}

// GetPermissionMode returns the thread's permission-mode preference, or
// empty string if unset.
func (s *SessionStore) GetPermissionMode(channelID, threadTS string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session := s.sessions[key(channelID, threadTS)]
	if session == nil {
		return ""
	}
	return session.PermissionMode
}

// GetModel returns the thread's model preference, or empty string if unset.
func (s *SessionStore) GetModel(channelID, threadTS string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session := s.sessions[key(channelID, threadTS)]
	if session == nil {
		return ""
	}
	return session.Model
}

// GetVerbosityLevel returns the thread's verbosity level, or the store default if no session exists.
func (s *SessionStore) GetVerbosityLevel(channelID, threadTS string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session := s.sessions[key(channelID, threadTS)]
	if session == nil {
		return s.defaultVerbosityLevel
	}
	return session.VerbosityLevel
}

// Load reads sessions from the JSON file.
// Returns nil if the file doesn't exist (fresh start).
func (s *SessionStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			// No sessions file yet, start fresh
			return nil
		}
		return oops.Trace(err)
	}

	var sessions []*SessionMapping
	if err := json.Unmarshal(data, &sessions); err != nil {
		return oops.Trace(err)
	}

	s.sessions = make(map[string]*SessionMapping, len(sessions))
	for _, session := range sessions {
		s.sessions[key(session.ChannelID, session.ThreadTS)] = session
	}

	return nil
}

// Save writes sessions to the JSON file atomically.
func (s *SessionStore) Save() error {
	s.mu.RLock()
	sessions := make([]*SessionMapping, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		return oops.Trace(err)
	}

	// Atomic write: temp file + rename
	dir := filepath.Dir(s.path)
	tmpFile, err := os.CreateTemp(dir, "sessions-*.json.tmp")
	if err != nil {
		return oops.Trace(err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return oops.Trace(err)
	}

	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return oops.Trace(err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return oops.Trace(err)
	}

	return nil
}
