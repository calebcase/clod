package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// PendingPermission tracks a permission request waiting for user response.
type PendingPermission struct {
	MessageTS string // Timestamp of the permission prompt message (for updating)
	ChannelID string
	ThreadTS  string
	ToolName  string         // Tool that requested permission
	ToolInput map[string]any // Tool input parameters (for display)
}

// Handler processes Slack events.
type Handler struct {
	bot    *Bot
	logger zerolog.Logger

	// Track running tasks by thread key ("channelID:threadTS")
	runningTasks sync.Map // key -> *RunningTask

	// Track threads waiting for permission responses
	pendingPermissions sync.Map // key -> *PendingPermission
}

// NewHandler creates a new Handler.
func NewHandler(bot *Bot) *Handler {
	return &Handler{
		bot:    bot,
		logger: bot.logger.With().Str("component", "handler").Logger(),
	}
}

// mentionPattern matches @mentions at the start of a message
var otherMentionPattern = regexp.MustCompile(`^<@([A-Z0-9]+)>`)

// HandleAppMention processes app mention events.
func (h *Handler) HandleAppMention(ctx context.Context, ev *slackevents.AppMentionEvent) {
	// Use thread_ts if in a thread, otherwise use the message ts as thread root
	threadTS := ev.ThreadTimeStamp
	if threadTS == "" {
		threadTS = ev.TimeStamp
	}

	logger := h.logger.With().
		Str("channel", ev.Channel).
		Str("thread_ts", threadTS).
		Str("user", ev.User).
		Str("text", ev.Text).
		Logger()

	logger.Info().Msg("received app mention")

	// Check authorization
	if !h.bot.auth.IsAuthorized(ev.User) {
		logger.Warn().Msg("unauthorized user")
		h.bot.PostMessage(ev.Channel, h.bot.auth.RejectMessage(), threadTS)
		return
	}

	// Check if there's already a running task in this thread
	progressKey := key(ev.Channel, threadTS)
	if taskVal, ok := h.runningTasks.Load(progressKey); ok {
		// Task is running - send this as input to Claude
		task := taskVal.(*RunningTask)
		input := ParseContinuation(ev.Text)
		if input != "" {
			logger.Debug().Str("input", input).Msg("sending input to running task")
			if err := task.SendInput(input); err != nil {
				logger.Error().Err(err).Msg("failed to send input to task")
			}
		}
		return
	}

	// Check for existing session (continuation in thread)
	session := h.bot.sessions.Get(ev.Channel, threadTS)

	if session != nil {
		// Continuation in existing thread
		h.handleContinuation(ctx, ev, session, threadTS, logger)
	} else {
		// New task request
		h.handleNewTask(ctx, ev, threadTS, logger)
	}
}

// HandleMessage processes regular message events (for thread replies).
func (h *Handler) HandleMessage(ctx context.Context, ev *slackevents.MessageEvent) {
	// Ignore bot messages
	if ev.BotID != "" {
		return
	}

	// Only handle thread replies
	threadTS := ev.ThreadTimeStamp
	if threadTS == "" {
		return
	}

	logger := h.logger.With().
		Str("channel", ev.Channel).
		Str("thread_ts", threadTS).
		Str("user", ev.User).
		Str("text", ev.Text).
		Logger()

	// Check if message is @mentioning someone (let app_mention handle those)
	if matches := otherMentionPattern.FindStringSubmatch(ev.Text); matches != nil {
		mentionedUser := matches[1]
		logger.Debug().
			Str("mentioned_user", mentionedUser).
			Msg("message mentions a user, ignoring (app_mention will handle if it's the bot)")
		return
	}

	// Check if there's a running task in this thread
	progressKey := key(ev.Channel, threadTS)
	taskVal, ok := h.runningTasks.Load(progressKey)

	if ok {
		// Task is running - send input or handle permission
		task := taskVal.(*RunningTask)

		// Check if we're waiting for a permission response (text fallback - buttons are preferred)
		if pending, ok := h.pendingPermissions.Load(progressKey); ok {
			perm := pending.(*PendingPermission)
			// Try to parse as permission response
			if resp := parsePermissionResponse(ev.Text); resp != nil {
				logger.Info().
					Str("behavior", resp.Behavior).
					Msg("received permission response from user (text)")

				task.SendPermissionResponse(*resp)
				h.pendingPermissions.Delete(progressKey)

				// Update the permission message to show it was handled
				h.updatePermissionMessage(perm, resp.Behavior, ev.User, "")
				return
			}
			// Not a clear yes/no, remind them to use buttons
			h.bot.PostMessage(
				ev.Channel,
				"_Please use the buttons above to respond, or type_ `yes` _or_ `no`_._",
				threadTS,
			)
			return
		}

		// Send the message as input to Claude
		logger.Debug().Str("input", ev.Text).Msg("sending thread reply to running task")
		if err := task.SendInput(ev.Text); err != nil {
			logger.Error().Err(err).Msg("failed to send input to task")
		}
		return
	}

	// No running task, so check if there's a saved session to resume.
	session := h.bot.sessions.Get(ev.Channel, threadTS)
	if session == nil {
		// No session for this thread. Let the user know.
		logger.Debug().Msg("no running task or saved session for thread")
		h.bot.PostMessage(
			ev.Channel,
			":question: I don't have a saved session for this thread. Use `@bot task_name: your instructions` to start a new task.",
			threadTS,
		)
		return
	}

	// Check authorization
	if !h.bot.auth.IsAuthorized(ev.User) {
		logger.Warn().Msg("unauthorized user trying to resume session")
		return
	}

	// Resume the session with the user's message
	logger = logger.With().
		Str("task", session.TaskName).
		Str("session_id", session.SessionID).
		Str("instructions", ev.Text).
		Logger()

	logger.Info().Msg("resuming session from thread reply")

	// Check for files attached to the message and download them to .clod-runtime/inputs.
	slackFiles, err := h.bot.files.GetThreadReplyFiles(ev.Channel, threadTS, ev.TimeStamp)
	if err != nil {
		logger.Warn().Err(err).Msg("failed to check for thread reply files")
	}

	// Download files to disk for Claude to read.
	var downloadedFiles []string
	if len(slackFiles) > 0 {
		h.bot.PostMessage(
			ev.Channel,
			fmt.Sprintf(":inbox_tray: Downloading %d file(s)...", len(slackFiles)),
			threadTS,
		)
		for _, file := range slackFiles {
			localPath, err := h.bot.files.DownloadToTask(file, session.TaskPath)
			if err != nil {
				logger.Error().Err(err).Str("file_id", file.ID).Msg("failed to download file")
				h.bot.PostMessage(
					ev.Channel,
					fmt.Sprintf(":warning: Failed to download `%s`: %v", file.Name, err),
					threadTS,
				)
				continue
			}
			logger.Info().
				Str("file_id", file.ID).
				Str("local_path", localPath).
				Msg("file downloaded to task inputs")
			downloadedFiles = append(downloadedFiles, localPath)
		}
	}

	// Build the prompt, appending file paths if any were downloaded.
	prompt := ev.Text
	if len(downloadedFiles) > 0 {
		prompt += "\n\nAttached files have been saved to:\n"
		for _, path := range downloadedFiles {
			prompt += fmt.Sprintf("- %s\n", path)
		}
	}

	// Post status
	h.bot.PostMessage(
		ev.Channel,
		fmt.Sprintf(":arrows_counterclockwise: Resuming task `%s`...", session.TaskName),
		threadTS,
	)

	// Run clod with existing session
	h.runClod(
		ctx,
		ev.Channel,
		ev.User,
		session.TaskPath,
		session.TaskName,
		prompt,
		session.SessionID,
		threadTS,
		logger,
	)
}

// handleNewTask processes a new task request.
func (h *Handler) handleNewTask(
	ctx context.Context,
	ev *slackevents.AppMentionEvent,
	threadTS string,
	logger zerolog.Logger,
) {
	// Parse the mention
	parsed := ParseMention(ev.Text)
	if parsed == nil {
		msg := fmt.Sprintf(
			"I didn't understand that. Please use the format: `@bot task_name: your instructions`\n\n%s",
			h.bot.tasks.ListFormatted(),
		)
		h.bot.PostMessage(ev.Channel, msg, threadTS)
		return
	}

	logger = logger.With().
		Str("task", parsed.TaskName).
		Str("instructions", parsed.Instructions).
		Logger()

	// Look up the task
	taskPath, err := h.bot.tasks.Get(parsed.TaskName)
	if err != nil {
		msg := fmt.Sprintf(
			"Unknown task: `%s`\n\n%s",
			parsed.TaskName,
			h.bot.tasks.ListFormatted(),
		)
		h.bot.PostMessage(ev.Channel, msg, threadTS)
		return
	}

	logger.Info().Str("task_path", taskPath).Msg("starting new task")

	// Check for files attached to the message and download them to .clod-runtime/inputs.
	logger.Debug().
		Str("channel", ev.Channel).
		Str("message_ts", ev.TimeStamp).
		Msg("checking for files in message")
	slackFiles, err := h.bot.files.GetMessageFiles(ev.Channel, ev.TimeStamp)
	if err != nil {
		logger.Warn().Err(err).Msg("failed to check for message files")
	}
	logger.Debug().Int("num_files", len(slackFiles)).Msg("files check complete")

	// Download files to disk for Claude to read.
	var downloadedFiles []string
	if len(slackFiles) > 0 {
		h.bot.PostMessage(
			ev.Channel,
			fmt.Sprintf(":inbox_tray: Downloading %d file(s)...", len(slackFiles)),
			threadTS,
		)
		for _, file := range slackFiles {
			localPath, err := h.bot.files.DownloadToTask(file, taskPath)
			if err != nil {
				logger.Error().Err(err).Str("file_id", file.ID).Msg("failed to download file")
				h.bot.PostMessage(
					ev.Channel,
					fmt.Sprintf(":warning: Failed to download `%s`: %v", file.Name, err),
					threadTS,
				)
				continue
			}
			logger.Info().
				Str("file_id", file.ID).
				Str("local_path", localPath).
				Msg("file downloaded to task inputs")
			downloadedFiles = append(downloadedFiles, localPath)
		}
	}

	// Build the prompt, appending file paths if any were downloaded.
	prompt := parsed.Instructions
	if len(downloadedFiles) > 0 {
		prompt += "\n\nAttached files have been saved to:\n"
		for _, path := range downloadedFiles {
			prompt += fmt.Sprintf("- %s\n", path)
		}
	}

	// Post initial status
	h.bot.PostMessage(
		ev.Channel,
		fmt.Sprintf(":rocket: Starting a `%s` task...", parsed.TaskName),
		threadTS,
	)

	// Run clod
	h.runClod(
		ctx,
		ev.Channel,
		ev.User,
		taskPath,
		parsed.TaskName,
		prompt,
		"",
		threadTS,
		logger,
	)
}

// handleContinuation processes a continuation in an existing thread.
func (h *Handler) handleContinuation(
	ctx context.Context,
	ev *slackevents.AppMentionEvent,
	session *SessionMapping,
	threadTS string,
	logger zerolog.Logger,
) {
	instructions := ParseContinuation(ev.Text)
	if instructions == "" {
		h.bot.PostMessage(ev.Channel, "Please provide instructions for the task.", threadTS)
		return
	}

	logger = logger.With().
		Str("task", session.TaskName).
		Str("session_id", session.SessionID).
		Str("instructions", instructions).
		Logger()

	logger.Info().Msg("continuing existing session")

	// Post initial status
	h.bot.PostMessage(
		ev.Channel,
		fmt.Sprintf(":arrows_counterclockwise: Continuing task `%s`...", session.TaskName),
		threadTS,
	)

	// Run clod with existing session
	h.runClod(
		ctx,
		ev.Channel,
		ev.User,
		session.TaskPath,
		session.TaskName,
		instructions,
		session.SessionID,
		threadTS,
		logger,
	)
}

// runClod executes clod and streams output to Slack.
func (h *Handler) runClod(
	ctx context.Context,
	channelID string,
	userID string,
	taskPath string,
	taskName string,
	prompt string,
	sessionID string,
	threadTS string,
	logger zerolog.Logger,
) {
	// Start the task
	task, err := h.bot.runner.Start(ctx, taskPath, prompt, sessionID)
	if err != nil {
		logger.Error().Err(err).Msg("failed to start clod")
		h.bot.PostMessage(channelID, fmt.Sprintf(":x: Failed to start task: %v", err), threadTS)
		return
	}

	// Register the running task
	progressKey := key(channelID, threadTS)
	h.runningTasks.Store(progressKey, task)
	defer h.runningTasks.Delete(progressKey)
	defer h.pendingPermissions.Delete(progressKey) // Clean up any pending permission state

	// Start watching for output files to upload to Slack.
	outputWatchDone := make(chan struct{})
	go h.bot.files.WatchOutputs(taskPath, channelID, threadTS, outputWatchDone)
	defer close(outputWatchDone)

	// Output batching
	const batchInterval = 2 * time.Second
	const maxBatchLen = 1500 // Leave room for formatting in Slack's 4000 char limit

	var outputBuffer strings.Builder
	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()

	// Function to flush the buffer.
	flushBuffer := func() {
		if outputBuffer.Len() > 0 {
			// Convert GitHub-flavored markdown to Slack's mrkdwn format.
			msg := strings.TrimSpace(outputBuffer.String())
			if msg != "" {
				msg = ConvertMarkdownToMrkdwn(msg)
				if _, err := h.bot.PostMessage(channelID, msg, threadTS); err != nil {
					logger.Debug().Err(err).Msg("failed to post output message")
				}
			}
			outputBuffer.Reset()
		}
	}

	// Get permission request channel (may be nil if FIFO not available)
	permRequests := task.PermissionRequests()

	// Process output and wait for completion
	for {
		select {
		case content, ok := <-task.Output():
			if !ok {
				// Channel closed, task is done.
				flushBuffer()
				goto done
			}

			// Check for special stats message.
			if strings.HasPrefix(content, "__STATS__") {
				flushBuffer() // Flush any pending output first.
				h.postStatsMessage(channelID, threadTS, content[9:]) // Skip "__STATS__" prefix.
				continue
			}

			// Check for snippet message (tool output to upload as collapsible file).
			if strings.HasPrefix(content, "__SNIPPET__") {
				flushBuffer() // Flush any pending output first.
				// Format: __SNIPPET__toolName\x00inputJSON\x00content
				payload := content[11:] // Skip "__SNIPPET__" prefix.
				parts := strings.SplitN(payload, "\x00", 3)
				if len(parts) == 3 {
					toolName := parts[0]
					inputJSON := parts[1]
					snippetContent := parts[2]
					h.postToolSnippet(channelID, threadTS, toolName, inputJSON, snippetContent, logger)
				}
				continue
			}

			outputBuffer.WriteString(content)

			// Flush if buffer is getting large.
			if outputBuffer.Len() >= maxBatchLen {
				flushBuffer()
			}

		case req, ok := <-permRequests:
			if ok {
				// Check if this permission is already allowed by saved rules.
				if h.isPermissionAllowed(task.taskPath, req.ToolName, req.ToolInput) {
					logger.Info().
						Str("tool_name", req.ToolName).
						Msg("auto-allowing permission based on saved rule")
					task.SendPermissionResponse(PermissionResponse{Behavior: "allow"})
					continue
				}

				// Post formatted permission prompt with buttons to Slack.
				flushBuffer() // Flush any pending output first.
				blocks := h.buildPermissionBlocks(req, progressKey)
				msgTS, err := h.bot.PostMessageBlocks(channelID, blocks, threadTS)
				if err != nil {
					logger.Error().Err(err).Msg("failed to post permission prompt")
					// Send deny on failure to post.
					task.SendPermissionResponse(
						PermissionResponse{Behavior: "deny", Message: "Failed to prompt user"},
					)
					continue
				}

				// Track the pending permission with its message timestamp and tool details.
				h.pendingPermissions.Store(progressKey, &PendingPermission{
					MessageTS: msgTS,
					ChannelID: channelID,
					ThreadTS:  threadTS,
					ToolName:  req.ToolName,
					ToolInput: req.ToolInput,
				})

				logger.Info().
					Str("tool_name", req.ToolName).
					Str("tool_use_id", req.ToolUseID).
					Str("message_ts", msgTS).
					Msg("posted permission prompt to slack, waiting for response")
			}

		case <-ticker.C:
			// Periodic flush
			flushBuffer()

		case result := <-task.Done():
			// Task completed
			flushBuffer()

			// Post completion message
			var finalMsg string
			if result.Error != nil {
				logger.Error().Err(result.Error).Msg("clod returned error")
				finalMsg = fmt.Sprintf(":warning: Task completed with error: %v", result.Error)
			} else {
				logger.Info().
					Str("session_id", result.SessionID).
					Msg("task completed successfully")
				finalMsg = ":white_check_mark: Task completed!"
			}
			h.bot.PostMessage(channelID, finalMsg, threadTS)

			// Save session mapping
			if result.SessionID != "" {
				session := &SessionMapping{
					ChannelID: channelID,
					ThreadTS:  threadTS,
					TaskName:  taskName,
					TaskPath:  taskPath,
					SessionID: result.SessionID,
					UserID:    userID,
					CreatedAt: time.Now(),
				}
				h.bot.sessions.Set(session)

				if err := h.bot.sessions.Save(); err != nil {
					logger.Error().Err(err).Msg("failed to save sessions")
				}
			}
			return
		}
	}

done:
	// Wait for final result if we exited via output channel close
	result := <-task.Done()
	var finalMsg string
	if result.Error != nil {
		logger.Error().Err(result.Error).Msg("clod returned error")
		finalMsg = fmt.Sprintf(":warning: Task completed with error: %v", result.Error)
	} else {
		logger.Info().
			Str("session_id", result.SessionID).
			Msg("task completed successfully")
		finalMsg = ":white_check_mark: Task completed!"
	}
	h.bot.PostMessage(channelID, finalMsg, threadTS)

	// Save session mapping
	if result.SessionID != "" {
		session := &SessionMapping{
			ChannelID: channelID,
			ThreadTS:  threadTS,
			TaskName:  taskName,
			TaskPath:  taskPath,
			SessionID: result.SessionID,
			UserID:    userID,
			CreatedAt: time.Now(),
		}
		h.bot.sessions.Set(session)

		if err := h.bot.sessions.Save(); err != nil {
			logger.Error().Err(err).Msg("failed to save sessions")
		}
	}
}

// PermissionActionValue holds the data encoded in button action values.
type PermissionActionValue struct {
	ThreadKey string `json:"k"`           // The progressKey for looking up the task
	Behavior  string `json:"b"`           // "allow" or "deny"
	Remember  string `json:"r,omitempty"` // Permission pattern to remember (empty = one-time)
}

// buildPermissionBlocks creates Slack blocks for a permission prompt with buttons.
func (h *Handler) buildPermissionBlocks(req PermissionRequest, progressKey string) []slack.Block {
	blocks := []slack.Block{}

	// Header
	headerText := slack.NewTextBlockObject("mrkdwn", ":lock: *Permission Required*", false, false)
	blocks = append(blocks, slack.NewSectionBlock(headerText, nil, nil))

	// Tool name
	toolText := slack.NewTextBlockObject(
		"mrkdwn",
		fmt.Sprintf("*Tool:* `%s`", req.ToolName),
		false,
		false,
	)
	blocks = append(blocks, slack.NewSectionBlock(toolText, nil, nil))

	// Tool-specific details
	var detailText string
	switch req.ToolName {
	case "Bash":
		if cmd, ok := req.ToolInput["command"].(string); ok {
			// Truncate long commands for display
			if len(cmd) > 500 {
				cmd = cmd[:500] + "..."
			}
			detailText = fmt.Sprintf("*Command:*\n```%s```", cmd)
		}
	case "Write", "Edit":
		if path, ok := req.ToolInput["file_path"].(string); ok {
			detailText = fmt.Sprintf("*File:* `%s`", path)
		}
	case "Read":
		if path, ok := req.ToolInput["file_path"].(string); ok {
			detailText = fmt.Sprintf("*File:* `%s`", path)
		}
	case "WebFetch":
		if url, ok := req.ToolInput["url"].(string); ok {
			detailText = fmt.Sprintf("*URL:* %s", url)
		}
	case "WebSearch":
		if query, ok := req.ToolInput["query"].(string); ok {
			detailText = fmt.Sprintf("*Query:* `%s`", query)
		}
	default:
		// Generic display of tool input
		var parts []string
		for k, v := range req.ToolInput {
			parts = append(parts, fmt.Sprintf("*%s:* `%v`", k, v))
		}
		detailText = strings.Join(parts, "\n")
	}

	if detailText != "" {
		detailBlock := slack.NewTextBlockObject("mrkdwn", detailText, false, false)
		blocks = append(blocks, slack.NewSectionBlock(detailBlock, nil, nil))
	}

	// Generate permission patterns for "remember" options
	alwaysPattern := req.ToolName // e.g., "Bash" allows all Bash commands
	similarPattern := generateSimilarPattern(req.ToolName, req.ToolInput)

	// Encode action values
	allowOnceValue, _ := json.Marshal(PermissionActionValue{
		ThreadKey: progressKey,
		Behavior:  "allow",
	})
	allowAlwaysValue, _ := json.Marshal(PermissionActionValue{
		ThreadKey: progressKey,
		Behavior:  "allow",
		Remember:  alwaysPattern,
	})
	denyValue, _ := json.Marshal(PermissionActionValue{
		ThreadKey: progressKey,
		Behavior:  "deny",
	})

	// Action buttons - first row: Allow Once, Deny
	allowOnceBtn := slack.NewButtonBlockElement(
		"permission_allow",
		string(allowOnceValue),
		slack.NewTextBlockObject("plain_text", "Allow Once", false, false),
	)
	allowOnceBtn.Style = "primary"

	denyBtn := slack.NewButtonBlockElement(
		"permission_deny",
		string(denyValue),
		slack.NewTextBlockObject("plain_text", "Deny", false, false),
	)
	denyBtn.Style = "danger"

	actionBlock1 := slack.NewActionBlock("permission_actions", allowOnceBtn, denyBtn)
	blocks = append(blocks, actionBlock1)

	// Second row: Allow Always, Allow Similar (if pattern is different from always)
	allowAlwaysBtn := slack.NewButtonBlockElement(
		"permission_allow_always",
		string(allowAlwaysValue),
		slack.NewTextBlockObject("plain_text", fmt.Sprintf("Allow All %s", req.ToolName), false, false),
	)

	if similarPattern != "" && similarPattern != alwaysPattern {
		allowSimilarValue, _ := json.Marshal(PermissionActionValue{
			ThreadKey: progressKey,
			Behavior:  "allow",
			Remember:  similarPattern,
		})
		allowSimilarBtn := slack.NewButtonBlockElement(
			"permission_allow_similar",
			string(allowSimilarValue),
			slack.NewTextBlockObject("plain_text", "Allow Similar", false, false),
		)
		actionBlock2 := slack.NewActionBlock("permission_actions_2", allowAlwaysBtn, allowSimilarBtn)
		blocks = append(blocks, actionBlock2)
	} else {
		actionBlock2 := slack.NewActionBlock("permission_actions_2", allowAlwaysBtn)
		blocks = append(blocks, actionBlock2)
	}

	return blocks
}

// generateSimilarPattern creates a permission pattern for "similar" requests.
// For example:
// - Bash: "python script.py" -> "Bash(python:*)"
// - Write: "/path/to/src/file.go" -> "Write(src/**)"
// - WebFetch: "https://example.com/api" -> "WebFetch(https://example.com:*)"
func generateSimilarPattern(toolName string, toolInput map[string]any) string {
	switch toolName {
	case "Bash":
		if cmd, ok := toolInput["command"].(string); ok {
			// Extract the first word (command name) and create pattern
			parts := strings.Fields(cmd)
			if len(parts) > 0 {
				cmdName := parts[0]
				// Handle common command patterns
				return fmt.Sprintf("Bash(%s:*)", cmdName)
			}
		}
	case "Write", "Edit", "Read":
		if path, ok := toolInput["file_path"].(string); ok {
			// Find a reasonable directory pattern
			// e.g., /home/user/project/src/file.go -> Write(src/**)
			dir := filepath.Dir(path)
			base := filepath.Base(dir)
			if base != "" && base != "." && base != "/" {
				return fmt.Sprintf("%s(%s/**)", toolName, base)
			}
		}
	case "WebFetch":
		if urlStr, ok := toolInput["url"].(string); ok {
			// Extract domain and create pattern
			// e.g., https://example.com/api/v1 -> WebFetch(https://example.com:*)
			if idx := strings.Index(urlStr, "://"); idx != -1 {
				rest := urlStr[idx+3:]
				if slashIdx := strings.Index(rest, "/"); slashIdx != -1 {
					domain := urlStr[:idx+3+slashIdx]
					return fmt.Sprintf("WebFetch(%s:*)", domain)
				}
			}
		}
	case "WebSearch":
		// No good pattern for search queries
		return ""
	}
	return ""
}

// HandleBlockAction processes button click events.
func (h *Handler) HandleBlockAction(
	ctx context.Context,
	callback *slack.InteractionCallback,
	action *slack.BlockAction,
) {
	logger := h.logger.With().
		Str("action_id", action.ActionID).
		Str("block_id", action.BlockID).
		Str("user", callback.User.ID).
		Logger()

	logger.Info().Msg("handling block action")

	// Check if this is a permission action
	isPermissionAction := action.ActionID == "permission_allow" ||
		action.ActionID == "permission_deny" ||
		action.ActionID == "permission_allow_always" ||
		action.ActionID == "permission_allow_similar"
	if !isPermissionAction {
		logger.Debug().Msg("ignoring non-permission action")
		return
	}

	// Decode the action value
	var actionValue PermissionActionValue
	if err := json.Unmarshal([]byte(action.Value), &actionValue); err != nil {
		logger.Error().Err(err).Str("value", action.Value).Msg("failed to decode action value")
		return
	}

	logger = logger.With().
		Str("thread_key", actionValue.ThreadKey).
		Str("behavior", actionValue.Behavior).
		Str("remember", actionValue.Remember).
		Logger()

	// Look up the running task
	taskVal, ok := h.runningTasks.Load(actionValue.ThreadKey)
	if !ok {
		logger.Warn().Msg("no running task found for permission response")
		// Update the message to show it's stale
		h.bot.UpdateMessage(callback.Channel.ID, callback.Message.Timestamp,
			":warning: This permission request is no longer active.")
		return
	}

	task := taskVal.(*RunningTask)

	// Check if we were waiting for this permission
	pendingVal, ok := h.pendingPermissions.Load(actionValue.ThreadKey)
	if !ok {
		logger.Warn().Msg("no pending permission found")
		return
	}
	pending := pendingVal.(*PendingPermission)

	// Send the response to Claude via FIFO
	resp := PermissionResponse{Behavior: actionValue.Behavior}
	if actionValue.Behavior == "deny" {
		resp.Message = fmt.Sprintf("User %s denied permission", callback.User.Name)
	}

	logger.Info().
		Str("behavior", resp.Behavior).
		Msg("sending permission response from button click")
	task.SendPermissionResponse(resp)
	logger.Info().Msg("permission response sent to FIFO")

	// Save the permission pattern if "remember" was selected
	if actionValue.Remember != "" && actionValue.Behavior == "allow" {
		if err := h.bot.savePermissionRule(task.taskPath, actionValue.Remember); err != nil {
			logger.Error().Err(err).Str("pattern", actionValue.Remember).Msg("failed to save permission rule")
		} else {
			logger.Info().Str("pattern", actionValue.Remember).Msg("saved permission rule")
		}
	}

	// Clear pending state
	h.pendingPermissions.Delete(actionValue.ThreadKey)

	// Update the permission message to show it was handled
	h.updatePermissionMessage(pending, actionValue.Behavior, callback.User.ID, actionValue.Remember)
}

// updatePermissionMessage updates a permission prompt message to show the result.
func (h *Handler) updatePermissionMessage(perm *PendingPermission, behavior, userID, remembered string) {
	var emoji, action string
	if behavior == "allow" {
		emoji = ":white_check_mark:"
		action = "Allowed"
	} else {
		emoji = ":x:"
		action = "Denied"
	}

	// Build updated blocks showing the decision with tool details preserved
	blocks := []slack.Block{}

	// Result header (includes remembered pattern if set)
	var headerStr string
	if remembered != "" {
		headerStr = fmt.Sprintf("%s *%s* by <@%s>\n:brain: Remembered: `%s`", emoji, action, userID, remembered)
	} else {
		headerStr = fmt.Sprintf("%s *%s* by <@%s>", emoji, action, userID)
	}
	headerText := slack.NewTextBlockObject("mrkdwn", headerStr, false, false)
	blocks = append(blocks, slack.NewSectionBlock(headerText, nil, nil))

	// Tool name
	toolText := slack.NewTextBlockObject(
		"mrkdwn",
		fmt.Sprintf("*Tool:* `%s`", perm.ToolName),
		false,
		false,
	)
	blocks = append(blocks, slack.NewSectionBlock(toolText, nil, nil))

	// Tool-specific details (same logic as buildPermissionBlocks)
	var detailText string
	switch perm.ToolName {
	case "Bash":
		if cmd, ok := perm.ToolInput["command"].(string); ok {
			if len(cmd) > 500 {
				cmd = cmd[:500] + "..."
			}
			detailText = fmt.Sprintf("*Command:*\n```%s```", cmd)
		}
	case "Write", "Edit":
		if path, ok := perm.ToolInput["file_path"].(string); ok {
			detailText = fmt.Sprintf("*File:* `%s`", path)
		}
	case "Read":
		if path, ok := perm.ToolInput["file_path"].(string); ok {
			detailText = fmt.Sprintf("*File:* `%s`", path)
		}
	case "WebFetch":
		if url, ok := perm.ToolInput["url"].(string); ok {
			detailText = fmt.Sprintf("*URL:* %s", url)
		}
	case "WebSearch":
		if query, ok := perm.ToolInput["query"].(string); ok {
			detailText = fmt.Sprintf("*Query:* `%s`", query)
		}
	default:
		var parts []string
		for k, v := range perm.ToolInput {
			parts = append(parts, fmt.Sprintf("*%s:* `%v`", k, v))
		}
		detailText = strings.Join(parts, "\n")
	}

	if detailText != "" {
		detailBlock := slack.NewTextBlockObject("mrkdwn", detailText, false, false)
		blocks = append(blocks, slack.NewSectionBlock(detailBlock, nil, nil))
	}

	if err := h.bot.UpdateMessageBlocks(perm.ChannelID, perm.MessageTS, blocks); err != nil {
		h.logger.Error().Err(err).Msg("failed to update permission message")
	}
}

// parsePermissionResponse interprets user input as a permission decision.
func parsePermissionResponse(text string) *PermissionResponse {
	lower := strings.ToLower(strings.TrimSpace(text))

	switch lower {
	case "yes", "y", "allow", "ok", "approve", "approved", "accept", "yep", "yeah", "sure":
		return &PermissionResponse{Behavior: "allow"}
	case "no", "n", "deny", "denied", "reject", "rejected", "nope", "nah":
		return &PermissionResponse{Behavior: "deny", Message: "User denied permission"}
	default:
		return nil
	}
}

// isPermissionAllowed checks if a permission request matches saved allowed rules.
// This enables "remember" to take effect immediately within the same session.
func (h *Handler) isPermissionAllowed(taskPath, toolName string, toolInput map[string]any) bool {
	configPath := filepath.Join(taskPath, ".clod", "claude", "claude.json")

	data, err := os.ReadFile(configPath)
	if err != nil {
		return false
	}

	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return false
	}

	projects, ok := config["projects"].(map[string]any)
	if !ok {
		return false
	}

	project, ok := projects[taskPath].(map[string]any)
	if !ok {
		return false
	}

	allowedTools, ok := project["allowedTools"].([]any)
	if !ok {
		return false
	}

	for _, rule := range allowedTools {
		ruleStr, ok := rule.(string)
		if !ok {
			continue
		}

		if matchesPermissionRule(ruleStr, toolName, toolInput) {
			return true
		}
	}

	return false
}

// matchesPermissionRule checks if a tool request matches a permission rule.
// Rules can be:
//   - "ToolName" - matches all uses of that tool
//   - "ToolName(pattern:*)" - matches tool with pattern prefix (e.g., "Bash(python:*)")
func matchesPermissionRule(rule, toolName string, toolInput map[string]any) bool {
	// Exact tool match (e.g., "WebSearch" matches any WebSearch)
	if rule == toolName {
		return true
	}

	// Pattern match (e.g., "Bash(python:*)" matches "python3 -m venv venv")
	if strings.HasPrefix(rule, toolName+"(") && strings.HasSuffix(rule, ")") {
		pattern := rule[len(toolName)+1 : len(rule)-1] // Extract "python:*" from "Bash(python:*)"

		// Parse the pattern.
		if strings.HasSuffix(pattern, ":*") {
			prefix := strings.TrimSuffix(pattern, ":*")

			// For Bash, check command prefix.
			if toolName == "Bash" {
				if cmd, ok := toolInput["command"].(string); ok {
					parts := strings.Fields(cmd)
					if len(parts) > 0 && parts[0] == prefix {
						return true
					}
				}
			}

			// For file operations, check path prefix.
			if toolName == "Write" || toolName == "Edit" || toolName == "Read" {
				if path, ok := toolInput["file_path"].(string); ok {
					// Check if path is under the specified directory.
					if strings.Contains(path, "/"+prefix+"/") || strings.HasPrefix(path, prefix+"/") {
						return true
					}
				}
			}
		}

		// Glob pattern (e.g., "Write(src/**)")
		if strings.HasSuffix(pattern, "**") {
			dirPrefix := strings.TrimSuffix(pattern, "**")
			if toolName == "Write" || toolName == "Edit" || toolName == "Read" {
				if path, ok := toolInput["file_path"].(string); ok {
					if strings.Contains(path, "/"+dirPrefix) || strings.HasPrefix(path, dirPrefix) {
						return true
					}
				}
			}
		}
	}

	return false
}

// TaskStats represents the statistics from a completed task.
type TaskStats struct {
	IsError    bool    `json:"is_error"`
	DurationMS int     `json:"duration_ms"`
	NumTurns   int     `json:"num_turns"`
	CostUSD    float64 `json:"cost_usd"`
}

// postStatsMessage posts a formatted stats message using Slack blocks.
func (h *Handler) postStatsMessage(channelID, threadTS, statsJSON string) {
	var stats TaskStats
	if err := json.Unmarshal([]byte(statsJSON), &stats); err != nil {
		h.logger.Error().Err(err).Str("json", statsJSON).Msg("failed to parse stats JSON")
		return
	}

	// Format duration.
	duration := time.Duration(stats.DurationMS) * time.Millisecond
	var durationStr string
	if duration >= time.Minute {
		durationStr = fmt.Sprintf("%dm %ds", int(duration.Minutes()), int(duration.Seconds())%60)
	} else {
		durationStr = fmt.Sprintf("%.1fs", duration.Seconds())
	}

	// Format cost.
	costStr := fmt.Sprintf("$%.4f", stats.CostUSD)

	// Build blocks with fields for table-like layout.
	blocks := []slack.Block{}

	// Status emoji based on error state.
	var statusEmoji string
	if stats.IsError {
		statusEmoji = ":warning:"
	} else {
		statusEmoji = ":chart_with_upwards_trend:"
	}

	// Use context block for compact inline display.
	contextElements := []slack.MixedElement{
		slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("%s *Task Stats*", statusEmoji), false, false),
		slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("⏱️ %s", durationStr), false, false),
		slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("🔄 %d turns", stats.NumTurns), false, false),
		slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("💰 %s", costStr), false, false),
	}

	contextBlock := slack.NewContextBlock("", contextElements...)
	blocks = append(blocks, contextBlock)

	// Post the stats message.
	if _, err := h.bot.PostMessageBlocks(channelID, blocks, threadTS); err != nil {
		h.logger.Error().Err(err).Msg("failed to post stats message")
	}
}

// formatBytes formats a byte count as a human-readable string.
func formatBytes(bytes int) string {
	const (
		KB = 1024
		MB = KB * 1024
	)
	switch {
	case bytes >= MB:
		return fmt.Sprintf("%.1fMB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.1fKB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d bytes", bytes)
	}
}

// postToolSnippet posts a tool result as a summary line with attached collapsible snippet.
func (h *Handler) postToolSnippet(channelID, threadTS, toolName, inputJSON, content string, logger zerolog.Logger) {
	contentLen := len(content)
	lineCount := strings.Count(content, "\n") + 1

	// Parse input JSON to extract tool-specific parameters.
	var input map[string]any
	json.Unmarshal([]byte(inputJSON), &input)

	// Helper to get string from input.
	getString := func(key string) string {
		if v, ok := input[key].(string); ok {
			return v
		}
		return ""
	}

	// Build summary line with tool-specific context.
	var summary string
	var snippetTitle string
	switch toolName {
	case "Read":
		filePath := getString("file_path")
		if filePath != "" {
			// Shorten path for display.
			shortPath := filePath
			if len(shortPath) > 50 {
				shortPath = "..." + shortPath[len(shortPath)-47:]
			}
			summary = fmt.Sprintf(":page_facing_up: `%s` `%s` (%s, %d lines)", toolName, shortPath, formatBytes(contentLen), lineCount)
			snippetTitle = filepath.Base(filePath)
		} else {
			summary = fmt.Sprintf(":page_facing_up: `%s` (%s, %d lines)", toolName, formatBytes(contentLen), lineCount)
			snippetTitle = "Read output"
		}
	case "Grep":
		pattern := getString("pattern")
		if pattern != "" {
			summary = fmt.Sprintf(":mag: `%s` `%s` (%d lines)", toolName, pattern, lineCount)
			snippetTitle = fmt.Sprintf("grep %s", pattern)
		} else {
			summary = fmt.Sprintf(":mag: `%s` (%d lines)", toolName, lineCount)
			snippetTitle = "Grep output"
		}
	case "Glob":
		pattern := getString("pattern")
		if pattern != "" {
			summary = fmt.Sprintf(":file_folder: `%s` `%s` (%d files)", toolName, pattern, lineCount)
			snippetTitle = fmt.Sprintf("glob %s", pattern)
		} else {
			summary = fmt.Sprintf(":file_folder: `%s` (%d files)", toolName, lineCount)
			snippetTitle = "Glob output"
		}
	case "Bash":
		command := getString("command")
		if command != "" {
			// Truncate long commands.
			shortCmd := command
			if len(shortCmd) > 60 {
				shortCmd = shortCmd[:57] + "..."
			}
			summary = fmt.Sprintf(":computer: `%s` `%s` (%s)", toolName, shortCmd, formatBytes(contentLen))
			snippetTitle = "bash output"
		} else {
			summary = fmt.Sprintf(":computer: `%s` (%s)", toolName, formatBytes(contentLen))
			snippetTitle = "Bash output"
		}
	case "WebSearch":
		query := getString("query")
		if query != "" {
			summary = fmt.Sprintf(":globe_with_meridians: `%s` `%s` (%s)", toolName, query, formatBytes(contentLen))
			snippetTitle = fmt.Sprintf("search %s", query)
		} else {
			summary = fmt.Sprintf(":globe_with_meridians: `%s` (%s)", toolName, formatBytes(contentLen))
			snippetTitle = "WebSearch output"
		}
	case "WebFetch":
		url := getString("url")
		if url != "" {
			// Shorten URL for display.
			shortURL := url
			if len(shortURL) > 50 {
				shortURL = shortURL[:47] + "..."
			}
			summary = fmt.Sprintf(":inbox_tray: `%s` `%s` (%s)", toolName, shortURL, formatBytes(contentLen))
			snippetTitle = "fetch output"
		} else {
			summary = fmt.Sprintf(":inbox_tray: `%s` (%s)", toolName, formatBytes(contentLen))
			snippetTitle = "WebFetch output"
		}
	default:
		summary = fmt.Sprintf(":gear: `%s` (%s)", toolName, formatBytes(contentLen))
		snippetTitle = fmt.Sprintf("%s output", toolName)
	}

	// Upload content as collapsible snippet with summary as the comment.
	// This keeps the summary and expandable content together in one message.
	if _, err := h.bot.files.UploadSnippet(content, snippetTitle, summary, channelID, threadTS); err != nil {
		logger.Error().Err(err).Str("tool", toolName).Msg("failed to upload tool snippet")
	}
}
