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
	"strings"
	"syscall"
	"time"

	"github.com/calebcase/oops"
	"github.com/creack/pty"
	"github.com/rs/zerolog"
)

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
	// For content_block_delta messages (partial streaming).
	ContentBlockDelta *ContentBlockDelta `json:"content_block_delta,omitempty"`
}

// StreamMsgBody represents the message body in stream-json output.
type StreamMsgBody struct {
	Content []StreamContentBlock `json:"content,omitempty"`
}

// StreamContentBlock represents a content block in the message.
type StreamContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
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
type TextDelta struct {
	Type string `json:"type"` // Usually "text_delta".
	Text string `json:"text,omitempty"`
}

// RunningTask represents a clod task that is currently executing.
type RunningTask struct {
	cmd            *exec.Cmd
	pty            *os.File
	output         chan string
	done           chan *Result
	cancel         context.CancelFunc
	sessionID      string
	taskPath       string // The path to the task directory.
	logger         zerolog.Logger
	permissionFIFO *PermissionFIFO
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

// SendInputWithImages writes text and optional images to the running task's PTY.
func (t *RunningTask) SendInputWithImages(text string, images []ImageData) error {
	if t.pty == nil {
		return oops.New("pty is closed")
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

	// Write JSON line to PTY
	_, err = t.pty.Write(append(data, '\n'))
	if err != nil {
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

// readAllowedTools reads the allowed tools from the task's claude.json config.
func readAllowedTools(taskPath string) []string {
	configPath := filepath.Join(taskPath, ".clod", "claude", "claude.json")

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}

	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return nil
	}

	projects, ok := config["projects"].(map[string]any)
	if !ok {
		return nil
	}

	project, ok := projects[taskPath].(map[string]any)
	if !ok {
		return nil
	}

	allowedTools, ok := project["allowedTools"].([]any)
	if !ok {
		return nil
	}

	var tools []string
	for _, t := range allowedTools {
		if s, ok := t.(string); ok {
			tools = append(tools, s)
		}
	}

	return tools
}

// Start begins executing clod in a task directory with the given prompt.
// If sessionID is provided, it resumes an existing session.
// Returns a RunningTask that can be used to send input and receive output.
func (r *Runner) Start(
	ctx context.Context,
	taskPath, prompt, sessionID string,
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
	// Note: We don't pass the prompt as a CLI arg - instead we send it via stream-json
	// input so we can include images inline.
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
	allowedTools := readAllowedTools(taskPath)
	for _, tool := range allowedTools {
		args = append(args, "--allowedTools", tool)
	}
	if len(allowedTools) > 0 {
		r.logger.Debug().
			Strs("allowed_tools", allowedTools).
			Msg("passing saved allowed tools to claude")
	}

	if r.permissionMode != "" && r.permissionMode != "default" {
		args = append(args, "--permission-mode", r.permissionMode)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	// Pass the text prompt as CLI argument.
	// Images (if any) will be sent via stream-json as a follow-up message.
	args = append(args, prompt)

	r.logger.Debug().
		Str("task_path", taskPath).
		Str("session_id", sessionID).
		Strs("args", args).
		Msg("starting clod with pty")

	//nolint:gosec
	cmd := exec.CommandContext(runCtx, "clod", args...)
	cmd.Dir = taskPath

	// Set MCP tool timeout to allow time for user to respond to permission prompts
	// Default is too short (causes "technical issues" when user doesn't respond quickly)
	// 5 minutes = 300000ms should be plenty for interactive approval
	cmd.Env = append(os.Environ(),
		"MCP_TOOL_TIMEOUT=300000",
		"CLOD_RUNTIME_SUFFIX="+permFIFO.RuntimeSuffix(),
		"CLOD_CONCURRENT=true",
		"CLOD_NONINTERACTIVE=true",
	)

	r.logger.Debug().
		Str("MCP_TOOL_TIMEOUT", "300000").
		Str("CLOD_RUNTIME_SUFFIX", permFIFO.RuntimeSuffix()).
		Bool("CLOD_NONINTERACTIVE", true).
		Bool("CLOD_CONCURRENT", true).
		Msg("setting environment variables for clod run")

	// Set up process group for clean termination
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	// Start command with PTY
	ptmx, err := pty.Start(cmd)
	if err != nil {
		cancel()
		permFIFO.Close()
		return nil, oops.Trace(err)
	}

	task := &RunningTask{
		cmd:            cmd,
		pty:            ptmx,
		output:         make(chan string, 100),
		done:           make(chan *Result, 1),
		cancel:         cancel,
		sessionID:      sessionID,
		taskPath:       taskPath,
		logger:         r.logger,
		permissionFIFO: permFIFO,
	}

	// Start permission FIFO listener
	permFIFO.Start(runCtx)

	// Read from PTY and parse stream-json in background
	go func() {
		defer close(task.output)
		defer close(task.done)
		defer func() { _ = ptmx.Close() }()
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

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}

			var msg StreamMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				r.logger.Debug().
					Str("line", line).
					Err(err).
					Msg("failed to parse stream-json line")
				continue
			}

			// Extract session ID if present
			if msg.SessionID != "" && task.sessionID == "" {
				task.sessionID = msg.SessionID
				r.logger.Debug().
					Str("session_id", task.sessionID).
					Msg("captured session ID")
			}

			// Handle different message types.
			switch msg.Type {
			case "system":
				// System messages include init with session_id.
				if msg.Subtype == "init" && msg.SessionID != "" {
					task.sessionID = msg.SessionID
					r.logger.Debug().
						Str("session_id", task.sessionID).
						Msg("captured session ID from system init")
				}
			case "assistant":
				// Assistant messages contain text output and tool_use requests.
				if msg.Message != nil {
					for _, block := range msg.Message.Content {
						switch block.Type {
						case "text":
							if block.Text != "" {
								outputBuilder.WriteString(block.Text)
								select {
								case task.output <- block.Text:
								default:
									r.logger.Warn().Msg("output channel full, dropping message")
								}
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

							var outputMsg string
							if info.Name == "Bash" && contentLen <= maxInlineLen {
								// Short Bash output: inline code block.
								outputMsg = fmt.Sprintf("\n```\n%s\n```", trimmedContent)
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
			case "content_block_delta":
				// Partial streaming output. Send immediately for responsive feedback.
				if msg.ContentBlockDelta != nil &&
					msg.ContentBlockDelta.Delta != nil &&
					msg.ContentBlockDelta.Delta.Text != "" {
					text := msg.ContentBlockDelta.Delta.Text
					outputBuilder.WriteString(text)
					select {
					case task.output <- text:
					default:
						r.logger.Warn().Msg("output channel full, dropping delta")
					}
				}
			case "result":
				// Final result with stats.
				r.logger.Info().
					Str("subtype", msg.Subtype).
					Float64("cost_usd", msg.TotalCostUSD).
					Int("duration_ms", msg.DurationMS).
					Int("num_turns", msg.NumTurns).
					Bool("is_error", msg.IsError).
					Msg("task result")
				if msg.Result != "" {
					outputBuilder.WriteString(msg.Result)
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
				result.Error = oops.Trace(err)
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
