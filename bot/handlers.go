package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// PendingPermission tracks a permission request waiting for user response.
type PendingPermission struct {
	MessageTS          string // Timestamp of the permission prompt message (for updating)
	ChannelID          string
	ThreadTS           string
	ToolName           string         // Tool that requested permission
	ToolInput          map[string]any // Tool input parameters (for display)
	ControlRequestID   string         // Request ID for control message permissions (empty for MCP)
	IsControlPermission bool          // True if this is a control message permission
}

// Handler processes Slack events.
type Handler struct {
	bot    *Bot
	logger zerolog.Logger

	// Track running tasks by thread key ("channelID:threadTS")
	runningTasks sync.Map // key -> *RunningTask

	// Track threads waiting for permission responses
	pendingPermissions sync.Map // key -> *PendingPermission

	// Track ambiguous-response prompts posted in place of the text reminder.
	// key -> *pendingAmbiguous (the user's original text + Slack message TS
	// of the prompt itself, so we can update it after a button click).
	pendingAmbiguous sync.Map

	// Track in-flight AskUserQuestion prompts awaiting Submit.
	// key (progressKey) -> *askUserQuestionState
	askQuestionStates sync.Map

	// Tools affected by verbosity toggle (controlled by VERBOSE_TOOLS env var)
	verboseTools map[string]bool

	// lastVerboseToolMsg tracks the most recent verbose tool summary message per thread
	// so consecutive summaries can be edited in-place rather than posted as new messages.
	lastVerboseToolMsg sync.Map // key -> string (messageTS)

	// lastOutputMsg tracks the most recent output message per thread for consolidation.
	// Consecutive outputs are edited into the same message to reduce notification noise.
	lastOutputMsg sync.Map // key -> *LastOutputMsg

	defaultVerbosityLevel int

	// defaultModel is the fallback model passed to `claude --model` when a
	// thread has no stored preference. Empty string lets claude use its own
	// built-in default.
	defaultModel string
}

// LastOutputMsg tracks the last output message for consolidation.
type LastOutputMsg struct {
	MessageTS string    // Slack message timestamp
	Content   string    // Current message content
	UpdatedAt time.Time // When the message was last updated
}

// pendingAmbiguous tracks an outstanding "ambiguous response" prompt — the
// block message we post when the user types something during a pending
// permission that doesn't parse as yes/no. Storing the Text server-side
// avoids the 2000-char limit on Slack button action values, and MessageTS
// lets us rewrite the prompt with the outcome after a button click.
type pendingAmbiguous struct {
	Text      string
	MessageTS string
	ChannelID string
	ThreadTS  string
	UserID    string
}

const (
	verbosityEmoji = "speech_balloon" // 💬 level 1 (full)
	seeNoEvilEmoji = "see_no_evil"    // 🙈 level -1 (silent)

	// Model-indicator emojis. Bot adds its own reaction to the task's
	// status message to show which model is active; a user reacting with a
	// different model emoji switches the active model for the thread.
	opusEmoji   = "musical_score" // 🎼 Opus
	sonnetEmoji = "scroll"        // 📜 Sonnet
	haikuEmoji  = "cherry_blossom" // 🌸 Haiku

	// Default model string when no thread preference exists and no bot
	// default is set. Matches a common --model alias.
	fallbackModel = "sonnet"

	// Message consolidation settings
	maxConsolidationAge = 1 * time.Minute // Start new message after this duration
	maxMessageLen       = 3500            // Slack truncates around 4000, leave buffer
)

// modelEmojis maps model strings (as accepted by `claude --model`) to the
// Slack emoji used for their reaction indicator.
var modelEmojis = map[string]string{
	"opus":               opusEmoji,
	"sonnet":             sonnetEmoji,
	"claude-haiku-4-5":   haikuEmoji,
}

// emojiToModel is the reverse mapping of modelEmojis for reaction handling.
var emojiToModel = map[string]string{
	opusEmoji:   "opus",
	sonnetEmoji: "sonnet",
	haikuEmoji:  "claude-haiku-4-5",
}

// emojiForModel returns the indicator emoji for a model string, falling back
// to the Sonnet emoji for anything we don't recognize (including empty
// "use default").
func emojiForModel(model string) string {
	if e, ok := modelEmojis[model]; ok {
		return e
	}
	return sonnetEmoji
}

// NewHandler creates a new Handler.
func NewHandler(bot *Bot, verboseTools []string, defaultVerbosityLevel int, defaultModel string) *Handler {
	// Build map for O(1) lookup
	verboseToolsMap := make(map[string]bool, len(verboseTools))
	for _, tool := range verboseTools {
		verboseToolsMap[tool] = true
	}

	return &Handler{
		bot:                   bot,
		logger:                bot.logger.With().Str("component", "handler").Logger(),
		verboseTools:          verboseToolsMap,
		defaultVerbosityLevel: defaultVerbosityLevel,
		defaultModel:          defaultModel,
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
		if _, err := h.bot.PostMessage(ev.Channel, h.bot.auth.RejectMessage(), threadTS); err != nil {
			logger.Error().Err(err).Msg("failed to post authorization rejection message")
		}
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
					Bool("is_control", perm.IsControlPermission).
					Msg("received permission response from user (text)")

				if perm.IsControlPermission && perm.ControlRequestID != "" {
					// Send control_response for control message permissions.
					if err := task.SendControlResponse(perm.ControlRequestID, resp.Behavior, resp.Message); err != nil {
						logger.Error().Err(err).Msg("failed to send control response")
					}
				} else {
					// Send via MCP FIFO for traditional permissions.
					task.SendPermissionResponse(*resp)
				}
				h.pendingPermissions.Delete(progressKey)

				// Update the permission message to show it was handled
				h.updatePermissionMessage(perm, resp.Behavior, ev.User, "")
				return
			}
			// Not a clear yes/no. Instead of a plaintext reminder, post a
			// permission-style block message that quotes the user's text and
			// offers buttons to route the intent — approve the pending
			// permission, deny it, or cancel it and redirect the agent with
			// the typed text as new instructions. This matches the style of
			// the original permission prompt and handles the common case
			// (user wants to redirect mid-task, not respond yes/no).
			h.postAmbiguousResponsePrompt(ev.Channel, threadTS, ev.User, ev.Text, progressKey, logger)
			return
		}

		// Send the message as input to Claude
		logger.Debug().Str("input", ev.Text).Msg("sending thread reply to running task")
		// Clear output message consolidation since user sent a message.
		h.lastOutputMsg.Delete(key(ev.Channel, threadTS))
		if err := task.SendInput(ev.Text); err != nil {
			logger.Error().Err(err).Msg("failed to send input to task")
		}
		return
	}

	// No running task, so check if there's a saved session to resume.
	session := h.bot.sessions.Get(ev.Channel, threadTS)
	if session == nil {
		// No session for this thread - stay silent (don't interrupt unrelated conversations)
		logger.Debug().Msg("no running task or saved session for thread, ignoring")
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

	// Clear output message consolidation since user sent a message.
	h.lastOutputMsg.Delete(key(ev.Channel, threadTS))

	// Check for files attached to the message and download them to .clod-runtime/inputs.
	slackFiles, err := h.bot.files.GetThreadReplyFiles(ev.Channel, threadTS, ev.TimeStamp)
	if err != nil {
		logger.Warn().Err(err).Msg("failed to check for thread reply files")
	}

	// Download files to disk for Claude to read.
	var downloadedFiles []string
	if len(slackFiles) > 0 {
		if _, err := h.bot.PostMessage(
			ev.Channel,
			fmt.Sprintf(":inbox_tray: Downloading %d file(s)...", len(slackFiles)),
			threadTS,
		); err != nil {
			logger.Error().Err(err).Msg("failed to post file download message")
		}
		for _, file := range slackFiles {
			localPath, err := h.bot.files.DownloadToTask(file, session.TaskPath)
			if err != nil {
				logger.Error().Err(err).Str("file_id", file.ID).Msg("failed to download file")
				if _, postErr := h.bot.PostMessage(
					ev.Channel,
					fmt.Sprintf(":warning: Failed to download `%s`: %v", file.Name, err),
					threadTS,
				); postErr != nil {
					logger.Error().Err(postErr).Msg("failed to post download error message")
				}
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
	if _, err := h.bot.PostMessage(
		ev.Channel,
		fmt.Sprintf(":arrows_counterclockwise: Resuming task `%s`...", session.TaskName),
		threadTS,
	); err != nil {
		logger.Error().Err(err).Msg("failed to post resume task message")
	}

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

// HandleReactionAdded processes reaction_added events.
func (h *Handler) HandleReactionAdded(ctx context.Context, ev *slackevents.ReactionAddedEvent) {
	// Model-switch reactions fall through to a dedicated handler since they
	// don't fit the verbosity level/message shape.
	if _, ok := emojiToModel[ev.Reaction]; ok {
		h.handleModelReaction(ctx, ev)
		return
	}

	var level int
	var message string
	switch ev.Reaction {
	case verbosityEmoji:
		level = 1
		message = ":speech_balloon: Verbose mode enabled - tool outputs will include full content."
	case seeNoEvilEmoji:
		level = -1
		message = ":see_no_evil: Silent mode enabled - verbose tool outputs will be hidden."
	default:
		return
	}

	logger := h.logger.With().
		Str("channel", ev.Item.Channel).
		Str("item_ts", ev.Item.Timestamp).
		Str("user", ev.User).
		Str("reaction", ev.Reaction).
		Int("level", level).
		Logger()

	// Determine thread TS - the reacted item could be the thread root or a reply.
	threadTS, err := h.getThreadTS(ev.Item.Channel, ev.Item.Timestamp)
	if err != nil {
		logger.Debug().Err(err).Msg("failed to get thread TS for reaction")
		return
	}

	logger = logger.With().Str("thread_ts", threadTS).Logger()

	// A non-empty SessionID means the bot was explicitly invoked in this thread.
	session := h.bot.sessions.Get(ev.Item.Channel, threadTS)
	hasActiveTask := session != nil && session.SessionID != ""

	if hasActiveTask {
		logger.Info().Msg("setting verbosity level for active thread")
	} else {
		logger.Debug().Msg("setting verbosity level for inactive thread (no confirmation will be posted)")
	}

	h.bot.sessions.SetVerbosityLevel(ev.Item.Channel, threadTS, level)

	if err := h.bot.sessions.Save(); err != nil {
		logger.Error().Err(err).Msg("failed to save session after verbosity change")
	}

	// Don't post a confirmation in threads that haven't explicitly invoked the bot.
	if hasActiveTask {
		if _, err := h.bot.PostMessage(
			ev.Item.Channel,
			message,
			threadTS,
		); err != nil {
			logger.Debug().Err(err).Msg("failed to post verbosity confirmation")
		}
	}
}

// handleModelReaction updates the per-thread model preference when a user
// reacts with one of the model-indicator emojis. It moves the bot's own
// reaction on the status message so "active model" stays accurate. The new
// model applies on the next invocation of clod in this thread (claude can't
// change models mid-session in stream-json mode).
func (h *Handler) handleModelReaction(ctx context.Context, ev *slackevents.ReactionAddedEvent) {
	newModel, ok := emojiToModel[ev.Reaction]
	if !ok {
		return
	}

	logger := h.logger.With().
		Str("channel", ev.Item.Channel).
		Str("item_ts", ev.Item.Timestamp).
		Str("user", ev.User).
		Str("reaction", ev.Reaction).
		Str("new_model", newModel).
		Logger()

	threadTS, err := h.getThreadTS(ev.Item.Channel, ev.Item.Timestamp)
	if err != nil {
		logger.Debug().Err(err).Msg("failed to get thread TS for model reaction")
		return
	}
	logger = logger.With().Str("thread_ts", threadTS).Logger()

	session := h.bot.sessions.Get(ev.Item.Channel, threadTS)
	// Only react inside threads the bot has been invoked in. A model emoji
	// in an unrelated thread is noise.
	if session == nil || session.TaskName == "" {
		logger.Debug().Msg("ignoring model reaction outside a bot-initiated thread")
		return
	}

	oldModel := session.Model
	if oldModel == newModel {
		logger.Debug().Msg("model reaction matches current model; nothing to do")
		return
	}

	h.bot.sessions.SetModel(ev.Item.Channel, threadTS, newModel)
	if err := h.bot.sessions.Save(); err != nil {
		logger.Error().Err(err).Msg("failed to save session after model change")
	}

	// Move the bot's own indicator reaction on the task's anchor message
	// (the user's @-mention that kicked off the task).
	if session.ReactionAnchorTS != "" {
		if oldModel != "" && oldModel != newModel {
			if err := h.bot.RemoveReaction(session.ChannelID, session.ReactionAnchorTS, emojiForModel(oldModel)); err != nil {
				logger.Debug().Err(err).Msg("failed to remove old model reaction")
			}
		}
		if err := h.bot.AddReaction(session.ChannelID, session.ReactionAnchorTS, emojiForModel(newModel)); err != nil {
			logger.Debug().Err(err).Msg("failed to add new model reaction")
		}
	}

	logger.Info().Str("old_model", oldModel).Msg("switched thread model")

	// Post a confirmation so the user sees the switch landed. Next-turn
	// semantics are important context.
	msg := fmt.Sprintf(":%s: Model set to `%s` — takes effect on the next message you send in this thread.",
		ev.Reaction, newModel)
	if _, err := h.bot.PostMessage(session.ChannelID, msg, threadTS); err != nil {
		logger.Debug().Err(err).Msg("failed to post model switch confirmation")
	}
}

// HandleReactionRemoved processes reaction_removed events.
func (h *Handler) HandleReactionRemoved(ctx context.Context, ev *slackevents.ReactionRemovedEvent) {
	// Only handle verbosity-related emojis.
	if ev.Reaction != verbosityEmoji && ev.Reaction != seeNoEvilEmoji {
		return
	}

	logger := h.logger.With().
		Str("channel", ev.Item.Channel).
		Str("item_ts", ev.Item.Timestamp).
		Str("user", ev.User).
		Str("reaction", ev.Reaction).
		Logger()

	// Determine thread TS.
	threadTS, err := h.getThreadTS(ev.Item.Channel, ev.Item.Timestamp)
	if err != nil {
		logger.Debug().Err(err).Msg("failed to get thread TS for reaction")
		return
	}

	logger = logger.With().Str("thread_ts", threadTS).Logger()

	// Check if this thread has an active task session before posting messages.
	// A session with a SessionID means the bot was explicitly invoked.
	session := h.bot.sessions.Get(ev.Item.Channel, threadTS)
	hasActiveTask := session != nil && session.SessionID != ""

	level := h.getThreadVerbosityFromReactions(ev.Item.Channel, threadTS, logger)

	if hasActiveTask {
		logger.Info().Int("level", level).Msg("updating verbosity level for active thread after reaction removal")
	} else {
		logger.Debug().Int("level", level).Msg("updating verbosity level for inactive thread (no confirmation will be posted)")
	}

	h.bot.sessions.SetVerbosityLevel(ev.Item.Channel, threadTS, level)

	if err := h.bot.sessions.Save(); err != nil {
		logger.Error().Err(err).Msg("failed to save session after verbosity change")
	}

	// Don't post a confirmation in threads that haven't explicitly invoked the bot.
	if hasActiveTask {
		var message string
		switch level {
		case -1:
			message = ":see_no_evil: Silent mode - verbose tool outputs hidden."
		case 0:
			message = ":mute: Summary mode - tool outputs show summaries only."
		case 1:
			message = ":speech_balloon: Verbose mode - tool outputs include full content."
		}

		if _, err := h.bot.PostMessage(
			ev.Item.Channel,
			message,
			threadTS,
		); err != nil {
			logger.Debug().Err(err).Msg("failed to post verbosity confirmation")
		}
	}
}

// getThreadTS determines the thread timestamp for a message.
// If the message is a thread reply, returns the thread_ts.
// If the message is a thread root, returns the message ts.
func (h *Handler) getThreadTS(channelID, messageTS string) (string, error) {
	// Fetch the message to check if it's in a thread
	params := &slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Latest:    messageTS,
		Oldest:    messageTS,
		Inclusive: true,
		Limit:     1,
	}

	history, err := h.bot.client.GetConversationHistory(params)
	if err != nil {
		return "", err
	}

	if len(history.Messages) == 0 {
		// Message not found, assume it's the thread root
		return messageTS, nil
	}

	msg := history.Messages[0]
	if msg.ThreadTimestamp != "" {
		// Message is a reply in a thread
		return msg.ThreadTimestamp, nil
	}

	// Message is a thread root (or standalone)
	return messageTS, nil
}

// getThreadVerbosityFromReactions returns the least verbose verbosity level
// found across all reactions in the thread, or the store default if none are found.
func (h *Handler) getThreadVerbosityFromReactions(channelID, threadTS string, logger zerolog.Logger) int {
	params := &slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTS,
		Limit:     1000,
	}

	msgs, _, _, err := h.bot.client.GetConversationReplies(params)
	if err != nil {
		logger.Debug().Err(err).Msg("failed to get thread replies for reaction check")
		return h.defaultVerbosityLevel
	}

	leastVerbose := h.defaultVerbosityLevel
	hasVerbosityReaction := false

	for _, msg := range msgs {
		for _, reaction := range msg.Reactions {
			switch reaction.Name {
			case seeNoEvilEmoji:
				return -1
			case verbosityEmoji:
				hasVerbosityReaction = true
				if leastVerbose < 1 {
					leastVerbose = 1
				}
			}
		}
	}

	if hasVerbosityReaction {
		return leastVerbose
	}
	return h.defaultVerbosityLevel
}

// handleNewTask processes a new task request.
func (h *Handler) handleNewTask(
	ctx context.Context,
	ev *slackevents.AppMentionEvent,
	threadTS string,
	logger zerolog.Logger,
) {
	parsed := ParseMention(ev.Text)
	if parsed == nil {
		logger.Debug().Msg("no valid command format in message, ignoring")
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
		if _, postErr := h.bot.PostMessage(ev.Channel, msg, threadTS); postErr != nil {
			logger.Error().Err(postErr).Msg("failed to post unknown task message")
		}
		return
	}

	logger.Info().Str("task_path", taskPath).Msg("starting new task")

	// Clear output message consolidation for this thread.
	h.lastOutputMsg.Delete(key(ev.Channel, threadTS))

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
		if _, err := h.bot.PostMessage(
			ev.Channel,
			fmt.Sprintf(":inbox_tray: Downloading %d file(s)...", len(slackFiles)),
			threadTS,
		); err != nil {
			logger.Error().Err(err).Msg("failed to post file download message")
		}
		for _, file := range slackFiles {
			localPath, err := h.bot.files.DownloadToTask(file, taskPath)
			if err != nil {
				logger.Error().Err(err).Str("file_id", file.ID).Msg("failed to download file")
				if _, postErr := h.bot.PostMessage(
					ev.Channel,
					fmt.Sprintf(":warning: Failed to download `%s`: %v", file.Name, err),
					threadTS,
				); postErr != nil {
					logger.Error().Err(postErr).Msg("failed to post download error message")
				}
				continue
			}
			logger.Info().
				Str("file_id", file.ID).
				Str("local_path", localPath).
				Msg("file downloaded to task inputs")
			downloadedFiles = append(downloadedFiles, localPath)
		}
	}

	// Gather prior thread messages as context for new sessions.
	threadContext := h.gatherThreadContext(ev.Channel, threadTS, ev.TimeStamp, logger)

	prompt := ""
	if threadContext != "" {
		prompt = threadContext
	}
	prompt += parsed.Instructions
	if len(downloadedFiles) > 0 {
		prompt += "\n\nAttached files have been saved to:\n"
		for _, path := range downloadedFiles {
			prompt += fmt.Sprintf("- %s\n", path)
		}
	}

	// Post initial status with verbosity + model info
	startMsg := fmt.Sprintf(":rocket: Starting a `%s` task...\n\n"+
		"_Verbosity: React with 🙈 for silent, 💬 for full output (including thinking)_\n"+
		"_Model: 🎼 Opus / 📜 Sonnet / 🌸 Haiku_",
		parsed.TaskName)
	if _, err := h.bot.PostMessage(
		ev.Channel,
		startMsg,
		threadTS,
	); err != nil {
		logger.Error().Err(err).Msg("failed to post task start message")
	}

	// Anchor the model-indicator reaction on the user's @-mention — that's
	// the message that actually kicked off the task — rather than on the
	// bot's "Starting..." status post.
	initialModel := h.bot.sessions.GetModel(ev.Channel, threadTS)
	if initialModel == "" {
		initialModel = h.defaultModel
	}
	if initialModel == "" {
		initialModel = fallbackModel
	}
	session := h.bot.sessions.Get(ev.Channel, threadTS)
	if session == nil {
		session = &SessionMapping{
			ChannelID: ev.Channel,
			ThreadTS:  threadTS,
			TaskName:  parsed.TaskName,
			TaskPath:  taskPath,
			UserID:    ev.User,
			CreatedAt: time.Now(),
		}
	}
	session.ReactionAnchorTS = ev.TimeStamp
	session.Model = initialModel
	h.bot.sessions.Set(session)
	if err := h.bot.sessions.Save(); err != nil {
		logger.Error().Err(err).Msg("failed to save session after status post")
	}
	if err := h.bot.AddReaction(ev.Channel, ev.TimeStamp, emojiForModel(initialModel)); err != nil {
		logger.Debug().Err(err).Msg("failed to add model reaction")
	}

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
		if _, err := h.bot.PostMessage(ev.Channel, "Please provide instructions for the task.", threadTS); err != nil {
			logger.Error().Err(err).Msg("failed to post empty instructions message")
		}
		return
	}

	logger = logger.With().
		Str("task", session.TaskName).
		Str("session_id", session.SessionID).
		Str("instructions", instructions).
		Logger()

	logger.Info().Msg("continuing existing session")

	// Post initial status
	if _, err := h.bot.PostMessage(
		ev.Channel,
		fmt.Sprintf(":arrows_counterclockwise: Continuing task `%s`...", session.TaskName),
		threadTS,
	); err != nil {
		logger.Error().Err(err).Msg("failed to post continue task message")
	}

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
	// Honor any per-thread model preference saved via reaction.
	model := h.bot.sessions.GetModel(channelID, threadTS)
	if model == "" {
		model = h.defaultModel
	}

	// Start the task
	task, err := h.bot.runner.Start(ctx, taskPath, prompt, sessionID, model)
	if err != nil {
		logger.Error().Err(err).Msg("failed to start clod")
		if _, postErr := h.bot.PostMessage(channelID, fmt.Sprintf(":x: Failed to start task: %v", err), threadTS); postErr != nil {
			logger.Error().Err(postErr).Msg("failed to post task start error message")
		}
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

	// Function to flush the buffer with message consolidation.
	threadKey := key(channelID, threadTS)
	flushBuffer := func() {
		if outputBuffer.Len() > 0 {
			// Convert GitHub-flavored markdown to Slack's mrkdwn format.
			newContent := strings.TrimSpace(outputBuffer.String())
			if newContent != "" {
				newContent = ConvertMarkdownToMrkdwn(newContent)

				// Check if we can consolidate with the previous message.
				var posted bool
				if lastVal, ok := h.lastOutputMsg.Load(threadKey); ok {
					last := lastVal.(*LastOutputMsg)
					age := time.Since(last.UpdatedAt)

					// Smart join: don't add separator if content already has appropriate whitespace.
					// This preserves intentional paragraph breaks while not breaking mid-sentence output.
					var separator string
					if last.Content != "" && newContent != "" {
						prevEndsWithNewline := strings.HasSuffix(last.Content, "\n")
						newStartsWithNewline := strings.HasPrefix(newContent, "\n")
						// Fence boundary: TrimSpace in flushBuffer strips the
						// blank-line padding the mrkdwn renderer emits, so a
						// chunk that ended with a closing ``` loses the newline
						// that was supposed to separate it from the next chunk.
						// Consolidating without a blank-line separator produces
						// "``````" (six literal backticks) on Slack when both
						// sides are fences, or a closing fence glued to plain
						// text (never terminates) when one side is text.
						prevEndsWithFence := strings.HasSuffix(last.Content, "```")
						newStartsWithFence := strings.HasPrefix(newContent, "```")
						switch {
						case prevEndsWithFence || newStartsWithFence:
							separator = "\n\n"
						case !prevEndsWithNewline && !newStartsWithNewline:
							// Neither has newline at boundary - streaming text, concatenate directly
							separator = ""
						}
						// Otherwise no separator needed (one side already has the newline)
					}

					combinedLen := len(last.Content) + len(separator) + len(newContent)

					// Edit existing message if: recent enough AND combined size fits
					if age < maxConsolidationAge && combinedLen <= maxMessageLen {
						combined := last.Content + separator + newContent
						if err := h.bot.UpdateMessage(channelID, last.MessageTS, combined); err != nil {
							logger.Debug().Err(err).Msg("failed to update consolidated message, posting new")
						} else {
							// Update tracking with new content and time.
							last.Content = combined
							last.UpdatedAt = time.Now()
							posted = true
							logger.Debug().
								Int("combined_len", len(combined)).
								Dur("age", age).
								Msg("consolidated output into existing message")
						}
					}
				}

				// Post new message if consolidation didn't happen.
				if !posted {
					if msgTS, err := h.bot.PostMessage(channelID, newContent, threadTS); err != nil {
						logger.Debug().Err(err).Msg("failed to post output message")
					} else {
						// Track this as the new last message.
						h.lastOutputMsg.Store(threadKey, &LastOutputMsg{
							MessageTS: msgTS,
							Content:   newContent,
							UpdatedAt: time.Now(),
						})
						// Clear last verbose tool message since we posted non-verbose content.
						h.lastVerboseToolMsg.Delete(threadKey)
					}
				}
			}
			outputBuffer.Reset()
		}
	}

	// Get permission request channels (may be nil if not available)
	permRequests := task.PermissionRequests()
	ctrlPermRequests := task.ControlPermissionRequests()
	sessionCaptured := task.SessionIDCaptured()

	// Process output and wait for completion
	for {
		select {
		case sid, ok := <-sessionCaptured:
			if ok && sid != "" {
				// Persist the thread → session mapping immediately instead of
				// waiting for task completion. Long-running tasks used to
				// leave the thread orphaned if the bot restarted mid-task
				// (sessions.json only got written on task.Done()).
				session := &SessionMapping{
					ChannelID: channelID,
					ThreadTS:  threadTS,
					TaskName:  taskName,
					TaskPath:  taskPath,
					SessionID: sid,
					UserID:    userID,
					CreatedAt: time.Now(),
				}
				h.bot.sessions.Set(session)
				if err := h.bot.sessions.Save(); err != nil {
					logger.Error().Err(err).Msg("failed to save session on capture")
				} else {
					logger.Info().
						Str("session_id", sid).
						Msg("persisted session mapping on capture")
				}
			}
			// Set to nil so this select case stops firing; notifySessionID
			// is one-shot but the channel stays open for the task lifetime.
			sessionCaptured = nil

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
				// Clear consolidation since stats message breaks the chain.
				h.lastOutputMsg.Delete(threadKey)
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
				// Clear consolidation since snippet breaks the chain.
				h.lastOutputMsg.Delete(threadKey)
				continue
			}

			// Check for thinking message (only show at verbosity level 1).
			if strings.HasPrefix(content, "__THINKING__") {
				verbosityLevel := h.bot.sessions.GetVerbosityLevel(channelID, threadTS)
				if verbosityLevel < 1 {
					// Skip thinking output at verbosity levels -1 (silent) and 0 (summary).
					logger.Debug().Msg("skipping thinking output (verbosity < 1)")
					continue
				}
				// Strip prefix and show thinking content.
				content = strings.TrimPrefix(content, "__THINKING__")
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
				var msgTS string
				// Special case: AskUserQuestion gets a CLI-style picker.
				if custom := h.tryPostAskUserQuestionPrompt(req, channelID, threadTS, progressKey, logger); custom != "" {
					msgTS = custom
				} else {
					blocks := h.buildPermissionBlocks(req, progressKey)
					var err error
					msgTS, err = h.bot.PostMessageBlocks(channelID, blocks, threadTS)
					if err != nil {
						logger.Error().Err(err).Msg("failed to post permission prompt")
						// Send deny on failure to post.
						task.SendPermissionResponse(
							PermissionResponse{Behavior: "deny", Message: "Failed to prompt user"},
						)
						continue
					}
				}

				// Track the pending permission with its message timestamp and tool details.
				h.pendingPermissions.Store(progressKey, &PendingPermission{
					MessageTS: msgTS,
					ChannelID: channelID,
					ThreadTS:  threadTS,
					ToolName:  req.ToolName,
					ToolInput: req.ToolInput,
				})

				// Clear consolidation since permission prompt breaks the chain.
				h.lastOutputMsg.Delete(threadKey)

				logger.Info().
					Str("tool_name", req.ToolName).
					Str("tool_use_id", req.ToolUseID).
					Str("message_ts", msgTS).
					Msg("posted permission prompt to slack, waiting for response (MCP)")
			}

		case req, ok := <-ctrlPermRequests:
			if ok {
				// Handle permission requests from control messages (newer protocol).
				// Check if this permission is already allowed by saved rules.
				if h.isPermissionAllowed(task.taskPath, req.ToolName, req.ToolInput) {
					logger.Info().
						Str("tool_name", req.ToolName).
						Msg("auto-allowing control permission based on saved rule")
					if err := task.SendControlResponse(task.pendingControlRequestID, "allow", ""); err != nil {
						logger.Error().Err(err).Msg("failed to send auto-allow control response")
					}
					continue
				}

				// Post formatted permission prompt with buttons to Slack.
				flushBuffer() // Flush any pending output first.
				var msgTS string
				// Special case: AskUserQuestion gets a CLI-style picker.
				if custom := h.tryPostAskUserQuestionPrompt(req, channelID, threadTS, progressKey, logger); custom != "" {
					msgTS = custom
				} else {
					blocks := h.buildPermissionBlocks(req, progressKey)
					var err error
					msgTS, err = h.bot.PostMessageBlocks(channelID, blocks, threadTS)
					if err != nil {
						logger.Error().Err(err).Msg("failed to post control permission prompt")
						// Send deny on failure to post.
						if err := task.SendControlResponse(task.pendingControlRequestID, "deny", "Failed to prompt user"); err != nil {
							logger.Error().Err(err).Msg("failed to send deny control response")
						}
						continue
					}
				}

				// Track the pending permission with its message timestamp, tool details, and control request ID.
				h.pendingPermissions.Store(progressKey, &PendingPermission{
					MessageTS:           msgTS,
					ChannelID:           channelID,
					ThreadTS:            threadTS,
					ToolName:            req.ToolName,
					ToolInput:           req.ToolInput,
					ControlRequestID:    task.pendingControlRequestID,
					IsControlPermission: true,
				})

				// Clear consolidation since permission prompt breaks the chain.
				h.lastOutputMsg.Delete(threadKey)

				logger.Info().
					Str("tool_name", req.ToolName).
					Str("tool_use_id", req.ToolUseID).
					Str("request_id", task.pendingControlRequestID).
					Str("message_ts", msgTS).
					Msg("posted permission prompt to slack, waiting for response (control)")
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
			if _, err := h.bot.PostMessage(channelID, finalMsg, threadTS); err != nil {
				logger.Error().Err(err).Msg("failed to post final task message")
			}

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
	if _, err := h.bot.PostMessage(channelID, finalMsg, threadTS); err != nil {
		logger.Error().Err(err).Msg("failed to post final task message")
	}

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
	// Redirect, when true, denies the pending permission and forwards the
	// user's typed text as a fresh stdin turn to the task. Used by the
	// ambiguous-response prompt to let the user cancel a stale permission
	// and redirect the agent with new instructions. The text itself is
	// looked up server-side from h.pendingAmbiguousTexts keyed by
	// ThreadKey (Slack action values are capped at 2000 chars).
	Redirect bool `json:"rd,omitempty"`
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

// tryPostAskUserQuestionPrompt renders the CLI-style Q&A picker in Slack when
// the tool is AskUserQuestion and the input parses into a valid question set.
// Returns the Slack message TS if a custom prompt was posted (and the caller
// should NOT fall back to the generic permission prompt). Returns "" when the
// tool/input doesn't qualify so the caller can use the generic path.
func (h *Handler) tryPostAskUserQuestionPrompt(
	req PermissionRequest,
	channelID, threadTS, progressKey string,
	logger zerolog.Logger,
) string {
	if req.ToolName != "AskUserQuestion" {
		return ""
	}
	questions := parseAskUserQuestionInput(req.ToolInput)
	if len(questions) == 0 {
		return ""
	}

	blocks := buildAskUserQuestionBlocks(questions, progressKey)
	msgTS, err := h.bot.PostMessageBlocks(channelID, blocks, threadTS)
	if err != nil {
		logger.Error().Err(err).Msg("failed to post AskUserQuestion prompt")
		return ""
	}

	// Seed Selections with recommended defaults so a single Submit click
	// submits the same answer the radio/checkbox initially shows.
	selections := make([][]string, len(questions))
	for i, q := range questions {
		for j, opt := range q.Options {
			if strings.Contains(strings.ToLower(opt.Label), "(recommended)") {
				selections[i] = append(selections[i], fmt.Sprintf("%d", j))
				if !q.MultiSelect {
					break
				}
			}
		}
	}

	h.askQuestionStates.Store(progressKey, &askUserQuestionState{
		MessageTS:  msgTS,
		ChannelID:  channelID,
		ThreadTS:   threadTS,
		Questions:  questions,
		Selections: selections,
	})

	logger.Info().
		Int("num_questions", len(questions)).
		Str("message_ts", msgTS).
		Msg("posted AskUserQuestion prompt")
	return msgTS
}

// postAmbiguousResponsePrompt posts a permission-style block message when the
// user types something during a pending permission that doesn't parse as
// yes/no. Offers three buttons: treat as allow, treat as deny, or cancel the
// pending permission and redirect the agent with the typed text as new input.
func (h *Handler) postAmbiguousResponsePrompt(
	channelID, threadTS, userID, userText, progressKey string,
	logger zerolog.Logger,
) {
	allowValue, _ := json.Marshal(PermissionActionValue{
		ThreadKey: progressKey,
		Behavior:  "allow",
	})
	denyValue, _ := json.Marshal(PermissionActionValue{
		ThreadKey: progressKey,
		Behavior:  "deny",
	})
	redirectValue, _ := json.Marshal(PermissionActionValue{
		ThreadKey: progressKey,
		Behavior:  "deny",
		Redirect:  true,
	})

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject(
				"mrkdwn",
				":grey_question: *How should I route this message?*\nA permission prompt is still pending above and your reply isn't a clear yes/no.",
				false, false,
			),
			nil, nil,
		),
		slack.NewSectionBlock(
			slack.NewTextBlockObject(
				"mrkdwn",
				fmt.Sprintf("*You said:*\n>%s", strings.ReplaceAll(userText, "\n", "\n>")),
				false, false,
			),
			nil, nil,
		),
		slack.NewActionBlock(
			"ambiguous_actions",
			func() *slack.ButtonBlockElement {
				b := slack.NewButtonBlockElement(
					"ambiguous_allow",
					string(allowValue),
					slack.NewTextBlockObject("plain_text", "Allow pending", false, false),
				)
				b.Style = "primary"
				return b
			}(),
			func() *slack.ButtonBlockElement {
				b := slack.NewButtonBlockElement(
					"ambiguous_deny",
					string(denyValue),
					slack.NewTextBlockObject("plain_text", "Deny pending", false, false),
				)
				b.Style = "danger"
				return b
			}(),
			slack.NewButtonBlockElement(
				"ambiguous_redirect",
				string(redirectValue),
				slack.NewTextBlockObject("plain_text", "Cancel & send as new input", false, false),
			),
		),
	}

	msgTS, err := h.bot.PostMessageBlocks(channelID, blocks, threadTS)
	if err != nil {
		logger.Error().Err(err).Msg("failed to post ambiguous response prompt")
		return
	}

	h.pendingAmbiguous.Store(progressKey, &pendingAmbiguous{
		Text:      userText,
		MessageTS: msgTS,
		ChannelID: channelID,
		ThreadTS:  threadTS,
		UserID:    userID,
	})
	logger.Info().
		Str("message_ts", msgTS).
		Msg("posted ambiguous response prompt")
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
	isAmbiguousAction := action.ActionID == "ambiguous_allow" ||
		action.ActionID == "ambiguous_deny" ||
		action.ActionID == "ambiguous_redirect"
	isAskQuestionSelect := action.ActionID == "askq_radio" ||
		action.ActionID == "askq_checkbox"
	isAskQuestionFinal := action.ActionID == "askq_submit" ||
		action.ActionID == "askq_cancel"
	if !isPermissionAction && !isAmbiguousAction && !isAskQuestionSelect && !isAskQuestionFinal {
		logger.Debug().Msg("ignoring non-permission action")
		return
	}

	if isAskQuestionSelect {
		h.handleAskQuestionSelect(callback, action, logger)
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
		Bool("redirect", actionValue.Redirect).
		Logger()

	if isAmbiguousAction {
		h.handleAmbiguousAction(callback, actionValue, logger)
		return
	}

	if isAskQuestionFinal {
		h.handleAskQuestionFinal(callback, action, actionValue, logger)
		return
	}

	// Look up the running task
	taskVal, ok := h.runningTasks.Load(actionValue.ThreadKey)
	if !ok {
		logger.Warn().Msg("no running task found for permission response")
		// Update the message to show it's stale
		if err := h.bot.UpdateMessage(callback.Channel.ID, callback.Message.Timestamp,
			":warning: This permission request is no longer active."); err != nil {
			logger.Error().Err(err).Msg("failed to update stale permission message")
		}
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

	// Send the response to Claude via FIFO or control message
	resp := PermissionResponse{Behavior: actionValue.Behavior}
	if actionValue.Behavior == "deny" {
		resp.Message = fmt.Sprintf("User %s denied permission", callback.User.Name)
	}

	logger.Info().
		Str("behavior", resp.Behavior).
		Bool("is_control", pending.IsControlPermission).
		Msg("sending permission response from button click")

	if pending.IsControlPermission && pending.ControlRequestID != "" {
		// Send control_response for control message permissions.
		if err := task.SendControlResponse(pending.ControlRequestID, resp.Behavior, resp.Message); err != nil {
			logger.Error().Err(err).Msg("failed to send control response")
		} else {
			logger.Info().Msg("permission response sent via control_response")
		}
	} else {
		// Send via MCP FIFO for traditional permissions.
		task.SendPermissionResponse(resp)
		logger.Info().Msg("permission response sent to FIFO")
	}

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
// handleAskQuestionSelect records a radio-button or checkbox change on an
// in-flight AskUserQuestion prompt. It does not resolve the permission —
// that happens on Submit. The block_id carries the question index as
// "askq_q<N>" so we can route the update to the right entry.
func (h *Handler) handleAskQuestionSelect(
	callback *slack.InteractionCallback,
	action *slack.BlockAction,
	logger zerolog.Logger,
) {
	// block_id format: "askq_q<idx>"
	blockID := action.BlockID
	if !strings.HasPrefix(blockID, "askq_q") {
		logger.Warn().Str("block_id", blockID).Msg("malformed askq block_id")
		return
	}
	qIdx, err := strconv.Atoi(strings.TrimPrefix(blockID, "askq_q"))
	if err != nil {
		logger.Warn().Err(err).Str("block_id", blockID).Msg("bad askq block_id index")
		return
	}

	// We don't know the threadKey from the action alone — scan askQuestionStates
	// for the state whose MessageTS matches the message this action fired on.
	// (Slack doesn't round-trip our server-side thread key on radio/checkbox
	// changes like it does with button values.) There's usually at most a
	// handful of pending states at once; a linear scan is fine.
	msgTS := callback.Container.MessageTs
	var state *askUserQuestionState
	h.askQuestionStates.Range(func(k, v any) bool {
		s := v.(*askUserQuestionState)
		if s.MessageTS == msgTS {
			state = s
			return false
		}
		return true
	})
	if state == nil {
		logger.Debug().Str("message_ts", msgTS).Msg("no askq state for selection; prompt may be stale")
		return
	}

	if qIdx < 0 || qIdx >= len(state.Selections) {
		logger.Warn().Int("q_idx", qIdx).Msg("askq index out of range")
		return
	}

	switch action.ActionID {
	case "askq_radio":
		if action.SelectedOption.Value != "" {
			state.Selections[qIdx] = []string{action.SelectedOption.Value}
		} else {
			state.Selections[qIdx] = nil
		}
	case "askq_checkbox":
		picks := make([]string, 0, len(action.SelectedOptions))
		for _, opt := range action.SelectedOptions {
			picks = append(picks, opt.Value)
		}
		state.Selections[qIdx] = picks
	}
	logger.Debug().
		Int("q_idx", qIdx).
		Strs("selections", state.Selections[qIdx]).
		Msg("recorded askq selection")
}

// handleAskQuestionFinal resolves an AskUserQuestion prompt on Submit or
// Cancel. Submit sends the underlying permission as allow with the user's
// formatted answer as the message so Claude sees what was chosen (approve-
// and-see path — if Claude's AskUserQuestion implementation doesn't honor
// this in headless mode we'll switch to the deny-with-answer strategy).
// Cancel denies the permission.
func (h *Handler) handleAskQuestionFinal(
	callback *slack.InteractionCallback,
	action *slack.BlockAction,
	actionValue PermissionActionValue,
	logger zerolog.Logger,
) {
	stateVal, ok := h.askQuestionStates.LoadAndDelete(actionValue.ThreadKey)
	if !ok {
		logger.Warn().Msg("no AskUserQuestion state found; prompt is stale")
		if err := h.bot.UpdateMessage(
			callback.Channel.ID, callback.Message.Timestamp,
			":warning: This question prompt is no longer active.",
		); err != nil {
			logger.Error().Err(err).Msg("failed to update stale askq message")
		}
		return
	}
	state := stateVal.(*askUserQuestionState)

	taskVal, ok := h.runningTasks.Load(actionValue.ThreadKey)
	if !ok {
		logger.Warn().Msg("no running task for askq final action")
		return
	}
	task := taskVal.(*RunningTask)

	pendingVal, hasPending := h.pendingPermissions.LoadAndDelete(actionValue.ThreadKey)
	var pending *PendingPermission
	if hasPending {
		pending = pendingVal.(*PendingPermission)
	}

	isCancel := action.ActionID == "askq_cancel"

	// Build the permission response.
	//
	// Approve-and-see doesn't work: Claude Code's internal AskUserQuestion
	// implementation runs the tool after permission is granted and — in
	// headless `--input-format stream-json` mode — has no interactive UI to
	// collect the answer, so it returns an empty result. The allow response's
	// message field is NOT substituted for the tool output. Observed in
	// practice: Claude replied "The AskUserQuestion tool is returning empty
	// answers both times."
	//
	// Instead we DENY the tool call and stuff the user's answers into the
	// deny message. Claude sees the deny as the tool result and reads the
	// answers out of the message body. This keeps the tool invocation from
	// racing with user input and is the documented pattern for surfacing
	// user-provided context when a tool can't run.
	resp := PermissionResponse{}
	var answerSummary string
	if isCancel {
		resp.Behavior = "deny"
		resp.Message = fmt.Sprintf("User %s cancelled the question prompt.", callback.User.Name)
	} else {
		resp.Behavior = "deny"
		answerSummary = formatAskUserQuestionAnswer(state)
		resp.Message = "AskUserQuestion is unavailable in this environment; the user answered directly:\n" + answerSummary
	}

	if hasPending {
		if pending.IsControlPermission && pending.ControlRequestID != "" {
			if err := task.SendControlResponse(pending.ControlRequestID, resp.Behavior, resp.Message); err != nil {
				logger.Error().Err(err).Msg("failed to send control response for askq")
			}
		} else {
			task.SendPermissionResponse(resp)
		}
	}

	// Update the prompt message with the outcome.
	var updated string
	if isCancel {
		updated = fmt.Sprintf(":x: *Question cancelled* by <@%s>", callback.User.ID)
	} else {
		updated = fmt.Sprintf(":white_check_mark: *Answer submitted* by <@%s>\n%s",
			callback.User.ID, answerSummary)
	}
	if err := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, updated); err != nil {
		logger.Error().Err(err).Msg("failed to update askq prompt after final click")
	}
}

// handleAmbiguousAction dispatches button clicks on the ambiguous-response
// prompt. allow/deny route the user's text as the corresponding permission
// response; redirect denies the pending permission and forwards the text as
// a fresh turn to the task.
func (h *Handler) handleAmbiguousAction(
	callback *slack.InteractionCallback,
	actionValue PermissionActionValue,
	logger zerolog.Logger,
) {
	ambigVal, ok := h.pendingAmbiguous.LoadAndDelete(actionValue.ThreadKey)
	if !ok {
		logger.Warn().Msg("no pending ambiguous prompt found; button is stale")
		if err := h.bot.UpdateMessage(
			callback.Channel.ID, callback.Message.Timestamp,
			":warning: This prompt is no longer active.",
		); err != nil {
			logger.Error().Err(err).Msg("failed to update stale ambiguous message")
		}
		return
	}
	ambig := ambigVal.(*pendingAmbiguous)

	taskVal, ok := h.runningTasks.Load(actionValue.ThreadKey)
	if !ok {
		logger.Warn().Msg("no running task for ambiguous action")
		if err := h.bot.UpdateMessage(
			callback.Channel.ID, callback.Message.Timestamp,
			":warning: Task is no longer running.",
		); err != nil {
			logger.Error().Err(err).Msg("failed to update stale ambiguous message")
		}
		return
	}
	task := taskVal.(*RunningTask)

	pendingVal, hasPending := h.pendingPermissions.Load(actionValue.ThreadKey)
	var pending *PendingPermission
	if hasPending {
		pending = pendingVal.(*PendingPermission)
	}

	// Build the permission response.
	resp := PermissionResponse{Behavior: actionValue.Behavior}
	switch {
	case actionValue.Redirect:
		resp.Message = fmt.Sprintf("User %s cancelled the pending permission to redirect with new instructions.", callback.User.Name)
	case actionValue.Behavior == "deny":
		resp.Message = fmt.Sprintf("User %s denied permission", callback.User.Name)
	}

	if hasPending {
		if pending.IsControlPermission && pending.ControlRequestID != "" {
			if err := task.SendControlResponse(pending.ControlRequestID, resp.Behavior, resp.Message); err != nil {
				logger.Error().Err(err).Msg("failed to send control response from ambiguous action")
			}
		} else {
			task.SendPermissionResponse(resp)
		}
		h.pendingPermissions.Delete(actionValue.ThreadKey)
		// Rewrite the ORIGINAL permission prompt to show the outcome — same
		// visual treatment as a direct button click on the prompt.
		h.updatePermissionMessage(pending, actionValue.Behavior, callback.User.ID, "")
	}

	// For redirect: forward the user's originally-typed text to Claude as a
	// new stdin turn so the agent picks up the new instructions.
	if actionValue.Redirect && ambig.Text != "" {
		// Clear output consolidation since the user is starting a new turn.
		h.lastOutputMsg.Delete(actionValue.ThreadKey)
		if err := task.SendInput(ambig.Text); err != nil {
			logger.Error().Err(err).Msg("failed to send redirected input")
		}
	}

	// Rewrite the ambiguous prompt itself with the outcome.
	var outcome string
	switch {
	case actionValue.Redirect:
		outcome = fmt.Sprintf(":twisted_rightwards_arrows: Cancelled pending and forwarded message by <@%s>", callback.User.ID)
	case actionValue.Behavior == "allow":
		outcome = fmt.Sprintf(":white_check_mark: Treated as *Allow* by <@%s>", callback.User.ID)
	default:
		outcome = fmt.Sprintf(":x: Treated as *Deny* by <@%s>", callback.User.ID)
	}
	quoted := strings.ReplaceAll(ambig.Text, "\n", "\n>")
	updated := fmt.Sprintf("%s\n>%s", outcome, quoted)
	if err := h.bot.UpdateMessage(ambig.ChannelID, ambig.MessageTS, updated); err != nil {
		logger.Error().Err(err).Msg("failed to update ambiguous prompt after click")
	}
}

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

// postToolSnippet posts a tool result summary, optionally uploading the full
// content as a collapsible snippet based on verbosity level and tool type.
func (h *Handler) postToolSnippet(channelID, threadTS, toolName, inputJSON, content string, logger zerolog.Logger) {
	contentLen := len(content)
	lineCount := strings.Count(content, "\n") + 1

	isVerboseTool := h.verboseTools[toolName]
	verbosityLevel := h.bot.sessions.GetVerbosityLevel(channelID, threadTS)

	var input map[string]any
	_ = json.Unmarshal([]byte(inputJSON), &input) // Best-effort parse, ignore errors

	getString := func(key string) string {
		if v, ok := input[key].(string); ok {
			return v
		}
		return ""
	}

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
			// For multi-line commands (e.g., heredocs), show only the first line.
			shortCmd := command
			if idx := strings.Index(shortCmd, "\n"); idx != -1 {
				shortCmd = shortCmd[:idx] + "..."
			}
			// Truncate long commands.
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
	case "Write":
		filePath := getString("file_path")
		if filePath != "" {
			// Shorten path for display.
			shortPath := filePath
			if len(shortPath) > 50 {
				shortPath = "..." + shortPath[len(shortPath)-47:]
			}
			summary = fmt.Sprintf(":pencil2: `%s` `%s` (%s)", toolName, shortPath, formatBytes(contentLen))
			snippetTitle = filepath.Base(filePath)
		} else {
			summary = fmt.Sprintf(":pencil2: `%s` (%s)", toolName, formatBytes(contentLen))
			snippetTitle = "Write output"
		}
	case "Edit":
		filePath := getString("file_path")
		if filePath != "" {
			// Shorten path for display.
			shortPath := filePath
			if len(shortPath) > 50 {
				shortPath = "..." + shortPath[len(shortPath)-47:]
			}
			summary = fmt.Sprintf(":pencil: `%s` `%s` (%s)", toolName, shortPath, formatBytes(contentLen))
			snippetTitle = filepath.Base(filePath)
		} else {
			summary = fmt.Sprintf(":pencil: `%s` (%s)", toolName, formatBytes(contentLen))
			snippetTitle = "Edit output"
		}
	case "TodoWrite":
		// Extract first todo item for context.
		var firstTask string
		if todos, ok := input["todos"].([]any); ok && len(todos) > 0 {
			if todo, ok := todos[0].(map[string]any); ok {
				if content, ok := todo["content"].(string); ok {
					firstTask = content
					if len(firstTask) > 40 {
						firstTask = firstTask[:37] + "..."
					}
				}
			}
			summary = fmt.Sprintf(":clipboard: `%s` `%s` (%d items)", toolName, firstTask, len(todos))
		} else {
			summary = fmt.Sprintf(":clipboard: `%s`", toolName)
		}
		snippetTitle = "TodoWrite output"
	case "EnterPlanMode":
		summary = fmt.Sprintf(":memo: `%s`", toolName)
		snippetTitle = "EnterPlanMode output"
	default:
		summary = fmt.Sprintf(":gear: `%s` (%s)", toolName, formatBytes(contentLen))
		snippetTitle = fmt.Sprintf("%s output", toolName)
	}

	// Verbose tools respect verbosity settings:
	//   -1 (silent): No output at all
	//    0 (summary): Summary only, no file upload
	//    1 (full): Upload as collapsible snippet
	// Non-verbose tools always upload snippets regardless of verbosity level.
	if isVerboseTool {
		switch verbosityLevel {
		case -1:
			logger.Debug().Str("tool", toolName).Msg("skipping tool output (silent mode)")
			return

		case 0:
			// If the previous message in this thread was also a verbose tool summary,
			// edit it in-place instead of posting a new one to reduce notification noise.
			threadKey := key(channelID, threadTS)

			if lastMsgTS, ok := h.lastVerboseToolMsg.Load(threadKey); ok {
				if err := h.bot.UpdateMessage(channelID, lastMsgTS.(string), summary); err != nil {
					logger.Error().Err(err).Str("tool", toolName).Msg("failed to update tool summary, posting new message")
					// Fallback to posting a new message.
					if msgTS, err := h.bot.PostMessage(channelID, summary, threadTS); err != nil {
						logger.Error().Err(err).Str("tool", toolName).Msg("failed to post tool summary")
					} else {
						h.lastVerboseToolMsg.Store(threadKey, msgTS)
					}
				}
			} else {
				if msgTS, err := h.bot.PostMessage(channelID, summary, threadTS); err != nil {
					logger.Error().Err(err).Str("tool", toolName).Msg("failed to post tool summary")
				} else {
					h.lastVerboseToolMsg.Store(threadKey, msgTS)
				}
			}
			return

		case 1:
			// Full verbose mode: fall through to upload snippet.
		}
	}

	// Upload content as collapsible snippet with summary as the comment.
	// This keeps the summary and expandable content together in one message.
	if _, err := h.bot.files.UploadSnippet(content, snippetTitle, summary, channelID, threadTS); err != nil {
		logger.Error().Err(err).Str("tool", toolName).Msg("failed to upload tool snippet")
	} else {
		// Clear last verbose tool message since we posted a snippet (non-verbose content).
		h.lastVerboseToolMsg.Delete(key(channelID, threadTS))
	}
}

// gatherThreadContext collects prior messages in a thread as context, returning
// an empty string if the thread was started by a bot mention or has no prior messages.
func (h *Handler) gatherThreadContext(channelID, threadTS, currentMessageTS string, logger zerolog.Logger) string {
	// If this is the thread root (threadTS == currentMessageTS), no prior context exists.
	if threadTS == currentMessageTS {
		return ""
	}

	params := &slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTS,
		Limit:     1000,
	}

	msgs, _, _, err := h.bot.client.GetConversationReplies(params)
	if err != nil {
		logger.Warn().Err(err).Msg("failed to get thread replies for context")
		return ""
	}

	if len(msgs) == 0 {
		return ""
	}

	// Check if the thread root (first message) is a bot mention.
	rootMsg := msgs[0]
	if mentionPattern.MatchString(rootMsg.Text) {
		logger.Debug().Msg("thread was started by bot mention, skipping context gather")
		return ""
	}

	var contextMessages []string
	for _, msg := range msgs {
		// Stop when we reach the current message (the @bot mention).
		if msg.Timestamp == currentMessageTS {
			break
		}

		var userName string
		if msg.User != "" {
			// Try to get user info for a friendly name.
			user, err := h.bot.client.GetUserInfo(msg.User)
			if err == nil && user != nil {
				userName = user.RealName
				if userName == "" {
					userName = user.Name
				}
			} else {
				userName = msg.User
			}
		} else if msg.BotID != "" {
			userName = "Bot"
		} else {
			userName = "Unknown"
		}

		contextMessages = append(contextMessages, fmt.Sprintf("%s: %s", userName, msg.Text))
	}

	if len(contextMessages) == 0 {
		return ""
	}

	context := "Previous conversation in this thread:\n\n"
	context += strings.Join(contextMessages, "\n") + "\n\n"
	context += "---\n\nThe user is now asking you to help with the following:\n\n"

	logger.Info().
		Int("message_count", len(contextMessages)).
		Msg("gathered thread context from existing conversation")

	return context
}
