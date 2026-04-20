package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/calebcase/oops"
	"github.com/creack/pty"
	"github.com/rs/zerolog"
)

// rerunPattern matches Claude Code's [rerun: ...] control messages that shouldn't be shown to users.
// Matches both "[rerun: b1]" (with space) and "[rerun:b1]" (without space).
var rerunPattern = regexp.MustCompile(`\[rerun:\s*[^\]]+\]\s*`)

// Runner executes clod processes.
type Runner struct {
	timeout          time.Duration
	permissionMode   string
	agentsPromptPath string
	logger           zerolog.Logger
}

// NewRunner creates a new Runner.
func NewRunner(
	timeout time.Duration,
	permissionMode string,
	agentsPromptPath string,
	logger zerolog.Logger,
) *Runner {
	return &Runner{
		timeout:          timeout,
		permissionMode:   permissionMode,
		agentsPromptPath: agentsPromptPath,
		logger:           logger.With().Str("component", "runner").Logger(),
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
	// sessionIDCaptured receives the session ID exactly once, as soon as the
	// stream parser observes it (normally the first system/init message). The
	// handler uses it to persist the thread → session mapping early so that
	// a bot restart mid-task doesn't orphan the thread.
	sessionIDCaptured chan string
	sessionIDOnce     sync.Once
}

// closeStdin closes the bot's end of the child's stdin pipe. It's safe to
// call multiple times.
func (t *RunningTask) closeStdin() {
	if t.stdin != nil {
		_ = t.stdin.Close()
		t.stdin = nil
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

	t.logger.Debug().
		Int("num_images", len(images)).
		Int("json_len", len(data)).
		Msg("sending input to claude")

	if _, err := t.stdin.Write(append(data, '\n')); err != nil {
		return oops.Trace(err)
	}
	return nil
}

// Output returns the channel for receiving output chunks.
func (t *RunningTask) Output() <-chan string {
	return t.output
}

// PermissionRequests returns the channel for receiving permission requests from the FIFO.
func (t *RunningTask) PermissionRequests() <-chan PermissionRequest {
	if t.permissionFIFO == nil {
		return nil
	}
	return t.permissionFIFO.Requests()
}

// SendPermissionResponse sends a response to a permission request.
func (t *RunningTask) SendPermissionResponse(resp PermissionResponse) {
	if t.permissionFIFO != nil {
		t.permissionFIFO.SendResponse(resp)
	}
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

// Cancel cancels the running task.
func (t *RunningTask) Cancel() {
	if t.cancel != nil {
		t.cancel()
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

	// Deduplicate tools (since we read from both arrays)
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

// Start begins executing clod in a task directory with the given prompt.
// If sessionID is provided, it resumes an existing session.
// If model is non-empty, it's passed through as claude's --model flag (e.g.
// "opus", "sonnet", or a full model id). Mid-session model switching isn't
// supported by claude --input-format stream-json, but --resume honors a
// changed --model on the next start.
// Returns a RunningTask that can be used to send input and receive output.
func (r *Runner) Start(
	ctx context.Context,
	taskPath, prompt, sessionID, model string,
) (*RunningTask, error) {
	// Create command with timeout context.
	runCtx, cancel := context.WithTimeout(ctx, r.timeout)

	// Create permission FIFO for MCP communication (must be done before building args).
	// Pass empty string to generate a unique runtime suffix for concurrent instances.
	permFIFO, err := NewPermissionFIFO(taskPath, "", r.agentsPromptPath, r.logger)
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

	// Add the agent system prompt flag if AGENT.md was copied to the runtime directory.
	if agentPromptPath := permFIFO.AgentPromptPath(); agentPromptPath != "" {
		runtimeDirName := filepath.Join(".clod", "runtime-"+permFIFO.RuntimeSuffix())
		args = append(
			args,
			"--append-system-prompt",
			fmt.Sprintf(
				"You are an agent as described in %s/AGENT.md; Read that document as soon as possible and treat it as part of your system prompt.",
				runtimeDirName,
			),
		)
	}

	// Pass any saved allowed tools so they're respected immediately.
	allowedTools := readAllowedTools(taskPath, r.logger)
	for _, tool := range allowedTools {
		args = append(args, "--allowedTools", tool)
	}
	if len(allowedTools) > 0 {
		r.logger.Info().
			Strs("allowed_tools", allowedTools).
			Msg("passing saved allowed tools to claude")
	} else {
		r.logger.Info().Msg("no saved allowed tools found")
	}

	if r.permissionMode != "" && r.permissionMode != "default" {
		args = append(args, "--permission-mode", r.permissionMode)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	if model != "" {
		args = append(args, "--model", model)
	}

	r.logger.Debug().
		Str("task_path", taskPath).
		Str("session_id", sessionID).
		Str("model", model).
		Strs("args", args).
		Msg("starting clod with pty")

	//nolint:gosec
	cmd := exec.CommandContext(runCtx, "clod", args...)
	cmd.Dir = taskPath

	// Set MCP tool timeout to allow time for user to respond to permission prompts.
	// Default is too short (causes "technical issues" when user doesn't respond quickly).
	// 5 minutes = 300000ms should be plenty for interactive approval.
	// Note: CLOD_RUNTIME_DIR is constructed by the run script from CLOD_RUNTIME_SUFFIX.
	cmd.Env = append(os.Environ(),
		"MCP_TOOL_TIMEOUT=300000",
		"CLOD_RUNTIME_SUFFIX="+permFIFO.RuntimeSuffix(),
		"CLOD_CONCURRENT=true",
		"CLOD_NONINTERACTIVE=true",
	)

	r.logger.Debug().
		Str("MCP_TOOL_TIMEOUT", "300000").
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
			r.logger.Debug().
				Str("stderr", line).
				Msg("clod wrapper stderr")
		}
	}()
	snapshotStderrTail := func() string {
		stderrMu.Lock()
		defer stderrMu.Unlock()
		return strings.Join(stderrTail, "\n")
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
	}
	// When resuming an existing session, the caller already has the mapping,
	// but fire the notification anyway so the save-on-first-observation path
	// is uniform and idempotent.
	if sessionID != "" {
		task.notifySessionID(sessionID)
	}

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
		defer close(task.done)
		defer func() { _ = ptmx.Close() }()
		defer task.closeStdin()
		defer permFIFO.Close()

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
			case "content_block_stop", "message_start", "message_delta", "message_stop", "ping":
				// These are part of the streaming protocol but we don't need to act on them.
				// content_block_stop: marks end of a content block
				// message_start: marks beginning of assistant message
				// message_delta: contains stop_reason and usage (we get this from result)
				// message_stop: marks end of assistant message
				// ping: keep-alive signal
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
			// Check if it was a timeout
			if runCtx.Err() == context.DeadlineExceeded {
				result.Error = oops.New("clod execution timed out after %v", r.timeout)
			} else if runCtx.Err() == context.Canceled {
				result.Error = oops.New("clod execution was cancelled")
			} else {
				// Include the recent stderr tail so the caller can surface
				// the actual reason to the user (SSH agent missing, auth
				// required, etc.) instead of "exit status 1".
				if tail := snapshotStderrTail(); tail != "" {
					result.Error = oops.New("%s\n```\n%s\n```", err.Error(), tail)
				} else {
					result.Error = oops.Trace(err)
				}
			}
		}

		task.done <- result
	}()

	return task, nil
}

// Kill terminates a running process by its PID.
func (r *Runner) Kill(pid int) error {
	// Kill the entire process group
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		return oops.Trace(err)
	}
	return nil
}
