package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/calebcase/oops"
	"github.com/creack/pty"
	"github.com/rs/zerolog"
)

// clodRuntimePrompt is the runtime notice baked into the top of every
// generated CONTEXT.md (see permission.go's NewPermissionFIFO). Kept in
// its own text file so it reads easily and can be edited without
// touching Go source. Used to live as its own --append-system-prompt
// flag, but claude rejects combining --append-system-prompt with
// --append-system-prompt-file, so it now ships inside the file.
//
//go:embed clod_runtime_prompt.txt
var clodRuntimePrompt string

// rerunPattern matches Claude Code's [rerun: ...] control messages that shouldn't be shown to users.
// Matches both "[rerun: b1]" (with space) and "[rerun:b1]" (without space).
var rerunPattern = regexp.MustCompile(`\[rerun:\s*[^\]]+\]\s*`)

// trivialBashResults are exact tool_result bodies that carry no useful
// information. They're tagged with __TRIVIAL__ so the handler drops them
// unless the user bumped verbosity. Keep the match list conservative —
// false positives here silently hide real output.
var trivialBashResults = map[string]bool{
	"(Bash completed with no output)": true,
	"(No output)":                     true,
}

// isTrivialToolResult reports whether a tool_result is safe to hide at
// default verbosity. Only Bash's "no output" sentinels qualify today; other
// tools' empty/short results often still carry signal (grep's "no matches
// found", ls of an empty dir, etc.).
func isTrivialToolResult(toolName, trimmedContent string) bool {
	if toolName != "Bash" {
		return false
	}
	return trivialBashResults[trimmedContent]
}

// progressStderrPattern matches stderr lines worth surfacing to the user as
// "something is happening" progress updates while the container/image is
// being prepared. Matches:
//   - lines prefixed with "[clod]" (wrapper lifecycle: SSH agent, errors)
//   - docker buildkit step markers like "#12 [wrapper 5/5] RUN ..." or
//     "#12 DONE 0.1s"
//
// Cache-hit lines ("#N CACHED") and low-signal exporter chatter are skipped
// to keep the progress post readable.
var progressStderrPattern = regexp.MustCompile(`^(\[clod\]|#\d+ (\[|DONE))`)

// Runner executes clod processes.
type Runner struct {
	timeout             time.Duration
	permissionMode      string
	domainReadmePath    string // per-domain README (default README.md inside the domain dir)
	workspaceReadmePath string // workspace-root README (default <WorkspacePath>/README.md, absolute)
	logger              zerolog.Logger
}

// NewRunner creates a new Runner.
func NewRunner(
	timeout time.Duration,
	permissionMode string,
	domainReadmePath string,
	workspaceReadmePath string,
	logger zerolog.Logger,
) *Runner {
	return &Runner{
		timeout:             timeout,
		permissionMode:      permissionMode,
		domainReadmePath:    domainReadmePath,
		workspaceReadmePath: workspaceReadmePath,
		logger:              logger.With().Str("component", "runner").Logger(),
	}
}

// Result represents the result of a clod execution.
type Result struct {
	SessionID string
	Output    string
	Error     error
}

// StreamMessage represents a message from Claude's stream-json output.
type StreamMessage struct {
	Type             string         `json:"type"`
	Subtype          string         `json:"subtype,omitempty"` // For system/result messages.
	SessionID        string         `json:"session_id,omitempty"`
	Message          *StreamMsgBody `json:"message,omitempty"`
	Content          string         `json:"content,omitempty"`
	Result           string         `json:"result,omitempty"` // Final result text.
	NotificationType string         `json:"notification_type,omitempty"`
	// Result message stats.
	TotalCostUSD float64 `json:"total_cost_usd,omitempty"`
	DurationMS   int     `json:"duration_ms,omitempty"`
	NumTurns     int     `json:"num_turns,omitempty"`
	IsError      bool    `json:"is_error,omitempty"`
	// For content_block_delta messages - these are TOP-LEVEL fields in the JSON.
	Index int        `json:"index,omitempty"` // Content block index.
	Delta *TextDelta `json:"delta,omitempty"` // The actual delta content.
	// For content_block_start messages (may contain initial text).
	ContentBlock *StreamContentBlock `json:"content_block,omitempty"`
}

// StreamMsgBody represents the message body in stream-json output.
type StreamMsgBody struct {
	Content []StreamContentBlock `json:"content,omitempty"`
}

// StreamContentBlock represents a content block in the message.
type StreamContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// For thinking blocks (extended thinking feature).
	Thinking  string `json:"thinking,omitempty"`  // Thinking content.
	Signature string `json:"signature,omitempty"` // Encrypted thinking signature for multi-turn.
	// For tool_use blocks.
	ID    string         `json:"id,omitempty"`   // Tool use ID.
	Name  string         `json:"name,omitempty"` // Tool name (Bash, Read, etc.).
	Input map[string]any `json:"input,omitempty"`
	// For tool_result blocks.
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // Can be string or []ContentBlock.
	IsError   bool            `json:"is_error,omitempty"`
}

// ToolResultContentBlock represents a content block inside a tool_result.
type ToolResultContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// GetContentText extracts the text content from a tool_result block.
// Handles both string content and array of content blocks.
func (b *StreamContentBlock) GetContentText() string {
	if len(b.Content) == 0 {
		return ""
	}

	// Try to unmarshal as string first.
	var strContent string
	if err := json.Unmarshal(b.Content, &strContent); err == nil {
		return strContent
	}

	// Try to unmarshal as array of content blocks.
	var blocks []ToolResultContentBlock
	if err := json.Unmarshal(b.Content, &blocks); err == nil {
		var texts []string
		for _, block := range blocks {
			if block.Type == "text" && block.Text != "" {
				texts = append(texts, block.Text)
			}
		}
		return strings.Join(texts, "\n")
	}

	return ""
}

// ContentBlockDelta represents a partial content update during streaming.
type ContentBlockDelta struct {
	Index int        `json:"index"`
	Delta *TextDelta `json:"delta,omitempty"`
}

// TextDelta represents the actual text content in a streaming delta.
// Can also contain thinking content or tool input JSON.
type TextDelta struct {
	Type        string `json:"type"` // "text_delta", "thinking_delta", "input_json_delta", "signature_delta"
	Text        string `json:"text,omitempty"`         // For text_delta
	Thinking    string `json:"thinking,omitempty"`     // For thinking_delta (extended thinking)
	PartialJSON string `json:"partial_json,omitempty"` // For input_json_delta (tool input)
	Signature   string `json:"signature,omitempty"`    // For signature_delta (thinking signature)
}

// StreamEventWrapper wraps events in newer Claude Code versions.
type StreamEventWrapper struct {
	Type  string          `json:"type"`  // "stream_event"
	Event json.RawMessage `json:"event"` // Inner event to unwrap.
}

// ControlRequest represents a permission request via control messages.
type ControlRequest struct {
	Type      string         `json:"type"`    // "control_request"
	Subtype   string         `json:"subtype"` // "can_use_tool"
	RequestID string         `json:"request_id,omitempty"`
	ToolName  string         `json:"tool_name,omitempty"`
	ToolInput map[string]any `json:"tool_input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
}

// ControlResponse is sent back to allow/deny a control request.
type ControlResponse struct {
	Type      string `json:"type"`      // "control_response"
	RequestID string `json:"request_id"`
	Behavior  string `json:"behavior"` // "allow" or "deny"
	Message   string `json:"message,omitempty"`
}

// RunningTask represents a clod task that is currently executing.
type RunningTask struct {
	cmd *exec.Cmd
	// pty is the master side of a PTY whose slave is wired to the child's
	// stdout. We never write to it; stream-json output flows out of it.
	pty *os.File
	// stdin is a pipe whose reader end is the child's stdin. Writes on this
	// pipe go straight through docker (-i, no -t) into claude's stream-json
	// reader inside the container. We intentionally do NOT use the PTY for
	// stdin because the kernel line discipline (canonical mode, echo, the
	// MAX_CANON line length cap, ^C/^D interpretation) has no place in a
	// stream-json transport.
	stdin                     *os.File
	output                    chan string
	done                      chan *Result
	cancel                    context.CancelFunc
	sessionID                 string
	taskPath                  string // The path to the task directory.
	logger                    zerolog.Logger
	permissionFIFO            *PermissionFIFO
	controlPermissionRequests chan PermissionRequest
	pendingControlRequestID   string
	// runtimeSuffix is the unique suffix the bot generated for this
	// task's container name (the trailing `-<6hex>` of
	// `clod-<task>-<id>-<suffix>`). Used by forceKill to find and
	// `docker stop` the container in the rare cases where SIGKILL'ing
	// the host process group doesn't propagate cleanly — e.g.
	// `docker run` already orphaned to init, or the daemon is holding
	// the container alive past its shim's death. Empty when claude is
	// running directly on the host (--use-claude-direct), which is
	// also the only mode where there's no container to stop.
	runtimeSuffix string

	// wakeupTimer holds the active bot-side ScheduleWakeup timer, if
	// any. claude's built-in ScheduleWakeup tool returns its
	// "scheduled for…" text but the actual harness for firing the
	// wakeup only exists in /loop / interactive mode — `-p
	// stream-json` callers (us) get the string but no actual wake.
	// The bot intercepts ScheduleWakeup tool_use blocks (see
	// scheduleWakeup) and arms this timer instead, which fires
	// SendInput(prompt) when the delay expires. A second
	// ScheduleWakeup call within the same task replaces the prior
	// timer; cancelWakeupTimer is called on task teardown so a
	// dying task doesn't leave a stray timer hanging onto its
	// SendInput goroutine.
	//
	// wakeupFireID is the id of the currently-armed timer's
	// expected fire. Each arming bumps it; the timer closure
	// captures the value at arming time and skips its SendInput
	// when the captured id doesn't match the live one (timer was
	// superseded between arming and firing). Defensive: Timer.Stop
	// returns false when the fire goroutine has already started
	// but not yet completed, so two timers can race to fire if
	// arming happens at exactly the wrong moment.
	wakeupMu     sync.Mutex
	wakeupTimer  *time.Timer
	wakeupFireID uint64
	// sessionIDCaptured receives the session ID exactly once, as soon as the
	// stream parser observes it (normally the first system/init message). The
	// handler uses it to persist the thread → session mapping early so that
	// a bot restart mid-task doesn't orphan the thread.
	sessionIDCaptured chan string
	sessionIDOnce     sync.Once
	// inputWaitingSince is the unix-nano timestamp of the most-
	// recent SendInput call for which no downstream stream event
	// has yet arrived. Set by SendInput; cleared by the stream
	// parser on the next parsed line. Used by the liveness ticker
	// to detect the specific wedge class where claude receives
	// stdin bytes but its Node event loop never gets around to
	// composing a new HTTP request — same fingerprint as upstream
	// #54434 observed on eagle 2026-07-13 11:50Z where the user's
	// question sat un-processed for 30+ minutes despite all our
	// proxy / connection layer being healthy.
	//
	// Threshold picked at 60s: a normal turn from stdin input to
	// first stream event runs 1–5s (claude spawns request +
	// upstream latency); 60s is well past any legitimate cold-
	// start jitter and shorter than we'd want to sit unnoticed.
	inputWaitingSince atomic.Int64

	// lastStreamAt is the timestamp of the most-recent parsed line
	// from claude's stream-json stdout. Used by the liveness ticker
	// as a proxy for "the model is actively producing output".
	//
	// A v0.36.6 attempt used SSE `ping` events as the heartbeat, on
	// the theory that Anthropic sends them every ~15s. That was
	// wrong for this transport: claude's `--output-format
	// stream-json` does NOT forward SSE pings to stdout — zero
	// `"type":"ping"` events appear across the entire log corpus.
	// Fallback: any parsed stream line counts (content_block_delta,
	// system.*, message_*, assistant, user, result). During active
	// model generation these fire multiple times per second, so a
	// short freshness window reliably captures "actively
	// producing". Absence of stream events does NOT prove wedged
	// though — legitimate long tool waits (a background bash job
	// running python for hours) produce zero events. Absence is a
	// hint, not a verdict; the Home tab and reaction let the user
	// interpret it with context they have and the bot doesn't
	// (what the model was asked to do, how long the tool takes).
	//
	// sync/atomic holder because the writer is the stream parser
	// goroutine and the reader is the liveness ticker.
	lastStreamAt atomic.Value // time.Time
	// compactPending is set when the stream emits a
	// `system.subtype:"compact_boundary"` marker, and cleared on the
	// next content_block_start. The handler surfaces the paired
	// __COMPACT_START__/__COMPACT_END__ sentinels as a rolling
	// "compacting session…" status message so the user can
	// distinguish a legitimate compaction pause from a wedge.
	compactPending atomic.Bool
	// livenessDone is closed by the liveness ticker goroutine when
	// it returns (via runCtx.Done). The outer stream-parsing
	// goroutine waits on it before running close(task.output) so
	// the ticker's __ALIVE__/__STALE__ sends can never race the
	// close. See runCtx cancel defer chain in the outer goroutine.
	livenessDone chan struct{}
}

// closeStdin closes the bot's end of the child's stdin pipe. It's safe to
// call multiple times.
func (t *RunningTask) closeStdin() {
	if t.stdin != nil {
		_ = t.stdin.Close()
		t.stdin = nil
	}
}

// scheduleWakeup arms (or replaces) the bot-side wake-up timer for
// this task in response to a ScheduleWakeup tool_use the agent
// emitted. claude's built-in ScheduleWakeup is a /loop-mode harness
// hook; under `-p --input-format stream-json` (which the bot uses)
// it returns its "scheduled for…" tool_result text but no actual
// wake fires. We bypass the gap by arming our own timer here:
// when it expires, SendInput(prompt) feeds `prompt` back as a fresh
// user message, which wakes the agent through the same path a Slack
// reply would.
//
// Bounds: clamped to [60, 3600] seconds to match the documented
// range of the upstream ScheduleWakeup tool — a single call should
// never push beyond an hour, and anything under a minute is the
// model misusing the tool for a busy-wait.
//
// Only one wakeup is armed per task at a time. A second call wins;
// the prior timer is stopped so we don't get duplicate wake inputs.
func (t *RunningTask) scheduleWakeup(input map[string]any) {
	prompt, _ := input["prompt"].(string)
	reason, _ := input["reason"].(string)

	// delaySeconds may arrive as float64 (json.Unmarshal default for
	// numeric JSON) or json.Number (if a decoder was set to
	// UseNumber). Handle both so we don't silently treat it as 0.
	var delaySeconds float64
	switch v := input["delaySeconds"].(type) {
	case float64:
		delaySeconds = v
	case json.Number:
		delaySeconds, _ = v.Float64()
	case int:
		delaySeconds = float64(v)
	}

	const minDelay, maxDelay = 60.0, 3600.0
	if delaySeconds < minDelay {
		delaySeconds = minDelay
	}
	if delaySeconds > maxDelay {
		delaySeconds = maxDelay
	}

	log := t.logger.With().
		Float64("delay_s", delaySeconds).
		Str("reason", reason).
		Int("prompt_len", len(prompt)).
		Logger()

	if prompt == "" {
		log.Warn().Msg("ScheduleWakeup with empty prompt; not arming timer")
		return
	}

	t.wakeupMu.Lock()
	if t.wakeupTimer != nil {
		// Latest call wins. Stop returns false if the timer already
		// fired or was already stopped, which is fine — we replace
		// it unconditionally and rely on the fire-id check below to
		// skip any stale firing already in flight.
		t.wakeupTimer.Stop()
	}
	t.wakeupFireID++
	fireID := t.wakeupFireID
	delay := time.Duration(delaySeconds * float64(time.Second))
	t.wakeupTimer = time.AfterFunc(delay, func() {
		t.wakeupMu.Lock()
		stale := fireID != t.wakeupFireID
		t.wakeupMu.Unlock()
		if stale {
			// A later scheduleWakeup superseded us between arming
			// and firing. Don't double-send.
			log.Warn().Uint64("fire_id", fireID).Uint64("live_id", t.wakeupFireID).Msg("ScheduleWakeup timer fired but is stale; skipping")
			return
		}
		log.Info().Uint64("fire_id", fireID).Msg("ScheduleWakeup timer fired; sending prompt as user input")
		if err := t.SendInput(prompt); err != nil {
			log.Error().Err(err).Uint64("fire_id", fireID).Msg("ScheduleWakeup SendInput failed")
		}
	})
	t.wakeupMu.Unlock()

	log.Info().Uint64("fire_id", fireID).Time("fires_at", time.Now().Add(delay)).Msg("ScheduleWakeup intercepted; bot-side timer armed")
}

// cancelWakeupTimer stops any active wakeup timer for this task. Safe
// to call multiple times. Used by the runner-goroutine teardown so a
// dying task doesn't leave a stray timer hanging onto its SendInput
// goroutine.
func (t *RunningTask) cancelWakeupTimer() {
	t.wakeupMu.Lock()
	defer t.wakeupMu.Unlock()
	if t.wakeupTimer != nil {
		t.wakeupTimer.Stop()
		t.wakeupTimer = nil
	}
}

// InputMessage represents a user input message in stream-json format.
type InputMessage struct {
	Type    string           `json:"type"`
	Message InputMessageBody `json:"message"`
}

// InputMessageBody represents the message body for input.
type InputMessageBody struct {
	Role    string              `json:"role"`
	Content []InputContentBlock `json:"content"`
}

// InputContentBlock represents a content block in the input message.
// Supports both text and image content types.
type InputContentBlock struct {
	Type   string       `json:"type"`
	Text   string       `json:"text,omitempty"`
	Source *ImageSource `json:"source,omitempty"`
}

// ImageSource represents the source of an image content block.
type ImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/jpeg", "image/png", etc.
	Data      string `json:"data"`       // Base64 encoded image data
}

// SendInput writes text to the running task's PTY in stream-json format.
func (t *RunningTask) SendInput(text string) error {
	return t.SendInputWithImages(text, nil)
}

// ImageData represents an image to be sent with input.
type ImageData struct {
	MediaType string // e.g., "image/jpeg", "image/png"
	Data      []byte // Raw image bytes
}

// SendInputWithImages writes a stream-json input message to the child's stdin
// pipe.
func (t *RunningTask) SendInputWithImages(text string, images []ImageData) error {
	if t.stdin == nil {
		return oops.New("stdin is closed")
	}

	// Build content blocks - images first, then text.
	var content []InputContentBlock
	for _, img := range images {
		content = append(content, InputContentBlock{
			Type: "image",
			Source: &ImageSource{
				Type:      "base64",
				MediaType: img.MediaType,
				Data:      base64.StdEncoding.EncodeToString(img.Data),
			},
		})
	}
	if text != "" {
		content = append(content, InputContentBlock{Type: "text", Text: text})
	}

	msg := InputMessage{
		Type: "user",
		Message: InputMessageBody{
			Role:    "user",
			Content: content,
		},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return oops.Trace(err)
	}

	// Info-level on purpose: we currently have an open question about
	// whether SendInput is being called more often than expected (the
	// ScheduleWakeup duplicate-fire investigation in May 2026). Promote
	// to Debug once we've root-caused it.
	preview := text
	if len(preview) > 80 {
		preview = preview[:80] + "…"
	}
	t.logger.Info().
		Int("num_images", len(images)).
		Int("json_len", len(data)).
		Int("text_len", len(text)).
		Str("text_preview", preview).
		Msg("sending input to claude")

	if _, err := t.stdin.Write(append(data, '\n')); err != nil {
		return oops.Trace(err)
	}
	// Arm the input-response watchdog. The liveness ticker will
	// warn if no stream event arrives within its threshold. Only
	// arm if not already armed — repeated SendInput without an
	// intervening response (rare but possible for control
	// messages during a running turn) shouldn't reset the clock
	// and hide a wedge.
	t.inputWaitingSince.CompareAndSwap(0, time.Now().UnixNano())
	return nil
}

// Output returns the channel for receiving output chunks.
func (t *RunningTask) Output() <-chan string {
	return t.output
}

// LastStreamAt returns the timestamp of the most recently parsed
// line from claude's stream-json output, or zero if nothing has been
// seen yet. Read by the Home-tab renderer to display a per-session
// freshness indicator without needing to reach into runner internals.
func (t *RunningTask) LastStreamAt() time.Time {
	if v := t.lastStreamAt.Load(); v != nil {
		return v.(time.Time)
	}
	return time.Time{}
}

// PermissionRequests returns the channel for receiving permission requests from the FIFO.
func (t *RunningTask) PermissionRequests() <-chan PermissionRequest {
	if t.permissionFIFO == nil {
		return nil
	}
	return t.permissionFIFO.Requests()
}

// RuntimeDir returns the absolute path to this task's runtime dir
// (where FIFOs, sockets, and embedded bridge binaries live). Used by
// the handler to start / stop the scheduling MCP socket alongside
// the task lifecycle.
func (t *RunningTask) RuntimeDir() string {
	if t.permissionFIFO == nil {
		return ""
	}
	return t.permissionFIFO.RuntimeDir()
}

// TaskPath returns the domain dir the task is running inside. Same
// value handlers.go passed to Runner.Start; re-exposed here so
// scheduling.StartSocket doesn't have to be threaded a duplicate
// parameter.
func (t *RunningTask) TaskPath() string {
	return t.taskPath
}

// SendPermissionResponse sends a response to a permission request.
func (t *RunningTask) SendPermissionResponse(resp PermissionResponse) {
	if t.permissionFIFO == nil {
		t.logger.Warn().
			Str("behavior", resp.Behavior).
			Msg("SendPermissionResponse called with nil permissionFIFO; dropping silently would strand claude")
		return
	}
	t.permissionFIFO.SendResponse(resp)
}

// ControlPermissionRequests returns the channel for receiving permission requests
// via control messages (newer protocol).
func (t *RunningTask) ControlPermissionRequests() <-chan PermissionRequest {
	return t.controlPermissionRequests
}

// SendControlResponse sends a control_response for permission requests.
func (t *RunningTask) SendControlResponse(requestID, behavior, message string) error {
	if t.stdin == nil {
		return oops.New("stdin is closed")
	}

	resp := ControlResponse{
		Type:      "control_response",
		RequestID: requestID,
		Behavior:  behavior,
		Message:   message,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		return oops.Trace(err)
	}

	t.logger.Debug().
		Str("request_id", requestID).
		Str("behavior", behavior).
		Msg("sending control_response")

	_, err = t.stdin.Write(append(data, '\n'))
	return oops.Trace(err)
}

// Done returns the channel that receives the final result.
func (t *RunningTask) Done() <-chan *Result {
	return t.done
}

// Cancel cancels the running task without giving the agent a chance
// to save state. Prefer Shutdown for ordinary stop/restart flows;
// reserve Cancel for paths where the process is already known dead
// or no save-state attempt is wanted (e.g. crashed-out cleanup).
func (t *RunningTask) Cancel() {
	if t.cancel != nil {
		t.cancel()
	}
}

// DefaultSaveStateMessage is the message Shutdown sends to the agent
// before forcing a kill. It mirrors the ephemeral-container guidance
// already in the system prompt (save progress notes / state under
// cwd) so the agent has a clear cue to flush in-flight work to disk.
const DefaultSaveStateMessage = "We are shutting down this task. Please immediately save any in-progress notes, progress markers, or state you'd want on resume into files under the current working directory (avoid `.clod/`, which the wrapper owns). Then stop. You have a limited grace period before forced shutdown."

// Shutdown attempts a graceful stop:
//
//  1. Send `saveStateMsg` as a stream-json input — the agent
//     processes it as the next user turn and gets a chance to
//     persist anything important.
//  2. Close the bot's end of stdin so claude sees EOF after that
//     turn and exits cleanly (without EOF, claude's stream-json
//     loop blocks on stdin even after the result is emitted).
//  3. Wait up to `gracePeriod` for the runner goroutine to finish
//     (Done()).
//  4. On timeout — or any send error — fall through to forceKill:
//     SIGKILL the process group + `docker stop` by runtime suffix.
//
// `ctx` lets the caller cancel the wait early (e.g. a second
// shutdown signal). Returns true when the agent exited within the
// grace period, false when the kill path was taken.
func (t *RunningTask) Shutdown(ctx context.Context, saveStateMsg string, gracePeriod time.Duration) bool {
	log := t.logger.With().Dur("grace_period", gracePeriod).Logger()
	log.Info().Msg("graceful shutdown requested")

	if err := t.SendInput(saveStateMsg); err != nil {
		log.Warn().Err(err).Msg("save-state SendInput failed; force-killing")
		t.forceKill()
		return false
	}
	t.closeStdin()

	timer := time.NewTimer(gracePeriod)
	defer timer.Stop()
	select {
	case <-t.done:
		log.Info().Msg("task exited gracefully within grace period")
		// Belt-and-suspenders: `docker run --rm` should already have
		// removed the container when its main process exited, but
		// don't take that on faith. A redundant stop is cheap
		// (no-op on an already-cleaned container) and closes the
		// "session closed but container still running" hole for
		// daemon-delay / --rm-failure edge cases. See stopContainer.
		t.stopContainer("graceful path")
		return true
	case <-timer.C:
		log.Warn().Msg("grace period expired; forcing")
	case <-ctx.Done():
		log.Warn().Err(ctx.Err()).Msg("shutdown context cancelled before grace; forcing")
	}
	t.forceKill()
	return false
}

// forceKill takes the host process tree down and ensures the docker
// container goes with it. Two paths because docker has two failure
// modes:
//
//  1. SIGKILL to the bash wrapper's process group catches the
//     `docker run` foreground client alongside it, and `--rm` fires
//     when the client dies cleanly. This handles the common case.
//  2. `docker stop` by runtime-suffix lookup catches the cases
//     SIGKILL can't: `docker run` already init-orphaned (the bash
//     parent died earlier), or the docker daemon holding the
//     container alive past its shim's SIGKILL'd death (the daemon
//     never received a stop request — SIGKILL'd clients can't send
//     one).
func (t *RunningTask) forceKill() {
	// Process-group SIGKILL. Setpgid was set in Start so the cmd
	// is its own process-group leader; SIGKILL'ing that group
	// catches bash + docker run + descendants together. Safety
	// check: only kill the group when its pgid differs from the
	// bot's own pgid. They match if Setpgid wasn't set (or for
	// tasks that pre-date this code), and SIGKILL'ing the bot's
	// own group would suicide the bot.
	if t.cmd != nil && t.cmd.Process != nil {
		cmdPgid, err1 := syscall.Getpgid(t.cmd.Process.Pid)
		myPgid, err2 := syscall.Getpgid(os.Getpid())
		switch {
		case err1 != nil:
			t.logger.Debug().Err(err1).Msg("getpgid(cmd) failed; skipping group kill")
		case err2 == nil && cmdPgid == myPgid:
			t.logger.Warn().Int("pgid", cmdPgid).Msg("cmd shares bot's process group (Setpgid was not set); skipping group kill to avoid suicide")
		default:
			if err := syscall.Kill(-cmdPgid, syscall.SIGKILL); err != nil {
				t.logger.Debug().Err(err).Int("pgid", cmdPgid).Msg("kill -pgid SIGKILL failed (probably already dead)")
			} else {
				t.logger.Info().Int("pgid", cmdPgid).Msg("SIGKILL sent to process group")
			}
		}
	}
	// Always run the runCtx cancel too — it propagates timeouts
	// upstream and unblocks any goroutine waiting on runCtx.Done().
	if t.cancel != nil {
		t.cancel()
	}

	// Stop the container directly. Delegates to stopContainer so the
	// graceful path can call the same logic — see stopContainer's doc
	// for why we don't rely on `docker run --rm` autocleanup alone.
	t.stopContainer("force path")
}

// stopContainer explicitly `docker stop`s the session's container by
// runtime-suffix filter. Called from both Shutdown branches:
//
//   - Graceful path: `docker run --rm` should have already removed
//     the container when its main process exited, but daemon delays,
//     stuck processes, or `--rm` failures leave orphans; a redundant
//     `docker stop` here is cheap (no-op when the container is
//     already gone) and closes the "session closed but container
//     still up" hole.
//   - Force path: after SIGKILL'ing the process group the docker
//     client may have died before it could tell the daemon anything,
//     so the daemon still owns the container. Explicit stop by name
//     brings it down.
//
// Tolerates every failure mode: claude-direct mode (no suffix),
// container already gone, docker daemon unreachable. `pathLabel`
// tags log entries so it's clear which branch triggered the call.
func (t *RunningTask) stopContainer(pathLabel string) {
	if t.runtimeSuffix == "" {
		return
	}
	// Filter by container name suffix. The bash wrapper builds names
	// as `clod-<task>-<id>-<suffix>`, so `name=<suffix>$` (regex tail
	// anchor) hits exactly our container. Short timeout so a hung
	// daemon doesn't stall shutdown.
	psCtx, psCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer psCancel()
	psOut, err := exec.CommandContext(psCtx, "docker", "ps", "-q", "--filter", "name="+t.runtimeSuffix+"$").Output()
	if err != nil {
		t.logger.Debug().Err(err).Str("suffix", t.runtimeSuffix).Str("path", pathLabel).Msg("docker ps lookup failed; skipping explicit container stop")
		return
	}
	cids := strings.Fields(string(psOut))
	if len(cids) == 0 {
		t.logger.Debug().Str("suffix", t.runtimeSuffix).Str("path", pathLabel).Msg("no running container matched runtime suffix")
		return
	}
	for _, cid := range cids {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := exec.CommandContext(stopCtx, "docker", "stop", "-t", "5", cid).Run()
		stopCancel()
		if err != nil {
			t.logger.Warn().Err(err).Str("cid", cid).Str("path", pathLabel).Msg("docker stop failed")
		} else {
			t.logger.Info().Str("cid", cid).Str("path", pathLabel).Msg("docker stop ok")
		}
	}
}

// SessionID returns the session ID once captured.
func (t *RunningTask) GetSessionID() string {
	return t.sessionID
}

// SessionIDCaptured returns a channel that receives the session ID exactly
// once, as soon as the stream parser observes it. Callers use this to persist
// the session mapping early so a bot restart mid-task doesn't orphan the
// thread.
func (t *RunningTask) SessionIDCaptured() <-chan string {
	return t.sessionIDCaptured
}

// notifySessionID fires the one-shot notification. Safe to call repeatedly;
// only the first call has any effect.
func (t *RunningTask) notifySessionID(id string) {
	t.sessionIDOnce.Do(func() {
		select {
		case t.sessionIDCaptured <- id:
		default:
		}
	})
}

// readAllowedTools reads the allowed tools from the task's claude.json config.
// It also checks for a permissions.allow array in the project config (Claude's native format).
func readAllowedTools(taskPath string, logger zerolog.Logger) []string {
	configPath := filepath.Join(taskPath, ".clod", "claude", "claude.json")

	logger.Info().
		Str("task_path", taskPath).
		Str("config_path", configPath).
		Msg("reading allowed tools from claude.json")

	data, err := os.ReadFile(configPath)
	if err != nil {
		logger.Warn().Err(err).Str("path", configPath).Msg("failed to read claude.json")
		return nil
	}

	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		logger.Warn().Err(err).Msg("failed to parse claude.json")
		return nil
	}

	projects, ok := config["projects"].(map[string]any)
	if !ok {
		logger.Info().Msg("no projects key in claude.json")
		return nil
	}

	// Log available project keys for debugging
	var projectKeys []string
	for k := range projects {
		projectKeys = append(projectKeys, k)
	}
	logger.Info().
		Str("task_path", taskPath).
		Strs("project_keys", projectKeys).
		Msg("looking for project in claude.json")

	project, ok := projects[taskPath].(map[string]any)
	if !ok {
		logger.Warn().
			Str("task_path", taskPath).
			Strs("available_keys", projectKeys).
			Msg("project not found in claude.json - task_path doesn't match any project key")
		return nil
	}

	var tools []string

	// Check for allowedTools (bot's format)
	if allowedTools, ok := project["allowedTools"].([]any); ok {
		logger.Info().Int("count", len(allowedTools)).Msg("found allowedTools array")
		for _, t := range allowedTools {
			if s, ok := t.(string); ok {
				tools = append(tools, s)
			}
		}
	} else {
		logger.Info().Msg("no allowedTools array in project")
	}

	// Also check for permissions.allow (Claude's native format)
	if permissions, ok := project["permissions"].(map[string]any); ok {
		if allow, ok := permissions["allow"].([]any); ok {
			logger.Info().Int("count", len(allow)).Msg("found permissions.allow array")
			for _, t := range allow {
				if s, ok := t.(string); ok {
					tools = append(tools, s)
				}
			}
		} else {
			logger.Info().Msg("no permissions.allow array in project")
		}
	} else {
		logger.Info().Msg("no permissions map in project")
	}

	// Deduplicate the user-saved entries; the runtime caller in
	// Start() unions these with builtinAlwaysAllowedTools so the
	// final --allowedTools list always includes the task-lifecycle
	// helpers even when claude.json has nothing saved yet.
	seen := make(map[string]bool)
	var uniqueTools []string
	for _, t := range tools {
		if !seen[t] {
			seen[t] = true
			uniqueTools = append(uniqueTools, t)
		}
	}

	logger.Info().
		Str("task_path", taskPath).
		Strs("tools", uniqueTools).
		Int("count", len(uniqueTools)).
		Msg("read allowed tools from claude.json")

	return uniqueTools
}

// builtinAlwaysAllowedTools are unioned into every task's allowed-
// tools list at runtime. They're internal claude-code task-harness
// tools that have no business going through the permission FIFO
// every time — and without them the agent can start background
// processes with Monitor but has no way to stop, inspect, or list
// them, so the bot's active-monitor count grows monotonically until
// the next session resume's ClearMonitors. Listed here rather than
// written into claude.json so the union is computed every start;
// bumping this list takes effect for in-flight tasks on their next
// runClod invocation without touching saved state.
var builtinAlwaysAllowedTools = []string{
	"TaskStop",
	"TaskGet",
	"TaskList",
	"TaskOutput",
}

// mergeBuiltinAllowedTools appends the builtin task-harness tools
// to whatever the per-task config supplied, deduplicating while it
// goes. Always returns a non-empty slice; if the per-task config
// has nothing saved, the result is just the builtins.
func mergeBuiltinAllowedTools(perTask []string) []string {
	seen := make(map[string]bool, len(perTask)+len(builtinAlwaysAllowedTools))
	out := make([]string, 0, len(perTask)+len(builtinAlwaysAllowedTools))
	for _, t := range perTask {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, t := range builtinAlwaysAllowedTools {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// Start begins executing clod in a task directory with the given prompt.
// If sessionID is provided, it resumes an existing session.
// If model is non-empty, it's passed through as claude's --model flag (e.g.
// "opus", "sonnet", or a full model id). Mid-session model switching isn't
// supported by claude --input-format stream-json, but --resume honors a
// changed --model on the next start.
// If permissionMode is non-empty and not "default", it overrides the
// Runner's configured default for this invocation — used to turn plan
// mode on/off per-thread without spinning up a new Runner. Like --model,
// this takes effect at process-start only; switches across turns require
// the next Start() call (typically via resume-after-finish).
// If useClaudeDirect is true, `claude` is invoked directly on the host
// instead of going through `clod` (which bind-mounts into a docker
// container). Per-task state isolation is preserved by redirecting HOME
// to `<taskPath>/.clod/direct-home`, which symlinks `.claude` and
// `.claude.json` into the same `.clod/claude/` tree clod uses.
// Returns a RunningTask that can be used to send input and receive output.
func (r *Runner) Start(
	ctx context.Context,
	taskPath, prompt, sessionID, model, permissionMode string,
	useClaudeDirect bool,
) (*RunningTask, error) {
	// Create command with timeout context. runStart is captured so the
	// error-return path below can report actual runtime, not just the
	// nominal deadline — a container that outlives its ctx deadline
	// (e.g. `docker run` orphaned from bash on ctx cancel) can push
	// cmd.Wait() to return hours past the deadline, and reporting
	// only "timed out after 24h" hides that gap.
	runCtx, cancel := context.WithTimeout(ctx, r.timeout)
	runStart := time.Now()

	// Create permission FIFO for MCP communication (must be done before building args).
	// Pass empty string to generate a unique runtime suffix for concurrent instances.
	permFIFO, err := NewPermissionFIFO(taskPath, "", r.domainReadmePath, r.workspaceReadmePath, r.logger)
	if err != nil {
		cancel()
		return nil, oops.Trace(err)
	}

	// Create MCP config for permission server.
	mcpConfigPath, permToolName, err := permFIFO.CreateMCPConfig()
	if err != nil {
		cancel()
		permFIFO.Close()
		return nil, oops.Trace(err)
	}

	// Build command arguments.
	// Use stream-json for both input and output to enable bidirectional communication.
	// The initial prompt is sent via stream-json input after the process starts
	// (not as a CLI arg) since --input-format stream-json requires stdin input.
	args := []string{
		"-p",
		"--output-format",
		"stream-json",
		"--input-format",
		"stream-json",
		"--include-partial-messages", // Enable streaming deltas for responsive output.
		"--verbose",
		"--mcp-config",
		mcpConfigPath,
		"--permission-prompt-tool",
		permToolName,
	}

	// Inline the combined onboarding context (runtime notice + workspace
	// + domain READMEs) via --append-system-prompt-file. The file form
	// sidesteps the OS argv length cap that --append-system-prompt
	// would hit on long READMEs. We deliberately do NOT also pass
	// --append-system-prompt: claude rejects the combination outright
	// ("Cannot use both --append-system-prompt and
	// --append-system-prompt-file"), so the runtime notice has to live
	// inside the file alongside the user-authored sections (built by
	// NewPermissionFIFO). An empty ContextPath() means even the
	// runtime notice came back blank, so the flag is omitted.
	if ctxPath := permFIFO.ContextPath(); ctxPath != "" {
		args = append(args, "--append-system-prompt-file", ctxPath)
	}

	// Pass any saved allowed tools — plus the builtin task-harness
	// helpers (TaskStop / TaskGet / TaskList / TaskOutput) — so
	// they're available immediately. The builtins are merged at
	// runtime rather than written into claude.json, so a bump to
	// the builtin list takes effect on the next start without
	// touching saved state.
	allowedTools := mergeBuiltinAllowedTools(readAllowedTools(taskPath, r.logger))
	for _, tool := range allowedTools {
		args = append(args, "--allowedTools", tool)
	}
	r.logger.Info().
		Strs("allowed_tools", allowedTools).
		Int("count", len(allowedTools)).
		Msg("passing allowed tools to claude (saved + builtins)")

	// Per-call permissionMode beats the Runner-wide default so callers can
	// flip plan mode on/off per thread. Empty falls through to the Runner's
	// configured value; either "" or "default" produces no flag.
	effectivePermissionMode := permissionMode
	if effectivePermissionMode == "" {
		effectivePermissionMode = r.permissionMode
	}
	if effectivePermissionMode != "" && effectivePermissionMode != "default" {
		args = append(args, "--permission-mode", effectivePermissionMode)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if model != "" {
		args = append(args, "--model", model)
	}

	exe := "clod"
	if useClaudeDirect {
		exe = "claude"
	}

	r.logger.Debug().
		Str("task_path", taskPath).
		Str("session_id", sessionID).
		Str("model", model).
		Strs("args", args).
		Str("exe", exe).
		Bool("claude_direct", useClaudeDirect).
		Msg("starting runtime with pty")

	//nolint:gosec
	cmd := exec.CommandContext(runCtx, exe, args...)
	cmd.Dir = taskPath
	// Setpgid puts the bash wrapper + its docker-run child + every
	// other descendant in the same process group, so a SIGKILL to
	// -pgid takes the whole tree down at once. Without this, Go's
	// exec.CommandContext SIGKILLs only the bash PID; `docker run`
	// gets reparented to init and keeps the container alive
	// indefinitely (and `--rm` never fires because docker-run is
	// the foreground holder, not the container's PID 1). The
	// process-group kill is paired with an explicit `docker stop`
	// in forceKill — belt and suspenders, since SIGKILL'ing
	// docker-run still leaves the daemon-managed container running
	// (the daemon never gets a stop request from a SIGKILL'd client).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Set MCP tool timeout generously so permission prompts waiting on a
	// human in Slack don't fail with "technical issues" when the approver
	// steps away. 24h = 86_400_000 ms — long enough for overnight pauses,
	// short enough to still eventually give up. Caller can override via
	// the MCP_TOOL_TIMEOUT env var on the bot process.
	// CLOD_RUNTIME_DIR is constructed by the run script from CLOD_RUNTIME_SUFFIX.
	const defaultMCPToolTimeoutMS = "86400000"
	mcpToolTimeout := os.Getenv("MCP_TOOL_TIMEOUT")
	if mcpToolTimeout == "" {
		mcpToolTimeout = defaultMCPToolTimeoutMS
	}
	cmd.Env = append(os.Environ(),
		"MCP_TOOL_TIMEOUT="+mcpToolTimeout,
		"CLOD_RUNTIME_SUFFIX="+permFIFO.RuntimeSuffix(),
		"CLOD_CONCURRENT=true",
		"CLOD_NONINTERACTIVE=true",
	)

	// In claude-direct mode, isolate per-task state by redirecting HOME
	// to `<taskPath>/.clod/direct-home/`. The directory is materialized
	// lazily with symlinks into `.clod/claude/` so claude reads/writes
	// the same `~/.claude/` + `~/.claude.json` tree clod would have
	// bind-mounted inside the container. Without this, every task would
	// share the bot process's real HOME and stomp each other's state.
	if useClaudeDirect {
		homeDir, herr := prepareDirectHome(taskPath)
		if herr != nil {
			cancel()
			permFIFO.Close()
			return nil, oops.Trace(herr)
		}
		cmd.Env = append(cmd.Env, "HOME="+homeDir)
		r.logger.Debug().Str("HOME", homeDir).Msg("claude-direct home redirected")
	}

	r.logger.Debug().
		Str("MCP_TOOL_TIMEOUT", mcpToolTimeout).
		Str("CLOD_RUNTIME_SUFFIX", permFIFO.RuntimeSuffix()).
		Bool("CLOD_CONCURRENT", true).
		Bool("CLOD_NONINTERACTIVE", true).
		Msg("setting environment variables for clod run")

	// Wire up the child's three standard streams with three different
	// transports, each chosen for its role:
	//
	//   stdin  — plain pipe. The bot writes stream-json messages on this,
	//            which flow through `docker run -i` straight into claude's
	//            stream-json reader. A PTY is wrong here: its line discipline
	//            would echo input, enforce MAX_CANON line length, and
	//            interpret ^C/^D — none of that belongs in a JSON transport.
	//
	//   stdout — PTY slave. This is claude's stream-json output channel. A
	//            TTY on stdout keeps any TTY-sniffing tooling inside the
	//            wrapper (ssh-add, tput, etc.) happy AND keeps docker's
	//            buildkit from flipping into some weird "I have no terminal"
	//            fallback.
	//
	//   stderr — plain pipe. All wrapper chatter (upgrade banners, docker
	//            build progress, [clod] SSH agent lifecycle) lands here and
	//            gets logged at debug. Keeping it off the stdout PTY is what
	//            lets stdout stay pure stream-json.
	//
	// pty.Start() would have bound stdin and stdout to the same PTY and
	// assumed stderr too, so we do the setup manually.
	ptmx, tty, err := pty.Open()
	if err != nil {
		cancel()
		permFIFO.Close()
		return nil, oops.Trace(err)
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		cancel()
		_ = ptmx.Close()
		_ = tty.Close()
		permFIFO.Close()
		return nil, oops.Trace(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		cancel()
		_ = ptmx.Close()
		_ = tty.Close()
		_ = stdinR.Close()
		_ = stdinW.Close()
		permFIFO.Close()
		return nil, oops.Trace(err)
	}

	cmd.Stdin = stdinR
	cmd.Stdout = tty
	cmd.Stderr = stderrW
	// Setsid puts the child in its own session (clean signal group).
	// Setctty + Ctty=1 makes the stdout tty the child's controlling terminal
	// — we can't use the default Ctty=0 because stdin is a pipe, not a TTY.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    1,
	}

	if err := cmd.Start(); err != nil {
		cancel()
		_ = ptmx.Close()
		_ = tty.Close()
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		permFIFO.Close()
		return nil, oops.Trace(err)
	}

	// The child inherited its own copies of tty, stdinR, and stderrW. Close
	// our copies so EOF propagates correctly when the child exits and so we
	// don't leak fds.
	_ = tty.Close()
	_ = stdinR.Close()
	_ = stderrW.Close()

	// Drain stderr in the background. These lines are diagnostics from the
	// wrapper — never stream-json — so we log them at debug and keep the
	// most recent handful in a bounded tail buffer. The tail is appended to
	// the final error message so the user sees *why* clod failed (SSH agent
	// missing, docker build error, login required, etc.) instead of a bare
	// "exit status 1".
	var stderrMu sync.Mutex
	const maxStderrTailLines = 40
	stderrTail := make([]string, 0, maxStderrTailLines)
	// taskForProgress is captured below once RunningTask is built. We can't
	// reference it here directly because it's constructed after the drain
	// goroutine starts; a pointer-to-pointer lets the goroutine read the
	// task once it's available.
	var taskPtr **RunningTask
	{
		var t *RunningTask
		taskPtr = &t
	}
	go func() {
		defer func() { _ = stderrR.Close() }()
		scanner := bufio.NewScanner(stderrR)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			stderrMu.Lock()
			if len(stderrTail) >= maxStderrTailLines {
				stderrTail = stderrTail[1:]
			}
			stderrTail = append(stderrTail, line)
			stderrMu.Unlock()
			// Bridge-emitted diagnostic lines land at Info —
			// they're the in-container half of the FIFO handshake
			// (permbridge) and the socket handshake (schedbridge),
			// and the only way to debug a deadlock or a flapping
			// MCP subprocess after the fact. Symmetric treatment
			// for both bridges; prior to 0.36.9 only permbridge
			// was promoted and schedbridge stderr was invisible
			// in default logs, which made the 2026-07-10 flapping
			// investigation harder than it needed to be.
			// Everything else (docker build chatter, SSH agent
			// banners) stays at debug to keep the log readable.
			if strings.HasPrefix(line, "[permbridge]") {
				r.logger.Info().
					Str("stderr", line).
					Msg("permbridge stderr")
			} else if strings.HasPrefix(line, "[schedbridge]") {
				r.logger.Info().
					Str("stderr", line).
					Msg("schedbridge stderr")
			} else if strings.HasPrefix(line, "[clodproxy]") || strings.HasPrefix(line, "[wrapper]") {
				r.logger.Info().
					Str("stderr", line).
					Msg("container helper stderr")
			} else {
				r.logger.Debug().
					Str("stderr", line).
					Msg("clod wrapper stderr")
			}

			// Forward selected lines as progress so the user can see
			// the container is being prepared (docker build + SSH agent
			// setup can take 1–3 minutes before claude is ready).
			if progressStderrPattern.MatchString(line) {
				if t := *taskPtr; t != nil {
					select {
					case t.output <- "__PROGRESS__" + line:
					default:
						// Output channel full; drop rather than block
						// the stderr drain.
					}
				}
			}
		}
	}()
	snapshotStderrTail := func() string {
		stderrMu.Lock()
		defer stderrMu.Unlock()
		joined := strings.Join(stderrTail, "\n")
		// Cap the tail so the formatted error still fits inside a
		// Slack section block (~3000-char text limit). We keep the
		// most-recent bytes — that's where the actual failure usually
		// surfaces; the leading docker-build steps are noise once
		// they've run. Without this, long preludes (e.g. SSH agent
		// known-hosts dumps) push the opening ``` of the formatted
		// error past Slack's truncation, leaving the user staring at
		// an unfenced wall of text.
		const maxTailBytes = 2200
		if len(joined) > maxTailBytes {
			joined = "…(earlier output trimmed)…\n" + joined[len(joined)-maxTailBytes:]
		}
		return joined
	}

	task := &RunningTask{
		cmd:                       cmd,
		pty:                       ptmx,
		stdin:                     stdinW,
		output:                    make(chan string, 100),
		done:                      make(chan *Result, 1),
		cancel:                    cancel,
		sessionID:                 sessionID,
		taskPath:                  taskPath,
		logger:                    r.logger,
		permissionFIFO:            permFIFO,
		controlPermissionRequests: make(chan PermissionRequest, 10),
		sessionIDCaptured:         make(chan string, 1),
		runtimeSuffix:             permFIFO.RuntimeSuffix(),
	}
	// Publish the task so the stderr drain can forward progress lines.
	*taskPtr = task
	// When resuming an existing session, the caller already has the mapping,
	// but fire the notification anyway so the save-on-first-observation path
	// is uniform and idempotent.
	if sessionID != "" {
		task.notifySessionID(sessionID)
	}

	// Watch runCtx.Done and stop the container as soon as our ctx
	// says "stop", NOT after cmd.Wait returns. The v0.36.4 attempt
	// placed the stopContainer call after cmd.Wait, which turned out
	// to be a no-op: exec.CommandContext SIGKILLs bash on ctx.Done,
	// but its docker-run child gets orphaned to init and keeps the
	// stdout/stderr pipes open. cmd.Wait blocks on those pipes
	// closing, which happens only when docker run exits, which
	// happens only when the container exits, which we're supposed to
	// be causing here — chicken and egg. See 2026-07-09 eagle
	// post-mortem: deadline fired at 01:15:39Z, "docker stop
	// (runctx path)" didn't log until 21:35:17Z — a 20h gap where
	// cmd.Wait was blocked on pipes held open by an orphaned docker
	// run.
	//
	// Watcher exits when runCtx.Done fires (either via WithTimeout
	// deadline, Shutdown's forceKill -> cancel, or parent-ctx
	// propagation). No task-done path — stopContainer is a no-op on
	// an already-gone container, so if the run completed cleanly
	// and the watcher fires later during bot shutdown it just
	// silently confirms the container is gone.
	go func() {
		<-runCtx.Done()
		task.logger.Info().
			Err(runCtx.Err()).
			Str("suffix", task.runtimeSuffix).
			Msg("runCtx.Done fired — stopping container without waiting for cmd.Wait")
		task.stopContainer("runctx-watcher")
	}()

	// Liveness ticker: watches lastPingAt (updated by the SSE `ping`
	// case) and toggles __ALIVE__ / __STALE__ sentinels on the output
	// channel when the freshness state flips. The handler translates
	// those into an add/remove of a liveness reaction on the anchor
	// message. Design rationale:
	//
	//   - content_block_delta events can go 2–3 hours silent in
	//     healthy long-running sessions (eagle model-training work),
	//     so a heartbeat driven by deltas false-positives constantly.
	//   - Anthropic sends pings every ~15–20s to keep the SSE alive
	//     regardless of what the model is doing, so ping freshness
	//     is a stable signal.
	//   - Fresh threshold (90s) gives headroom over the ~20s ping
	//     interval so a single dropped ping doesn't flip the state;
	//     stale threshold (120s) adds hysteresis so we don't churn
	//     add/remove on a marginal connection.
	//   - Runner owns the state machine so the handler only sees
	//     transitions (one __ALIVE__ per becoming-fresh, one
	//     __STALE__ per becoming-stale) — no repeated reaction API
	//     hits.
	livenessDone := make(chan struct{})
	task.livenessDone = livenessDone
	go func() {
		defer close(livenessDone)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		var alive bool
		// inputWedgeWarnedAt tracks the input-arming timestamp we
		// most recently warned about, so we don't spam the same
		// wedge every tick. A fresh SendInput resets
		// inputWaitingSince to a new nanosecond value, which
		// distinguishes it from the one we already warned about.
		var inputWedgeWarnedFor int64
		// Sends are gated on runCtx to avoid racing the outer
		// goroutine's `close(task.output)`. The outer goroutine's
		// defer chain cancels runCtx first, then waits on
		// livenessDone before allowing close(task.output) to run —
		// so once we return via <-runCtx.Done() here, the outer
		// goroutine unblocks and closes task.output. Never send
		// after ctx cancels.
		send := func(msg string) {
			select {
			case task.output <- msg:
			case <-runCtx.Done():
			}
		}
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				var lastAt time.Time
				if v := task.lastStreamAt.Load(); v != nil {
					lastAt = v.(time.Time)
				}
				// Undefined lastAt means no line observed yet;
				// treat as not-fresh so we don't add the liveness
				// reaction before we've seen anything.
				since := time.Since(lastAt)
				fresh := !lastAt.IsZero() && since < 60*time.Second
				stale := lastAt.IsZero() || since > 90*time.Second
				// Per-tick state at Debug (fires every 15s per
				// session, would flood the log at Info). Transitions
				// stay at Info because they are rare and useful for
				// forensics of any wedge investigation.
				r.logger.Debug().
					Time("last_stream_at", lastAt).
					Dur("since", since).
					Bool("fresh", fresh).
					Bool("stale", stale).
					Bool("alive_prev", alive).
					Msg("liveness tick")
				if fresh && !alive {
					alive = true
					r.logger.Info().Dur("since_last_stream", since).Msg("liveness transition -> ALIVE")
					send("__ALIVE__")
				} else if stale && alive {
					alive = false
					r.logger.Info().Dur("since_last_stream", since).Msg("liveness transition -> STALE")
					send("__STALE__")
				}
				// Input-response watchdog: SendInput → stream event
				// should be near-instant (1–5s typical). If it's
				// been >60s and we haven't warned for THIS input
				// event yet, warn. This catches the 2026-07-13
				// wedge class where claude receives stdin but its
				// Node event loop never composes a new HTTP
				// request — invisible to the HTTP-layer probes,
				// visible here.
				armed := task.inputWaitingSince.Load()
				if armed != 0 && armed != inputWedgeWarnedFor {
					waited := time.Since(time.Unix(0, armed))
					if waited > 60*time.Second {
						r.logger.Warn().
							Dur("waited", waited).
							Msg("claude did not respond to input within 60s — likely internal event-loop wedge (upstream claude-code #54434 class)")
						inputWedgeWarnedFor = armed
						// Signal the handler to auto-restart this
						// session. Handler applies a per-session
						// cooldown so a tight restart loop can't
						// happen. Send is bounded by runCtx via
						// the same `send` helper that gates the
						// ALIVE/STALE sentinels.
						send("__WEDGE_AUTO_RESTART__")
					}
				}
			}
		}
	}()

	// Start permission FIFO listener
	permFIFO.Start(runCtx)

	// Send the initial prompt now. It's safe to push it into the stdin pipe
	// before claude exists — kernel pipe buffer holds up to 64KiB, and once
	// `docker run -i` starts forwarding stdin into the container, claude
	// drains it. Deferring until we see claude's system init would deadlock:
	// in `-p --input-format stream-json` claude does NOT emit anything
	// before it has read its first stream-json message, so bot-waits-for-init
	// + claude-waits-for-input is a mutual stare-off.
	if prompt != "" {
		if err := task.SendInput(prompt); err != nil {
			cancel()
			_ = ptmx.Close()
			_ = stdinW.Close()
			_ = stderrR.Close()
			permFIFO.Close()
			return nil, oops.Trace(err)
		}
	}

	// Read from PTY and parse stream-json in background
	go func() {
		defer close(task.output)
		// Wait for the liveness ticker to fully exit before letting
		// the close(task.output) defer above run. The ticker sends
		// __ALIVE__/__STALE__ into task.output; without this
		// barrier a slow ticker could still be in-flight when the
		// close fires and panic. Registered after close(task.output)
		// so it runs FIRST (LIFO). livenessDone is closed by the
		// ticker on exit; nil check tolerates test paths that skip
		// the ticker setup.
		defer func() {
			if task.livenessDone != nil {
				<-task.livenessDone
			}
		}()
		defer close(task.done)
		defer func() { _ = ptmx.Close() }()
		defer task.closeStdin()
		defer task.cancelWakeupTimer()
		defer permFIFO.Close()
		// Cancel runCtx on ANY exit path so goroutines observing
		// ctx.Done (liveness ticker, stopContainer watcher) shut
		// down promptly. Registered LAST here so it runs FIRST in
		// the defer chain (LIFO) — before the livenessDone wait
		// above. Also fixes a pre-existing minor leak where
		// clean-exit runs left runCtx uncancelled until parent-ctx
		// propagation.
		defer cancel()

		var outputBuilder strings.Builder
		// Track tool_use IDs to their names and inputs so we can show context in results.
		type toolInfo struct {
			Name  string
			Input map[string]any
		}
		toolInfos := make(map[string]toolInfo)
		scanner := bufio.NewScanner(ptmx)
		// Increase buffer size for long lines
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		// handshaken flips to true once we parse our first valid stream-json
		// message (normally claude's system init). Before the handshake we
		// silently tolerate non-JSON on stdout — the wrapper *shouldn't* be
		// writing there anymore, but we stay permissive in case any stray
		// bytes leak through (old .clod scripts on disk, PTY echo of our own
		// input, etc.). After the handshake, stdout is supposed to be pure
		// stream-json, so anything non-JSON is a real protocol violation and
		// gets logged loudly.
		handshaken := false

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			// Check if this is a wrapped stream_event and unwrap it.
			var wrapper StreamEventWrapper
			if err := json.Unmarshal([]byte(line), &wrapper); err == nil && wrapper.Type == "stream_event" {
				r.logger.Info().
					Str("unwrapped_event_preview", string(wrapper.Event)[:min(150, len(wrapper.Event))]).
					Msg("unwrapped stream_event wrapper")
				line = string(wrapper.Event)
			}

			var msg StreamMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				if handshaken {
					r.logger.Warn().
						Str("line", line).
						Err(err).
						Msg("non-JSON line on stdout after stream-json handshake (protocol violation)")
				} else {
					r.logger.Debug().
						Str("line", line).
						Err(err).
						Msg("pre-handshake non-JSON line on stdout, skipping")
				}
				continue
			}
			handshaken = true

			// Liveness signal: every parsed line proves the model
			// (or claude itself) is currently producing output.
			// The ticker uses this to toggle the __ALIVE__/__STALE__
			// reaction. See RunningTask.lastStreamAt for why this
			// is bumped on every line rather than a periodic
			// keep-alive (there isn't one in stream-json).
			task.lastStreamAt.Store(time.Now())

			// Clear the input-response watchdog. A stream event
			// arriving from claude means it processed some stdin
			// input; we're no longer waiting on a first-response
			// to a specific SendInput call. If a follow-up
			// SendInput happens without an intervening response
			// it will re-arm.
			if prev := task.inputWaitingSince.Swap(0); prev != 0 {
				elapsed := time.Since(time.Unix(0, prev))
				r.logger.Info().
					Dur("since_input", elapsed).
					Str("type", msg.Type).
					Msg("input-response watchdog disarmed by stream event")
			}

			// Extract session ID if present
			if msg.SessionID != "" && task.sessionID == "" {
				task.sessionID = msg.SessionID
				r.logger.Debug().
					Str("session_id", task.sessionID).
					Msg("captured session ID")
				task.notifySessionID(msg.SessionID)
			}

			// Handle different message types.
			r.logger.Info().
				Str("type", msg.Type).
				Str("subtype", msg.Subtype).
				Msg("processing stream message")

			switch msg.Type {
			case "system":
				// System messages include init with session_id.
				if msg.Subtype == "init" && msg.SessionID != "" {
					task.sessionID = msg.SessionID
					r.logger.Debug().
						Str("session_id", task.sessionID).
						Msg("captured session ID from system init")
					task.notifySessionID(msg.SessionID)
				}
				// Compaction boundary: claude signals that it's
				// summarizing history to reduce context. Nothing
				// visible flows during compaction, so from Slack the
				// gap looks identical to a wedge. Surface a
				// __COMPACT_START__ sentinel and let the handler
				// post a rolling "compacting session…" banner. The
				// paired __COMPACT_END__ fires on the next
				// content_block_start (real work resuming).
				if msg.Subtype == "compact_boundary" {
					if task.compactPending.CompareAndSwap(false, true) {
						r.logger.Info().Msg("compact_boundary observed — surfacing status")
						select {
						case task.output <- "__COMPACT_START__":
						default:
							r.logger.Warn().Msg("output channel full, dropping __COMPACT_START__")
						}
					}
				}
			case "assistant":
				// Assistant messages contain text output and tool_use requests.
				// Note: We don't send text to output here because content_block_delta
				// already streams text as it arrives. Sending here would cause duplicates.
				if msg.Message != nil {
					for _, block := range msg.Message.Content {
						switch block.Type {
						case "text":
							// Only add to outputBuilder for final result, but don't send to channel
							// (content_block_delta already handles streaming output)
							if block.Text != "" {
								outputBuilder.WriteString(block.Text)
							}
						case "tool_use":
							// Track tool ID → name and input for showing context in results.
							toolInfos[block.ID] = toolInfo{
								Name:  block.Name,
								Input: block.Input,
							}
							// Bot-side ScheduleWakeup interception. claude's
							// built-in tool only really fires under /loop or
							// interactive mode; in `-p stream-json` the
							// "scheduled for…" tool_result text comes back
							// but no wake actually arrives. We arm our own
							// timer so SendInput(prompt) re-feeds the agent
							// when the delay expires. claude's tool_result
							// flows through unchanged (the user still sees
							// the "scheduled for HH:MM" message), and the
							// bot owns the actual wake.
							if block.Name == "ScheduleWakeup" {
								task.scheduleWakeup(block.Input)
							}
							// Log tool use but don't send to Slack - we'll show a summary
							// with the result instead (avoids duplicate "Using tool" + "result" messages).
							r.logger.Debug().
								Str("tool_id", block.ID).
								Str("tool_name", block.Name).
								Msg("tool use requested")
						}
					}
				}
			case "user":
				// User messages contain tool results.
				if msg.Message != nil {
					for _, block := range msg.Message.Content {
						if block.Type == "tool_result" {
							contentText := block.GetContentText()
							if contentText == "" {
								continue
							}
							info := toolInfos[block.ToolUseID]
							contentLen := len(contentText)
							r.logger.Debug().
								Str("tool_use_id", block.ToolUseID).
								Str("tool_name", info.Name).
								Bool("is_error", block.IsError).
								Int("content_len", contentLen).
								Msg("received tool result")
							outputBuilder.WriteString(contentText)

							// Send tool results to Slack:
							// - Short Bash output (<=500 bytes): inline code block
							// - Everything else: summary line + collapsible snippet
							const maxInlineLen = 500
							trimmedContent := strings.TrimRight(contentText, " \t\n\r")

							// Filter out [rerun: ...] control messages from tool output
							trimmedContent = rerunPattern.ReplaceAllString(trimmedContent, "")
							trimmedContent = strings.TrimRight(trimmedContent, " \t\n\r") // Re-trim after filtering

							// Skip empty content (e.g., after filtering control messages)
							if trimmedContent == "" {
								continue
							}

							var outputMsg string
							if info.Name == "Bash" && contentLen <= maxInlineLen {
								// Short Bash output: inline code block. Trailing newline
								// guarantees the closing fence sits on its own line even
								// when assistant text streams in right after — without
								// it, concatenating produces "```SSH works..." and the
								// fence never closes in Slack's parser.
								outputMsg = fmt.Sprintf("\n```\n%s\n```\n", trimmedContent)
							} else {
								// Show summary + upload as expandable snippet.
								// Use __SNIPPET__ prefix so handler can upload as collapsible file.
								// Format: __SNIPPET__toolName\x00inputJSON\x00content
								inputJSON, _ := json.Marshal(info.Input)
								outputMsg = fmt.Sprintf("__SNIPPET__%s\x00%s\x00%s", info.Name, inputJSON, trimmedContent)
							}

							// Tag results with no meaningful content so the
							// handler can drop them at default verbosity. The
							// sentinel text is what Bash emits when both stdout
							// and stderr are empty; there's no reason to clutter
							// the thread with a dozen "no output" fenced blocks
							// unless the user asked for full verbosity.
							if isTrivialToolResult(info.Name, trimmedContent) {
								outputMsg = "__TRIVIAL__" + outputMsg
							}

							select {
							case task.output <- outputMsg:
							default:
								r.logger.Warn().Msg("output channel full, dropping tool result")
							}
						}
					}
				}
			case "content_block_start":
				// Initial content block - may contain text/thinking for short responses.
				r.logger.Info().
					Bool("has_content_block", msg.ContentBlock != nil).
					Msg("received content_block_start")
				// If a compact_boundary was surfaced earlier in this
				// run, the next content_block_start is the model
				// producing real output again — pair the sentinel so
				// the handler can finalize its "compacting…" banner.
				if task.compactPending.CompareAndSwap(true, false) {
					select {
					case task.output <- "__COMPACT_END__":
					default:
						r.logger.Warn().Msg("output channel full, dropping __COMPACT_END__")
					}
				}
				if msg.ContentBlock != nil {
					r.logger.Info().
						Str("block_type", msg.ContentBlock.Type).
						Int("text_len", len(msg.ContentBlock.Text)).
						Int("thinking_len", len(msg.ContentBlock.Thinking)).
						Msg("content_block_start details")

					var text string
					var isThinking bool
					switch msg.ContentBlock.Type {
					case "text":
						text = msg.ContentBlock.Text
					case "thinking":
						// Extended thinking content - prefix for verbosity filtering
						text = msg.ContentBlock.Thinking
						isThinking = true
					}

					if text != "" {
						r.logger.Info().
							Int("text_len", len(text)).
							Str("text_preview", text[:min(50, len(text))]).
							Msg("extracting text from content_block_start")
						outputBuilder.WriteString(text)

						// Filter out [rerun: ...] control messages from output
						filtered := rerunPattern.ReplaceAllString(text, "")
						if filtered != "" {
							// Add __THINKING__ prefix for thinking blocks so handler can filter by verbosity
							if isThinking {
								filtered = "__THINKING__" + filtered
							}
							select {
							case task.output <- filtered:
							default:
								r.logger.Warn().Msg("output channel full, dropping content_block_start text")
							}
						}
					}
				}
			case "content_block_delta":
				// Partial streaming output. Send immediately for responsive feedback.
				// Note: delta is a TOP-LEVEL field in the message, not nested.
				r.logger.Info().
					Bool("has_delta", msg.Delta != nil).
					Int("index", msg.Index).
					Str("raw_line_preview", line[:min(200, len(line))]).
					Msg("received content_block_delta")
				if msg.Delta != nil {
					delta := msg.Delta
					r.logger.Info().
						Str("delta_type", delta.Type).
						Int("text_len", len(delta.Text)).
						Int("thinking_len", len(delta.Thinking)).
						Msg("content_block_delta details")
					var text string

					switch delta.Type {
					case "text_delta":
						text = delta.Text
					case "thinking_delta":
						// Extended thinking - prefix with __THINKING__ so handler can filter by verbosity
						text = "__THINKING__" + delta.Thinking
					case "input_json_delta":
						// Tool input JSON - we don't need to display this
						continue
					case "signature_delta":
						// Thinking signature - internal, don't display
						continue
					}

					if text != "" {
						r.logger.Info().
							Int("text_len", len(text)).
							Msg("sending text to output channel")
						// Strip __THINKING__ prefix for outputBuilder (final result)
						cleanText := strings.TrimPrefix(text, "__THINKING__")
						outputBuilder.WriteString(cleanText)

						// Filter out [rerun: ...] control messages from output
						filtered := rerunPattern.ReplaceAllString(text, "")
						if filtered != "" {
							select {
							case task.output <- filtered:
							default:
								r.logger.Warn().Msg("output channel full, dropping delta")
							}
						}
					}
				}
			case "result":
				// Final result with stats.
				resultLog := r.logger.Info().
					Str("subtype", msg.Subtype).
					Float64("cost_usd", msg.TotalCostUSD).
					Int("duration_ms", msg.DurationMS).
					Int("num_turns", msg.NumTurns).
					Bool("is_error", msg.IsError)
				if msg.IsError && msg.Result != "" {
					resultLog = resultLog.Str("result", msg.Result)
				}
				resultLog.Msg("task result")
				if msg.Result != "" {
					outputBuilder.WriteString(msg.Result)
				}
				// Surface the result text to Slack when the task errored.
				// On success, the same text already reached the channel via
				// content_block_delta streaming. On a synthetic error like
				// "Not logged in · Please run /login" there are no deltas,
				// so without this the user only sees the stats warning.
				if msg.IsError && msg.Result != "" {
					errMsg := fmt.Sprintf(":warning: %s", msg.Result)
					select {
					case task.output <- errMsg:
					default:
						r.logger.Warn().Msg("output channel full, dropping error result text")
					}
				}
				// Send stats as JSON for special formatting by handler.
				// Use __STATS__ prefix so handler can detect and format with blocks.
				statsJSON := fmt.Sprintf(
					"__STATS__{\"is_error\":%t,\"duration_ms\":%d,\"num_turns\":%d,\"cost_usd\":%.6f}",
					msg.IsError,
					msg.DurationMS,
					msg.NumTurns,
					msg.TotalCostUSD,
				)
				select {
				case task.output <- statsJSON:
				default:
					r.logger.Warn().Msg("output channel full, dropping stats")
				}
			case "control_request":
				// Handle permission requests via control messages (newer protocol).
				var ctrlReq ControlRequest
				if err := json.Unmarshal([]byte(line), &ctrlReq); err != nil {
					r.logger.Error().Err(err).Str("line", line).Msg("failed to parse control_request")
					continue
				}

				if ctrlReq.Subtype == "can_use_tool" {
					r.logger.Info().
						Str("tool_name", ctrlReq.ToolName).
						Str("request_id", ctrlReq.RequestID).
						Str("tool_use_id", ctrlReq.ToolUseID).
						Msg("received control_request for tool permission")

					// Convert to PermissionRequest format for existing handling.
					permReq := PermissionRequest{
						ToolName:  ctrlReq.ToolName,
						ToolInput: ctrlReq.ToolInput,
						ToolUseID: ctrlReq.ToolUseID,
					}
					// Store request_id for response.
					task.pendingControlRequestID = ctrlReq.RequestID

					select {
					case task.controlPermissionRequests <- permReq:
					default:
						r.logger.Warn().Msg("control permission channel full, dropping request")
					}
				} else {
					r.logger.Debug().
						Str("subtype", ctrlReq.Subtype).
						Msg("unhandled control_request subtype")
				}
			case "content_block_stop", "message_start", "message_delta", "message_stop":
				// These are part of the streaming protocol but we don't need to act on them.
				// content_block_stop: marks end of a content block
				// message_start: marks beginning of assistant message
				// message_delta: contains stop_reason and usage (we get this from result)
				// message_stop: marks end of assistant message
				r.logger.Debug().Str("type", msg.Type).Msg("received streaming marker")
			case "error":
				// Error event from Claude API
				r.logger.Error().
					Str("line", line).
					Msg("received error event from Claude")
			default:
				// Log unknown message types for debugging.
				if msg.Type != "" {
					r.logger.Warn().
						Str("type", msg.Type).
						Str("subtype", msg.Subtype).
						Int("line_len", len(line)).
						Msg("unknown message type")
				}
			}
		}

		// Wait for process to complete
		err := cmd.Wait()

		result := &Result{
			SessionID: task.sessionID,
			Output:    outputBuilder.String(),
		}

		if err != nil {
			// Report the actual runtime, not just the nominal deadline.
			// A container that outlives ctx cancellation (docker isn't
			// killed by Go's SIGKILL of the bash intermediate) can push
			// cmd.Wait to return hours past the deadline; "timed out
			// after 24h" hides that. See 2026-07-06 eagle post-mortem:
			// deadline fired at Jul 6 09:44:27, cmd.Wait didn't return
			// until docker stop at Jul 6 22:46:59, but the error text
			// still just read "timed out after 24h0m0s".
			runtimeElapsed := time.Since(runStart)
			// Container teardown on ctx cancel/timeout is handled by
			// the runCtx.Done watcher goroutine spawned at task
			// start — it fires stopContainer immediately when the
			// ctx signals, not after cmd.Wait unblocks. v0.36.4
			// tried calling stopContainer here and hit a chicken-
			// and-egg deadlock: cmd.Wait was blocked on the
			// stdout/stderr pipes held open by orphaned docker run,
			// so this branch didn't execute until the container was
			// stopped some other way. See the watcher's block
			// comment for details.
			switch runCtx.Err() {
			case context.DeadlineExceeded:
				if runtimeElapsed > r.timeout+time.Minute {
					result.Error = oops.New(
						"clod execution timed out (deadline %v elapsed %v ago; process kept running until now, total runtime %v)",
						r.timeout, runtimeElapsed-r.timeout, runtimeElapsed,
					)
				} else {
					result.Error = oops.New("clod execution timed out after %v", r.timeout)
				}
			case context.Canceled:
				result.Error = oops.New("clod execution was cancelled after %v", runtimeElapsed)
			default:
				// Neither we-timed-it-out nor we-cancelled-it. The
				// child died on its own — could be `docker stop` from
				// outside, OOM, a permbridge crash, an image issue,
				// etc. Include the stderr tail so the caller can
				// surface the actual reason to the user (SSH agent
				// missing, auth required, etc.) instead of "exit
				// status 1".
				if tail := snapshotStderrTail(); tail != "" {
					result.Error = oops.New("clod exited unexpectedly after %v: %s\n```\n%s\n```", runtimeElapsed, err.Error(), tail)
				} else {
					result.Error = oops.New("clod exited unexpectedly after %v: %w", runtimeElapsed, err)
				}
			}
		}

		task.done <- result
	}()

	return task, nil
}

// prepareDirectHome materializes `<taskPath>/.clod/direct-home/` with
// symlinks into `<taskPath>/.clod/claude/` so claude sees its normal
// `~/.claude/` + `~/.claude.json` layout. Idempotent — safe to call on
// every Start(). Returns the absolute HOME path to set in the child's
// environment.
func prepareDirectHome(taskPath string) (string, error) {
	absTask, err := filepath.Abs(taskPath)
	if err != nil {
		return "", oops.Trace(err)
	}
	homeDir := filepath.Join(absTask, ".clod", "direct-home")
	if err := os.MkdirAll(homeDir, 0700); err != nil {
		return "", oops.Trace(err)
	}

	claudeDir := filepath.Join(absTask, ".clod", "claude")
	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		return "", oops.Trace(err)
	}

	targets := []struct {
		link, target string
	}{
		{filepath.Join(homeDir, ".claude"), claudeDir},
		{filepath.Join(homeDir, ".claude.json"), filepath.Join(claudeDir, "claude.json")},
	}
	for _, t := range targets {
		// If the link already points at the right place, leave it alone.
		if existing, err := os.Readlink(t.link); err == nil && existing == t.target {
			continue
		}
		// Remove whatever is there (stale symlink or wrong target) before
		// recreating. os.Remove tolerates a missing file.
		_ = os.Remove(t.link)
		if err := os.Symlink(t.target, t.link); err != nil {
			return "", oops.Trace(err)
		}
	}
	return homeDir, nil
}

// Kill terminates a running process by its PID.
func (r *Runner) Kill(pid int) error {
	// Kill the entire process group
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		return oops.Trace(err)
	}
	return nil
}
