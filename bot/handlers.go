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

	"github.com/calebcase/oops"
	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// startMsgTemplate is the ":rocket: Starting a task" banner shown at the
// top of every new task thread. Settings help is a fixed-width table so
// fields / values / notes line up. Emojis in the "notes" column may
// render slightly wider than a monospace glyph on some clients — that's
// fine because nothing to their right depends on alignment.
const startMsgTemplate = ":rocket: Starting a `%s` task...\n\n" +
	"_Settings are controlled with `@bot set FIELD=VALUE`:_\n" +
	"```\n" +
	"field      | values                             | notes\n" +
	"-----------+------------------------------------+--------------------------------\n" +
	"verbosity  | +/- or 0/1/-1 (or 💬 / 🙈)         | 🙈 silent · summary · 💬 full\n" +
	"model      | opus|sonnet|haiku (+/- cycles)     | 🎼 · 📜 · 🌸\n" +
	"plan       | on|off (or +/-)                    | 💭 on by default\n" +
	"filesync   | on|off                             | sync project dir (non-recursive)\n" +
	"```"

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

	// Track in-flight init prompts (setup dialogs for tasks whose
	// directory or .clod/ is missing). key (progressKey) -> *pendingInit
	pendingInits sync.Map

	// Track in-flight `@bot !:` confirmation dialogs. Keyed by
	// progressKey; value is *pendingDangerous. The pending state holds
	// the original instructions so the button handler can invoke the
	// task without re-parsing the event text.
	pendingDangerous sync.Map

	// Track in-flight Slack-reference confirmation dialogs (permalink
	// expansion). Keyed by progressKey; value is *pendingSlackRefState.
	// Only one dialog per thread at a time — additional refs queue
	// naturally because the caller doesn't advance until this resolves.
	pendingSlackRefs sync.Map

	// userNameCache memoizes Slack user_id → display name lookups for
	// the session. Slack's team/users.info is rate-limited and we call
	// it once per distinct author when formatting referenced threads.
	// sync.Map keyed by user id, value is string.
	userNameCache sync.Map

	// Track the per-thread rolling progress message used to surface docker
	// build + SSH agent lines while clod boots. key (progressKey) ->
	// *progressMsg.
	progressMessages sync.Map

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

// progressMsg tracks a rolling "container is being prepared" message so
// clod wrapper / docker build activity is visible during the minute+
// silence between task start and the first claude output.
type progressMsg struct {
	MessageTS string
	// Lines is the tail of recent progress lines. We keep only the last
	// few so the message doesn't grow unbounded on long builds.
	Lines []string
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

// pendingDangerous tracks a posted `@bot !:` confirmation dialog while
// we wait for the user to click Proceed or Cancel. Instructions are
// stored server-side so the button value stays small (Slack caps
// action values at 2000 chars); on Proceed the button handler runs
// the task using this state.
type pendingDangerous struct {
	Instructions string
	MessageTS    string
	ChannelID    string
	ThreadTS     string
	MentionTS    string // TS of the user's @-mention (anchor for reactions)
	RequesterID  string // user who typed `@bot !:` (only they can confirm)
}

// pendingSlackRefState tracks a posted Slack-permalink-expansion dialog
// while we wait for the user's inclusion choice. PromptBase is the
// user's input with ref URLs left as-is; once the choice is resolved
// the button handler rebuilds the final prompt (inline-always refs +
// whatever the user chose for the confirm-needed ones) and invokes
// OnFinalize. OnFinalize is the deferred launch/SendInput — wrapping
// the caller's forward path in a closure lets this flow work
// identically for new tasks, running-task mentions, thread replies,
// and session resumes.
type pendingSlackRefState struct {
	ChannelID   string
	ThreadTS    string
	TaskPath    string
	MessageTS   string // TS of the dialog message (updated on click)
	RequesterID string // only they can Proceed
	PromptBase  string // original user input (URLs left in place)
	InlineRefs  []*SlackRefResult
	ConfirmRefs []*SlackRefResult
	HasOverCap  bool
	OnFinalize  func(finalPrompt string)
	OnCancel    func() // invoked on the Cancel button so callers can clean up
}

const (
	verbosityEmoji = "speech_balloon" // 💬 level 1 (full)
	seeNoEvilEmoji = "see_no_evil"    // 🙈 level -1 (silent)

	// Model-indicator emojis. Bot adds its own reaction to the task's
	// status message to show which model is active; a user reacting with a
	// different model emoji switches the active model for the thread.
	opusEmoji   = "musical_score"  // 🎼 Opus
	sonnetEmoji = "scroll"         // 📜 Sonnet
	haikuEmoji  = "cherry_blossom" // 🌸 Haiku

	// planModeEmoji marks plan-mode on the anchor message. A thread starts
	// with plan mode ON by default (bot adds this reaction); removing the
	// reaction drops back to default permission mode; re-adding it turns
	// plan mode back on. Takes effect on the NEXT runClod invocation —
	// claude can't switch permission modes mid-session in stream-json
	// mode.
	planModeEmoji = "thought_balloon" // 💭 plan mode

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
	// Normalize model ids with context-window suffixes or full
	// claude-NAME-X-Y forms to their family. Covers what claude-
	// code writes into `.clod/claude/settings.json` when you run
	// `/model` in a session: `opus[1m]`, `sonnet[1m]`,
	// `claude-opus-4-7`, `claude-haiku-4-5`, etc.
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "opus"):
		return opusEmoji
	case strings.Contains(lower, "sonnet"):
		return sonnetEmoji
	case strings.Contains(lower, "haiku"):
		return haikuEmoji
	}
	return sonnetEmoji
}

// readTaskClaudeSettingsModel reads `.clod/claude/settings.json`
// from the task directory (the same file claude-code writes when a
// user runs `/model` inside the container) and returns the `model`
// field. Empty string when the file is missing, malformed, or the
// field isn't set. claude-code is the authoritative writer; the bot
// only reads to inherit the choice — e.g. for templated tasks where
// the template's model preference carries over.
func readTaskClaudeSettingsModel(taskPath string) string {
	settingsPath := filepath.Join(taskPath, ".clod", "claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return ""
	}
	var s struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s.Model)
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

	// Authorization — global allowlist plus the per-thread allowlist
	// maintained via `@bot allow/disallow @user`. The per-thread list
	// only carries weight inside the thread it was set in.
	if !h.authorizedInThread(ev.Channel, threadTS, ev.User) {
		logger.Warn().Msg("unauthorized user")
		if _, err := h.bot.PostMessage(ev.Channel, h.bot.auth.RejectMessage(), threadTS); err != nil {
			logger.Error().Err(err).Msg("failed to post authorization rejection message")
		}
		return
	}

	// `@bot !: <instructions>` — run claude DIRECTLY on the host
	// (outside the clod/docker sandbox) in the agents base directory.
	// Checked before `*:` so the two syntaxes don't fight; the `!:`
	// form posts a confirmation dialog first because the user is
	// opting out of container isolation.
	if instructions := ParseDangerousRootMention(ev.Text); instructions != "" {
		h.handleDangerousRootTask(ev, threadTS, instructions, logger)
		return
	}

	// `@bot *: <instructions>` — run clod directly in the agents base
	// directory (treat the base itself as the task, not a subdir of it).
	// Useful for cross-task work or setups where the base dir IS the
	// project you want the agent working in.
	if instructions := ParseRootMention(ev.Text); instructions != "" {
		h.handleRootTask(ctx, ev, threadTS, instructions, logger)
		return
	}

	// `@bot <template>:: <instructions>` — auto-name a new task, copy
	// the named sibling as the template, skip the init dialog
	// entirely. This is the fast path when the user already knows
	// which existing setup they want to clone from.
	if cmd := ParseNamedAutoMention(ev.Text); cmd != nil {
		h.handleNamedTemplateAutoTask(ctx, ev, threadTS, cmd.Template, cmd.Instructions, logger)
		return
	}

	// `@bot :: <instructions>` — auto-generate a memorable task name
	// (YYYY-MM-DD-adjective-noun) and route into the normal new-task
	// flow. The init prompt still surfaces so the user can tweak image
	// / ssh / model / packages; they can just click Create to take the
	// defaults.
	if instructions := ParseAutoNameMention(ev.Text); instructions != "" {
		base := h.bot.tasks.BasePath()
		if base == "" {
			if _, err := h.bot.PostMessage(ev.Channel,
				":warning: Couldn't generate a task name — `CLOD_BOT_AGENTS_PATH` isn't set.",
				threadTS); err != nil {
				logger.Debug().Err(err).Msg("failed to post auto-name error")
			}
			return
		}
		name, err := generateTaskName(base)
		if err != nil {
			logger.Error().Err(err).Msg("failed to generate task name")
			if _, perr := h.bot.PostMessage(ev.Channel,
				fmt.Sprintf(":warning: Couldn't generate a unique task name: %v", err),
				threadTS); perr != nil {
				logger.Debug().Err(perr).Msg("failed to post auto-name error")
			}
			return
		}
		if _, err := h.bot.PostMessage(ev.Channel,
			fmt.Sprintf(":label: Auto-generated task name: `%s`", name),
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post auto-name notice")
		}
		// Rewrite the mention text so ParseMention sees a normal
		// `<@BOT> name: instructions` shape for the downstream flow.
		// Replace only the first `::` occurrence (the one we matched).
		ev.Text = strings.Replace(ev.Text, "::", name+":", 1)
		h.handleNewTask(ctx, ev, threadTS, logger)
		return
	}

	// `@bot close` — stop the current task (if any) and mark the
	// session dormant so resume-on-restart skips it. The session
	// record stays so reactions/metadata persist; a later @-mention
	// in this thread re-opens via the normal continuation path.
	if ParseCloseCommand(ev.Text) {
		h.handleCloseCommand(ev, threadTS, logger)
		return
	}

	// `@bot allow @user` / `@bot disallow @user` — manage the
	// per-thread allowlist. Handled before `set` so the field-value
	// regex can't accidentally match it (it won't, but be explicit).
	if cmd := ParseAllowCommand(ev.Text); cmd != nil {
		h.handleAllowCommand(ev, threadTS, cmd, logger)
		return
	}

	// `@bot set FIELD=VALUE` — thread-level preference change. Handled
	// before running-task input forwarding so commands work regardless of
	// whether the task is still running.
	if cmd := ParseSetCommand(ev.Text); cmd != nil {
		h.handleSetCommand(ev, threadTS, cmd, logger)
		return
	}

	// Check if there's already a running task in this thread
	progressKey := key(ev.Channel, threadTS)
	if taskVal, ok := h.runningTasks.Load(progressKey); ok {
		// Task is running - send this as input to Claude.
		task := taskVal.(*RunningTask)
		input := ParseContinuation(ev.Text)
		// If the user attached files, download them into the task dir
		// and append the paths so the agent can act on them.
		input = h.augmentInputWithAttachments(
			ev.Channel, threadTS, ev.TimeStamp, task.taskPath,
			input, false, logger,
		)
		if input != "" {
			send := func(finalInput string) {
				logger.Debug().Str("input", finalInput).Msg("sending input to running task")
				if err := task.SendInput(finalInput); err != nil {
					logger.Error().Err(err).Msg("failed to send input to task")
				}
			}
			cancel := func() {
				if _, err := h.bot.PostMessage(ev.Channel,
					":x: Message not forwarded — referenced content couldn't be confirmed.",
					threadTS); err != nil {
					logger.Debug().Err(err).Msg("failed to post slackref-cancel notice")
				}
			}
			finalInput, proceed := h.resolveAndRouteRefs(
				ev.Channel, threadTS, task.taskPath, input, ev.User, send, cancel, logger,
			)
			if proceed {
				send(finalInput)
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

// HandleMessage processes regular message events. In channels we only
// act on thread replies (top-level posts need an explicit @-mention
// to route through HandleAppMention); in DMs — both 1:1 (`im`) and
// group (`mpim`) — we also act on top-level messages by treating the
// whole DM as an implicit @bot conversation.
//
// DM semantics:
//   - First top-level DM with no prior session for this channel →
//     starts a new auto-named task rooted at that message.
//   - Subsequent top-level DMs → treated as continuations of the
//     most-recently-updated session in this DM channel. This means
//     bot commands like `close`, `stop`, `set ...`, `allow @user`,
//     and free-form text all route to the active session, mirroring
//     the experience of @-mentioning the bot in a channel thread.
//   - Shortcut prefixes (`*:`, `!:`, `::`) are preserved so the user
//     can still start a new root / host-direct / auto-named task
//     mid-DM without re-prefixing.
func (h *Handler) HandleMessage(ctx context.Context, ev *slackevents.MessageEvent) {
	// Ignore bot messages (ours and other bots').
	if ev.BotID != "" {
		return
	}
	// Skip edits/deletes/file-shares/joins/leaves. The base shape we
	// want has no SubType. Extending to file_share is worth
	// considering later so a DM with just an attached file starts a
	// task.
	if ev.SubType != "" {
		return
	}

	threadTS := ev.ThreadTimeStamp
	isDM := ev.ChannelType == "im" || ev.ChannelType == "mpim"
	// Non-DM top-level messages go through HandleAppMention only. DM
	// top-level messages fall through to the new-task path below.
	if threadTS == "" && !isDM {
		return
	}

	logger := h.logger.With().
		Str("channel", ev.Channel).
		Str("thread_ts", threadTS).
		Str("user", ev.User).
		Str("text", ev.Text).
		Bool("dm", isDM).
		Logger()

	// Check if message is @mentioning someone.
	//
	// In channels, Slack emits a separate `app_mention` event for
	// bot mentions — we drop the duplicate `message.channels` /
	// `message.groups` copy here and let HandleAppMention process
	// it. In DMs, Slack does NOT emit `app_mention` (every DM
	// message is implicitly "to the bot"), so dropping the mention
	// here would silently ignore commands like `<@bot> close` in a
	// thread reply. Synthesize the AppMentionEvent ourselves and
	// route through HandleAppMention instead.
	if matches := otherMentionPattern.FindStringSubmatch(ev.Text); matches != nil {
		mentionedUser := matches[1]
		if isDM {
			logger.Debug().
				Str("mentioned_user", mentionedUser).
				Msg("DM message has leading mention; dispatching via HandleAppMention")
			synthetic := &slackevents.AppMentionEvent{
				Channel:         ev.Channel,
				User:            ev.User,
				TimeStamp:       ev.TimeStamp,
				ThreadTimeStamp: threadTS,
				Text:            ev.Text,
			}
			h.HandleAppMention(ctx, synthetic)
			return
		}
		logger.Debug().
			Str("mentioned_user", mentionedUser).
			Msg("message mentions a user, ignoring (app_mention will handle if it's the bot)")
		return
	}

	// DM top-level message: no thread → route as an implicit @bot
	// mention. If a prior session exists for this channel, target its
	// thread (so `close`/`set`/free-form all continue that session);
	// otherwise start fresh through the auto-name flow.
	// Channel top-levels were filtered out above; reaching here with
	// empty threadTS implies DM.
	if threadTS == "" {
		h.dispatchDMAsMention(ctx, ev, logger)
		return
	}

	// Check if there's a running task in this thread
	progressKey := key(ev.Channel, threadTS)
	taskVal, ok := h.runningTasks.Load(progressKey)

	if ok {
		// Task is running - send input or handle permission.
		// Authorization check: only bot-wide or per-thread allowed users
		// can drive the agent. Everyone else is silently ignored so we
		// don't disrupt unrelated conversation in the same channel.
		if !h.authorizedInThread(ev.Channel, threadTS, ev.User) {
			logger.Debug().Msg("unauthorized user posted in active thread, ignoring")
			return
		}
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
				// Post-resolution hooks (plan-mode exit, etc.).
				h.afterPermissionResolved(ev.Channel, threadTS, perm.ToolName, resp.Behavior, logger)
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

		// Send the message as input to Claude, augmented with file
		// attachment paths if any were uploaded alongside the message.
		input := h.augmentInputWithAttachments(
			ev.Channel, threadTS, ev.TimeStamp, task.taskPath,
			ev.Text, true, logger,
		)
		// Clear output message consolidation since user sent a message.
		h.lastOutputMsg.Delete(key(ev.Channel, threadTS))
		send := func(finalInput string) {
			logger.Debug().Str("input", finalInput).Msg("sending thread reply to running task")
			if err := task.SendInput(finalInput); err != nil {
				logger.Error().Err(err).Msg("failed to send input to task")
			}
		}
		cancel := func() {
			if _, err := h.bot.PostMessage(ev.Channel,
				":x: Message not forwarded — referenced content couldn't be confirmed.",
				threadTS); err != nil {
				logger.Debug().Err(err).Msg("failed to post slackref-cancel notice")
			}
		}
		finalInput, proceed := h.resolveAndRouteRefs(
			ev.Channel, threadTS, task.taskPath, input, ev.User, send, cancel, logger,
		)
		if proceed {
			send(finalInput)
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

	// Check authorization — accept bot-wide OR per-thread allowlist.
	if !h.authorizedInThread(ev.Channel, threadTS, ev.User) {
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

	// Download any attached files and append their saved paths to the
	// prompt so the resumed agent can pick them up on this turn.
	prompt := h.augmentInputWithAttachments(
		ev.Channel, threadTS, ev.TimeStamp, session.TaskPath,
		ev.Text, true, logger,
	)

	// Post status
	if _, err := h.bot.PostMessage(
		ev.Channel,
		fmt.Sprintf(":arrows_counterclockwise: Resuming task `%s`...", session.TaskName),
		threadTS,
	); err != nil {
		logger.Error().Err(err).Msg("failed to post resume task message")
	}

	launch := func(finalPrompt string) {
		h.runClod(
			ctx,
			ev.Channel,
			ev.User,
			session.TaskPath,
			session.TaskName,
			finalPrompt,
			session.SessionID,
			threadTS,
			logger,
		)
	}
	cancelResume := func() {
		if _, err := h.bot.PostMessage(ev.Channel,
			":x: Resume cancelled — referenced content couldn't be confirmed.",
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post slackref-cancel notice")
		}
	}
	finalPrompt, proceed := h.resolveAndRouteRefs(
		ev.Channel, threadTS, session.TaskPath, prompt, ev.User, launch, cancelResume, logger,
	)
	if proceed {
		launch(finalPrompt)
	}
}

// HandleReactionAdded is intentionally a no-op. Thread-level state
// (verbosity, model, plan mode) is changed via `@bot set FIELD=VALUE`
// messages, not reactions. The bot still *posts* its own reactions on
// the anchor message as a read-only visual indicator of current state,
// but user-added reactions are ignored so accidental clicks don't flip
// settings and so the bot-vs-user reaction asymmetry (the bot can't
// remove a user's reaction) stops causing stuck indicators.
func (h *Handler) HandleReactionAdded(ctx context.Context, ev *slackevents.ReactionAddedEvent) {
	// Intentionally blank — see comment above.
}

// HandleAppHomeOpened republishes the Home tab view for the user who
// just opened it. The view always includes a personal section
// (sessions where SessionMapping.UserID == ev.User) and — when the
// user is on the bot-wide allowlist — a workspace section
// aggregating across all sessions. We always republish on every
// open rather than caching because sessions.json changes frequently
// (stats accumulate on each turn) and the tab should reflect the
// latest numbers the moment the user looks at it.
func (h *Handler) HandleAppHomeOpened(ctx context.Context, ev *slackevents.AppHomeOpenedEvent) {
	// Slack fires this event for every tab open, including "messages".
	// We only care about the actual Home tab.
	if ev.Tab != "home" {
		return
	}

	logger := h.logger.With().
		Str("user", ev.User).
		Str("tab", ev.Tab).
		Logger()

	var hash string
	if ev.View != nil {
		hash = ev.View.Hash
	}
	h.publishHomeView(ev.User, hash, logger)
}

// publishHomeView renders and publishes the Home tab for a user.
// Shared by the app_home_opened event handler and the in-view
// Refresh button so both entry points build the view identically.
//
// knownHash is the hash of the view the caller currently believes is
// published (from the AppHomeOpenedEvent or the click callback).
// Slack uses it to guard against races between concurrent publishes.
// We always retry on hash_conflict by republishing WITHOUT a hash —
// Slack's docs treat an absent hash as "I don't care about prior
// state, just publish this", which is the right behavior for our
// idempotent re-renders. (slack-go's PublishView would have sent
// `"hash": ""` literally and Slack treats that as a stale-state
// assertion, hence the direct PublishViewContext call with the
// pointer left nil on the retry.)
func (h *Handler) publishHomeView(userID string, knownHash string, logger zerolog.Logger) {
	sessions := h.bot.sessions.AllSessions()
	includeWorkspace := h.bot.auth.IsAuthorized(userID)
	var rollup map[string][]UsageTotals
	if includeWorkspace {
		rollup = h.bot.sessions.UsageRollup(usageRollupWindows)
	}
	view := buildHomeTabView(sessions, rollup, h.bot.PermalinkFor, h.bot.LatestPermalinkFor, userID, includeWorkspace, Version)

	req := slack.PublishViewContextRequest{
		UserID: userID,
		View:   view,
	}
	if knownHash != "" {
		hashCopy := knownHash
		req.Hash = &hashCopy
	}
	_, err := h.bot.client.PublishViewContext(context.Background(), req)
	if err != nil && strings.Contains(err.Error(), "hash_conflict") && req.Hash != nil {
		logger.Debug().Str("hash", knownHash).Msg("home view hash conflict; retrying without hash")
		req.Hash = nil
		_, err = h.bot.client.PublishViewContext(context.Background(), req)
	}
	if err != nil {
		logger.Error().Err(err).Msg("failed to publish home tab view")
		return
	}
	logger.Debug().
		Int("session_count", len(sessions)).
		Bool("workspace", includeWorkspace).
		Msg("published home tab view")
}

// handleHomeRefresh handles clicks on the in-view Refresh button.
// Re-renders the view for the clicking user using current
// sessions.json state, so active sessions' counts and timestamps
// advance without the user having to leave and re-enter the tab.
func (h *Handler) handleHomeRefresh(ctx context.Context, callback *slack.InteractionCallback, logger zerolog.Logger) {
	logger = logger.With().
		Str("user", callback.User.ID).
		Str("action", "home_refresh").
		Logger()
	hash := callback.View.Hash
	h.publishHomeView(callback.User.ID, hash, logger)
}

// augmentInputWithAttachments downloads any Slack files attached to a
// message and appends their local paths to `input` in the same
// "Attached files have been saved to:" shape that the initial prompt
// uses. Posts status/error messages in the thread as side-effects.
// When isThreadReply is true we use GetThreadReplyFiles (which resolves
// via conversations.replies); false uses GetMessageFiles (resolves via
// conversations.history) — app_mention events don't carry the files
// array directly so we always have to refetch.
func (h *Handler) augmentInputWithAttachments(
	channelID, threadTS, messageTS, taskPath, input string,
	isThreadReply bool,
	logger zerolog.Logger,
) string {
	var slackFiles []slack.File
	var err error
	if isThreadReply {
		slackFiles, err = h.bot.files.GetThreadReplyFiles(channelID, threadTS, messageTS)
	} else {
		slackFiles, err = h.bot.files.GetMessageFiles(channelID, messageTS)
	}
	if err != nil {
		logger.Warn().Err(err).Msg("failed to check for message files")
		return input
	}
	if len(slackFiles) == 0 {
		return input
	}
	if _, err := h.bot.PostMessage(
		channelID,
		fmt.Sprintf(":inbox_tray: Downloading %d file(s)...", len(slackFiles)),
		threadTS,
	); err != nil {
		logger.Error().Err(err).Msg("failed to post file download message")
	}
	var downloaded []string
	for _, file := range slackFiles {
		localPath, err := h.bot.files.DownloadToTask(file, taskPath)
		if err != nil {
			logger.Error().Err(err).Str("file_id", file.ID).Msg("failed to download file")
			if _, postErr := h.bot.PostMessage(
				channelID,
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
		downloaded = append(downloaded, localPath)
	}
	if len(downloaded) == 0 {
		return input
	}
	if input != "" {
		input += "\n\n"
	}
	input += "Attached files have been saved to:\n"
	for _, p := range downloaded {
		input += fmt.Sprintf("- %s\n", p)
	}
	return input
}

// userCacheMap pulls the cached {user_id → display name} map out of the
// concurrent sync.Map. Safe to call from any goroutine; returns a fresh
// local map each time.
func (h *Handler) userCacheMap() map[string]string {
	out := make(map[string]string)
	h.userNameCache.Range(func(k, v any) bool {
		ks, _ := k.(string)
		vs, _ := v.(string)
		out[ks] = vs
		return true
	})
	return out
}

// mergeUserCache writes any newly-resolved names back into the shared
// cache so subsequent formats hit it.
func (h *Handler) mergeUserCache(cache map[string]string) {
	for k, v := range cache {
		h.userNameCache.Store(k, v)
	}
}

// resolveAndRouteRefs finds any Slack permalinks in `input`, resolves
// each one, splits them into "auto-include inline" (public + under cap)
// and "needs confirmation" (private or over cap), and either (a)
// returns the finalized prompt synchronously when no confirmation is
// needed, or (b) posts a dialog and returns empty + proceed=false. On
// (b) the caller must NOT proceed; `onFinalize` will be invoked from
// the button handler once the user chooses. `onCancel` fires if the
// user clicks Cancel.
//
// taskPath is used to materialize conversation assets under the task
// directory. requesterID is the user who originated the mention — only
// they can click the confirmation buttons.
//
// Error cases are surfaced as thread posts ("bot isn't in #foo — ...")
// and the offending ref is dropped from the output. The caller still
// proceeds with whatever refs resolved successfully.
func (h *Handler) resolveAndRouteRefs(
	channelID, threadTS, taskPath, input, requesterID string,
	onFinalize func(finalPrompt string),
	onCancel func(),
	logger zerolog.Logger,
) (finalized string, proceed bool) {
	refs := FindSlackRefs(input)
	if len(refs) == 0 {
		return input, true
	}

	cache := h.userCacheMap()
	var inline []*SlackRefResult
	var confirm []*SlackRefResult
	hasOverCap := false
	for _, ref := range refs {
		res := resolveSlackRef(h.bot.client, ref, logger)
		if res.Joined {
			// Auto-joined to read this ref. Post a thread notice
			// so human members of the channel don't wonder why the
			// bot suddenly appeared in their member list.
			if _, err := h.bot.PostMessage(channelID,
				fmt.Sprintf(":inbox_tray: Auto-joined <#%s|%s> to read the referenced thread.", res.Ref.ChannelID, res.ChannelName),
				threadTS); err != nil {
				logger.Debug().Err(err).Msg("failed to post auto-join notice")
			}
		}
		if res.Err != nil {
			h.postRefError(channelID, threadTS, res, logger)
			continue
		}
		if res.NeedsConfirm() {
			if res.OverCap() {
				hasOverCap = true
			}
			confirm = append(confirm, res)
		} else {
			inline = append(inline, res)
		}
	}
	h.mergeUserCache(cache)

	// Fast path: nothing needs confirmation — splice inline refs and
	// return immediately.
	if len(confirm) == 0 {
		return buildPromptWithRefs(input, inline, nil, h.userCacheMap(), h.bot.client, logger), true
	}

	// Post dialog and park the state. OnFinalize fires from the button
	// handler once the user resolves.
	progressKey := key(channelID, threadTS)
	if _, already := h.pendingSlackRefs.Load(progressKey); already {
		// Shouldn't happen in practice — the caller only advances after
		// resolution — but stay safe rather than stacking dialogs.
		if _, err := h.bot.PostMessage(channelID,
			":warning: A reference confirmation is already pending on this thread.",
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post duplicate-slackref warning")
		}
		return "", false
	}

	messageTS, err := h.postSlackRefDialog(channelID, threadTS, progressKey, confirm, hasOverCap, logger)
	if err != nil {
		// Posting the dialog failed — best effort: fall through with just
		// the inline refs and skip the confirm-needed ones.
		logger.Error().Err(err).Msg("failed to post slackref confirmation dialog")
		return buildPromptWithRefs(input, inline, nil, h.userCacheMap(), h.bot.client, logger), true
	}
	h.pendingSlackRefs.Store(progressKey, &pendingSlackRefState{
		ChannelID:   channelID,
		ThreadTS:    threadTS,
		TaskPath:    taskPath,
		MessageTS:   messageTS,
		RequesterID: requesterID,
		PromptBase:  input,
		InlineRefs:  inline,
		ConfirmRefs: confirm,
		HasOverCap:  hasOverCap,
		OnFinalize:  onFinalize,
		OnCancel:    onCancel,
	})
	return "", false
}

// postRefError writes a short thread note explaining why a referenced
// message couldn't be read, so the user knows to invite the bot / fix
// the permalink rather than wondering why the agent didn't use it.
func (h *Handler) postRefError(channelID, threadTS string, res *SlackRefResult, logger zerolog.Logger) {
	body := fmt.Sprintf(
		":warning: Couldn't read the referenced Slack thread (<%s>): %s",
		res.Ref.Permalink,
		res.ErrReason,
	)
	if _, err := h.bot.PostMessage(channelID, body, threadTS); err != nil {
		logger.Debug().Err(err).Msg("failed to post slackref error notice")
	}
}

// postSlackRefDialog builds and posts the confirmation block message
// for a set of confirm-needed refs. Returns the message TS so the
// caller can update it when the user clicks a button.
func (h *Handler) postSlackRefDialog(
	channelID, threadTS, progressKey string,
	confirm []*SlackRefResult,
	hasOverCap bool,
	logger zerolog.Logger,
) (string, error) {
	actionValue := fmt.Sprintf(`{"k":%q}`, progressKey)

	var lines []string
	for _, r := range confirm {
		badges := []string{}
		if r.IsPrivate {
			if r.IsDM {
				badges = append(badges, ":lock: DM")
			} else {
				badges = append(badges, ":lock: private")
			}
		}
		if r.OverCap() {
			badges = append(badges, fmt.Sprintf(":bookmark_tabs: %d msgs / %d chars (over cap)", r.MsgCount, r.CharCount))
		} else {
			badges = append(badges, fmt.Sprintf("%d msgs / %d chars", r.MsgCount, r.CharCount))
		}
		tag := "#" + r.ChannelName
		if r.IsDM {
			tag = r.ChannelName
		}
		lines = append(lines, fmt.Sprintf("• %s — %s — <%s|permalink>", strings.Join(badges, " · "), tag, r.Ref.Permalink))
	}

	header := slack.NewSectionBlock(
		slack.NewTextBlockObject(
			"mrkdwn",
			":eyes: *Referenced Slack content needs confirmation*",
			false, false,
		),
		nil, nil,
	)
	var noteText string
	switch {
	case hasOverCap:
		noteText = "One or more referenced threads are too large to inline into the prompt. Save them as conversation assets (a directory under the task dir) so the agent can read the content on disk, or skip them."
	default:
		noteText = "One or more referenced threads come from a private conversation. Confirm you want their contents included in the agent's context."
	}
	body := slack.NewSectionBlock(
		slack.NewTextBlockObject(
			"mrkdwn",
			noteText+"\n\n"+strings.Join(lines, "\n"),
			false, false,
		),
		nil, nil,
	)

	var buttons []slack.BlockElement
	if !hasOverCap {
		inlineBtn := slack.NewButtonBlockElement(
			"slackref_inline",
			actionValue,
			slack.NewTextBlockObject("plain_text", "Include inline", false, false),
		)
		inlineBtn.Style = "primary"
		buttons = append(buttons, inlineBtn)
	}
	assetBtn := slack.NewButtonBlockElement(
		"slackref_asset",
		actionValue,
		slack.NewTextBlockObject("plain_text", "Save as asset", false, false),
	)
	if hasOverCap {
		assetBtn.Style = "primary"
	}
	buttons = append(buttons, assetBtn)

	skipBtn := slack.NewButtonBlockElement(
		"slackref_skip",
		actionValue,
		slack.NewTextBlockObject("plain_text", "Skip references", false, false),
	)
	cancelBtn := slack.NewButtonBlockElement(
		"slackref_cancel",
		actionValue,
		slack.NewTextBlockObject("plain_text", "Cancel", false, false),
	)
	cancelBtn.Style = "danger"
	buttons = append(buttons, skipBtn, cancelBtn)

	actions := slack.NewActionBlock("slackref_actions", buttons...)

	ts, err := h.bot.PostMessageBlocks(channelID, []slack.Block{header, body, actions}, threadTS)
	if err != nil {
		return "", oops.Trace(err)
	}
	return ts, nil
}

// buildPromptWithRefs splices resolved refs into the user's input. For
// refs with AssetPath set, the splice is a short pointer at the
// on-disk path rather than the inline content.
func buildPromptWithRefs(
	input string,
	inline []*SlackRefResult,
	assetNotes []string,
	userCache map[string]string,
	client *slack.Client,
	logger zerolog.Logger,
) string {
	if len(inline) == 0 && len(assetNotes) == 0 {
		return input
	}
	var b strings.Builder
	for _, r := range inline {
		b.WriteString(FormatRefInline(r, userCache, client, logger))
		b.WriteString("\n")
	}
	for _, note := range assetNotes {
		b.WriteString(note)
		b.WriteString("\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(input)
	return b.String()
}

// authorizedInThread reports whether userID can drive the bot in this
// thread: either they're on the bot-wide allowlist OR the thread's
// per-thread allowlist (managed via `@bot allow/disallow @user`).
func (h *Handler) authorizedInThread(channelID, threadTS, userID string) bool {
	if h.bot.auth.IsAuthorized(userID) {
		return true
	}
	return h.bot.sessions.IsExtraAllowedUser(channelID, threadTS, userID)
}

// handleAllowCommand adds or removes a Slack user from the thread's
// per-thread allowlist. The thread owner (and anyone already authorized
// in this thread) can manage the list.
func (h *Handler) handleAllowCommand(
	ev *slackevents.AppMentionEvent,
	threadTS string,
	cmd *AllowCommand,
	logger zerolog.Logger,
) {
	logger = logger.With().
		Str("allow_action", cmd.Action).
		Str("target_user", cmd.UserID).
		Logger()

	session := h.bot.sessions.Get(ev.Channel, threadTS)
	if session == nil || session.TaskName == "" {
		if _, err := h.bot.PostMessage(ev.Channel,
			":warning: `allow`/`disallow` only work inside a thread the bot has started.",
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post allow-command scope warning")
		}
		return
	}

	switch cmd.Action {
	case "allow":
		added, count := h.bot.sessions.AddExtraAllowedUser(ev.Channel, threadTS, cmd.UserID)
		if err := h.bot.sessions.Save(); err != nil {
			logger.Debug().Err(err).Msg("save after AddExtraAllowedUser")
		}
		var msg string
		if added {
			msg = fmt.Sprintf(":white_check_mark: <@%s> is now allowed to interact with the bot in this thread (%d extra user%s).",
				cmd.UserID, count, pluralSuffix(count))
		} else {
			msg = fmt.Sprintf(":information_source: <@%s> already has permission in this thread (%d extra user%s).",
				cmd.UserID, count, pluralSuffix(count))
		}
		if _, err := h.bot.PostMessage(ev.Channel, msg, threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post allow confirmation")
		}
	case "disallow":
		removed, count := h.bot.sessions.RemoveExtraAllowedUser(ev.Channel, threadTS, cmd.UserID)
		if err := h.bot.sessions.Save(); err != nil {
			logger.Debug().Err(err).Msg("save after RemoveExtraAllowedUser")
		}
		var msg string
		if removed {
			msg = fmt.Sprintf(":no_entry: <@%s> no longer has per-thread permission (%d extra user%s remain).",
				cmd.UserID, count, pluralSuffix(count))
		} else {
			msg = fmt.Sprintf(":information_source: <@%s> was not on this thread's allowlist.", cmd.UserID)
		}
		if _, err := h.bot.PostMessage(ev.Channel, msg, threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post disallow confirmation")
		}
	}
}

func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// afterPermissionResolved runs any extra cleanup that should happen
// once the user has answered a permission prompt. Today the only hook
// is exiting plan mode when the agent's ExitPlanMode tool was
// approved — that's the moment plan mode has served its purpose and
// the thread should revert to the bot's default permission mode.
// Centralised so every permission-resolution path (buttons, text
// reply, ambiguous-response prompt) stays in sync.
func (h *Handler) afterPermissionResolved(channelID, threadTS, toolName, behavior string, logger zerolog.Logger) {
	if toolName == "ExitPlanMode" && behavior == "allow" {
		h.exitPlanModeIndicator(channelID, threadTS, logger)
	}
}

// exitPlanModeIndicator clears session.PermissionMode and removes the
// bot's plan-mode reaction from the anchor. Posts a confirmation so
// the user knows plan mode is no longer active. Idempotent: safe to
// call when plan mode wasn't on.
func (h *Handler) exitPlanModeIndicator(channelID, threadTS string, logger zerolog.Logger) {
	session := h.bot.sessions.Get(channelID, threadTS)
	if session == nil || session.PermissionMode != "plan" {
		return
	}
	h.bot.sessions.SetPermissionMode(channelID, threadTS, "")
	if err := h.bot.sessions.Save(); err != nil {
		logger.Debug().Err(err).Msg("save after plan-mode exit")
	}
	if session.ReactionAnchorTS != "" {
		if err := h.bot.RemoveReaction(channelID, session.ReactionAnchorTS, planModeEmoji); err != nil {
			logger.Debug().Err(err).Msg("failed to remove plan-mode indicator after exit")
		}
	}
	if _, err := h.bot.PostMessage(channelID,
		":thought_balloon: Exited plan mode — the agent will now make changes directly. `@bot set plan=on` to re-enable for the next turn.",
		threadTS); err != nil {
		logger.Debug().Err(err).Msg("failed to post plan-exit confirmation")
	}
}

// handleCloseCommand stops the currently running task (if any) and
// clears the thread's Active flag so resume-on-restart skips it. The
// session record stays intact — session.TaskPath, model, permission
// mode, extra-allowed users etc. all survive, so a later @-mention in
// the thread re-opens via the normal continuation path. Use this when
// you're done with a thread but might want to come back to it later;
// delete the thread or just stop mentioning the bot if you want it
// completely forgotten.
func (h *Handler) handleCloseCommand(
	ev *slackevents.AppMentionEvent,
	threadTS string,
	logger zerolog.Logger,
) {
	session := h.bot.sessions.Get(ev.Channel, threadTS)
	if session == nil || session.TaskName == "" {
		if _, err := h.bot.PostMessage(ev.Channel,
			":warning: No active bot session in this thread.",
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post close no-session warning")
		}
		return
	}

	// Clear Active FIRST so neither finalizeTask (fires after Cancel)
	// nor a crashing restart picks this thread up as "should resume".
	// finalizeTask's non-clean-exit path leaves Active alone, so our
	// explicit false survives.
	h.bot.sessions.SetActive(ev.Channel, threadTS, false)
	if err := h.bot.sessions.Save(); err != nil {
		logger.Debug().Err(err).Msg("save after close-clear-active")
	}

	progressKey := key(ev.Channel, threadTS)
	var wasRunning bool
	if taskVal, ok := h.runningTasks.Load(progressKey); ok {
		task := taskVal.(*RunningTask)
		task.Cancel()
		wasRunning = true
		logger.Info().Msg("cancelling running task due to close command")
	}

	var msg string
	if wasRunning {
		msg = ":wave: *Session closed.* The running task has been stopped. Auto-resume is disabled for this thread — @-mention me here with new instructions to pick it back up."
	} else {
		msg = ":wave: *Session closed.* Auto-resume is disabled for this thread — @-mention me here with new instructions to pick it back up."
	}
	if _, err := h.bot.PostMessage(ev.Channel, msg, threadTS); err != nil {
		logger.Debug().Err(err).Msg("failed to post close confirmation")
	}
}

// handleSetCommand processes `@bot set FIELD=VALUE` messages. Supported
// fields: verbosity (−1/0/1 or emoji), model (opus/sonnet/haiku or emoji),
// plan (on/off/+/-/emoji). Values of "+" and "-" mean "step up" and
// "step down" respectively — sensible for ordinals (verbosity) and
// cyclic (model) values, binary-toggle for plan. The bot-owned anchor
// reactions are updated to reflect the new state so they remain a
// read-only visual indicator.
func (h *Handler) handleSetCommand(
	ev *slackevents.AppMentionEvent,
	threadTS string,
	cmd *SetCommand,
	logger zerolog.Logger,
) {
	logger = logger.With().
		Str("set_field", cmd.Field).
		Str("set_value", cmd.Value).
		Logger()

	session := h.bot.sessions.Get(ev.Channel, threadTS)
	if session == nil || session.TaskName == "" {
		// Only accept `set` commands inside bot-initiated threads so
		// random users in random channels can't flip state on sessions
		// they don't own.
		if _, err := h.bot.PostMessage(ev.Channel,
			":warning: `set` commands only work inside a thread the bot has started.",
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post set-command scope warning")
		}
		return
	}

	// Older sessions (created before ReactionAnchorTS tracking, or via
	// continuation paths that didn't set it) have no anchor recorded.
	// Fall back to the thread root — that's always the message that
	// kicked off the thread — and persist so subsequent set commands
	// hit the fast path.
	if session.ReactionAnchorTS == "" {
		session.ReactionAnchorTS = threadTS
		h.bot.sessions.Set(session)
		if err := h.bot.sessions.Save(); err != nil {
			logger.Debug().Err(err).Msg("failed to persist backfilled reaction anchor")
		}
		logger.Info().Str("anchor_ts", threadTS).Msg("backfilled reaction anchor from thread root")
	}

	switch cmd.Field {
	case "verbosity", "v":
		h.applyVerbositySet(ev.Channel, threadTS, session, cmd.Value, logger)
	case "model", "m":
		h.applyModelSet(ev.Channel, threadTS, session, cmd.Value, logger)
	case "plan", "plan_mode", "p":
		h.applyPlanSet(ev.Channel, threadTS, session, cmd.Value, logger)
	case "filesync", "sync", "files":
		h.applyFileSyncSet(ev.Channel, threadTS, session, cmd.Value, logger)
	default:
		if _, err := h.bot.PostMessage(ev.Channel,
			fmt.Sprintf(":warning: Unknown setting `%s`. Valid: `verbosity`, `model`, `plan`, `filesync`.", cmd.Field),
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post unknown-field warning")
		}
	}
}

// normalizeEmojiToken strips surrounding colons so ":musical_score:" and
// "musical_score" both map to the same emoji name.
func normalizeEmojiToken(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, ":")
	v = strings.TrimSuffix(v, ":")
	return strings.ToLower(v)
}

// applyVerbositySet interprets the value against the verbosity ladder
// (-1 silent, 0 summary, 1 full) and updates session + anchor reaction.
func (h *Handler) applyVerbositySet(channelID, threadTS string, session *SessionMapping, value string, logger zerolog.Logger) {
	current := h.bot.sessions.GetVerbosityLevel(channelID, threadTS)
	var newLevel int
	switch normalizeEmojiToken(value) {
	case "+":
		newLevel = current + 1
	case "-":
		newLevel = current - 1
	case "0", "summary", "default":
		newLevel = 0
	case "1", "full", "verbose", verbosityEmoji:
		newLevel = 1
	case "-1", "silent", seeNoEvilEmoji:
		newLevel = -1
	default:
		if _, err := h.bot.PostMessage(channelID,
			fmt.Sprintf(":warning: Unknown verbosity value `%s`. Use `+`, `-`, `-1`/`0`/`1`, or `:speech_balloon:`/`:see_no_evil:`.", value),
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post verbosity-value warning")
		}
		return
	}
	if newLevel > 1 {
		newLevel = 1
	}
	if newLevel < -1 {
		newLevel = -1
	}

	h.bot.sessions.SetVerbosityLevel(channelID, threadTS, newLevel)
	if err := h.bot.sessions.Save(); err != nil {
		logger.Debug().Err(err).Msg("save after verbosity set")
	}
	// Sync bot's anchor reactions: remove the other verbosity emojis,
	// add the one that matches the new level. Level 0 means "default" —
	// no dedicated reaction.
	if session.ReactionAnchorTS != "" {
		for _, e := range []string{verbosityEmoji, seeNoEvilEmoji} {
			if err := h.bot.RemoveReaction(channelID, session.ReactionAnchorTS, e); err != nil {
				logger.Debug().Err(err).Str("emoji", e).Msg("failed to remove old verbosity emoji")
			}
		}
		switch newLevel {
		case 1:
			_ = h.bot.AddReaction(channelID, session.ReactionAnchorTS, verbosityEmoji)
		case -1:
			_ = h.bot.AddReaction(channelID, session.ReactionAnchorTS, seeNoEvilEmoji)
		}
	}

	var msg string
	switch newLevel {
	case 1:
		msg = ":speech_balloon: Verbose mode enabled — tool outputs include full content."
	case 0:
		msg = ":bookmark: Summary mode — tool outputs are summarised (default)."
	case -1:
		msg = ":see_no_evil: Silent mode — verbose tool outputs are hidden."
	}
	if _, err := h.bot.PostMessage(channelID, msg, threadTS); err != nil {
		logger.Debug().Err(err).Msg("failed to post verbosity confirmation")
	}
}

// applyModelSet accepts model names, model emoji tokens, or `+`/`-`
// (cycle forward/back through the known model list).
func (h *Handler) applyModelSet(channelID, threadTS string, session *SessionMapping, value string, logger zerolog.Logger) {
	// Canonical cycle order: sonnet (default) → opus → haiku → sonnet …
	cycle := []string{"sonnet", "opus", "claude-haiku-4-5"}

	current := session.Model
	if current == "" {
		current = fallbackModel
	}

	var newModel string
	switch v := normalizeEmojiToken(value); v {
	case "+":
		newModel = cycleModel(cycle, current, 1)
	case "-":
		newModel = cycleModel(cycle, current, -1)
	case "opus", opusEmoji:
		newModel = "opus"
	case "sonnet", sonnetEmoji:
		newModel = "sonnet"
	case "haiku", "claude-haiku-4-5", haikuEmoji:
		newModel = "claude-haiku-4-5"
	default:
		// Accept any other string verbatim — the user may pass a full
		// model id we don't hard-code. claude will reject it at the
		// --model flag if it's genuinely bogus.
		newModel = value
	}

	if newModel == current {
		if _, err := h.bot.PostMessage(channelID,
			fmt.Sprintf(":information_source: Model already `%s`.", newModel), threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post already-set notice")
		}
		return
	}

	h.bot.sessions.SetModel(channelID, threadTS, newModel)
	if err := h.bot.sessions.Save(); err != nil {
		logger.Debug().Err(err).Msg("save after model set")
	}

	// Refresh the anchor reaction — remove any known model emoji, add
	// the new one. Idempotent RemoveReaction on emojis we didn't add.
	newEmoji := emojiForModel(newModel)
	if session.ReactionAnchorTS != "" {
		for _, e := range []string{opusEmoji, sonnetEmoji, haikuEmoji} {
			if e == newEmoji {
				continue
			}
			if err := h.bot.RemoveReaction(channelID, session.ReactionAnchorTS, e); err != nil {
				logger.Debug().Err(err).Str("emoji", e).Msg("failed to remove old model emoji")
			}
		}
		if err := h.bot.AddReaction(channelID, session.ReactionAnchorTS, newEmoji); err != nil {
			logger.Debug().Err(err).Msg("failed to add new model emoji")
		}
		h.bot.sessions.SetModelReactionEmoji(channelID, threadTS, newEmoji)
		if err := h.bot.sessions.Save(); err != nil {
			logger.Debug().Err(err).Msg("save after model emoji update")
		}
	}

	if _, err := h.bot.PostMessage(channelID,
		fmt.Sprintf(":%s: Model set to `%s` — takes effect on the next message you send in this thread.", newEmoji, newModel),
		threadTS); err != nil {
		logger.Debug().Err(err).Msg("failed to post model-set confirmation")
	}
}

func cycleModel(cycle []string, current string, step int) string {
	if len(cycle) == 0 {
		return current
	}
	idx := 0
	for i, m := range cycle {
		if m == current {
			idx = i
			break
		}
	}
	next := (idx + step) % len(cycle)
	if next < 0 {
		next += len(cycle)
	}
	return cycle[next]
}

// applyFileSyncSet toggles the thread's file-sync preference. File sync
// is the watcher that uploads files from the task's top-level directory
// to Slack as they get created/modified; default is ON. Takes effect
// within ~2s (the watcher's poll interval).
func (h *Handler) applyFileSyncSet(channelID, threadTS string, session *SessionMapping, value string, logger zerolog.Logger) {
	var enable bool
	switch normalizeEmojiToken(value) {
	case "on", "+", "true", "yes", "enabled":
		enable = true
	case "off", "-", "false", "no", "disabled":
		enable = false
	default:
		if _, err := h.bot.PostMessage(channelID,
			fmt.Sprintf(":warning: Unknown filesync value `%s`. Use `on`/`off` or `+`/`-`.", value),
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post filesync-value warning")
		}
		return
	}
	disabled := !enable
	if session.FileSyncDisabled == disabled {
		if _, err := h.bot.PostMessage(channelID,
			fmt.Sprintf(":information_source: File sync is already %s.",
				map[bool]string{true: "on", false: "off"}[enable]),
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post filesync already-set notice")
		}
		return
	}
	h.bot.sessions.SetFileSyncDisabled(channelID, threadTS, disabled)
	if err := h.bot.sessions.Save(); err != nil {
		logger.Debug().Err(err).Msg("save after filesync set")
	}

	var msg string
	if enable {
		msg = ":outbox_tray: *File sync ON* — files created or modified in the project directory will be uploaded to this thread."
	} else {
		msg = ":mute: *File sync OFF* — new or modified files in the project directory will NOT be uploaded. The watcher still tracks their state so re-enabling won't flood the thread."
	}
	if _, err := h.bot.PostMessage(channelID, msg, threadTS); err != nil {
		logger.Debug().Err(err).Msg("failed to post filesync confirmation")
	}
}

// applyPlanSet accepts on/off/+/-/emoji and toggles the thread's plan mode.
func (h *Handler) applyPlanSet(channelID, threadTS string, session *SessionMapping, value string, logger zerolog.Logger) {
	var newEnabled bool
	switch normalizeEmojiToken(value) {
	case "on", "+", "plan", "true", "yes", planModeEmoji:
		newEnabled = true
	case "off", "-", "default", "none", "false", "no":
		newEnabled = false
	default:
		if _, err := h.bot.PostMessage(channelID,
			fmt.Sprintf(":warning: Unknown plan value `%s`. Use `on`/`off`, `+`/`-`, or `:thought_balloon:`.", value),
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post plan-value warning")
		}
		return
	}
	newMode := ""
	if newEnabled {
		newMode = "plan"
	}
	if session.PermissionMode == newMode {
		if _, err := h.bot.PostMessage(channelID,
			fmt.Sprintf(":information_source: Plan mode is already `%s`.",
				map[bool]string{true: "on", false: "off"}[newEnabled]),
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post already-set notice")
		}
		return
	}

	h.bot.sessions.SetPermissionMode(channelID, threadTS, newMode)
	if err := h.bot.sessions.Save(); err != nil {
		logger.Debug().Err(err).Msg("save after plan set")
	}

	if session.ReactionAnchorTS != "" {
		if newEnabled {
			_ = h.bot.AddReaction(channelID, session.ReactionAnchorTS, planModeEmoji)
		} else {
			_ = h.bot.RemoveReaction(channelID, session.ReactionAnchorTS, planModeEmoji)
		}
	}

	var msg string
	if newEnabled {
		msg = ":thought_balloon: *Plan mode ON* — the agent will propose changes for approval before editing. Takes effect on the next message you send in this thread."
	} else {
		msg = ":thought_balloon: *Plan mode OFF* — the agent can edit directly. Takes effect on the next message you send in this thread."
	}
	if _, err := h.bot.PostMessage(channelID, msg, threadTS); err != nil {
		logger.Debug().Err(err).Msg("failed to post plan-mode confirmation")
	}
}

// handleModelReaction is retained for history but no longer called —
// model changes go through `applyModelSet` via the `set` command.
// Kept compiled so a future user-reaction path can re-enable it
// trivially; remove after the new command UX settles if the dead code
// bothers anyone.
//
//lint:ignore U1000 retained for potential re-enable
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
	// (the user's @-mention that kicked off the task). We don't trust
	// session.ModelReactionEmoji alone — sessions from older bot builds
	// don't carry that field, so the stored hint can be empty even when
	// the bot previously added an indicator. Instead, unconditionally
	// try to remove every OTHER model emoji; each RemoveReaction is
	// idempotent (Slack's "no_reaction" error is swallowed inside
	// bot.RemoveReaction), so asking to remove emojis we never added
	// is harmless and it guarantees a stale indicator doesn't survive.
	newEmoji := emojiForModel(newModel)
	if session.ReactionAnchorTS != "" {
		for _, e := range []string{opusEmoji, sonnetEmoji, haikuEmoji} {
			if e == newEmoji {
				continue
			}
			if err := h.bot.RemoveReaction(session.ChannelID, session.ReactionAnchorTS, e); err != nil {
				logger.Debug().Err(err).Str("emoji", e).Msg("failed to remove stale model reaction")
			}
		}
		if err := h.bot.AddReaction(session.ChannelID, session.ReactionAnchorTS, newEmoji); err != nil {
			logger.Debug().Err(err).Msg("failed to add new model reaction")
		}
		h.bot.sessions.SetModelReactionEmoji(session.ChannelID, session.ThreadTS, newEmoji)
		if err := h.bot.sessions.Save(); err != nil {
			logger.Debug().Err(err).Msg("failed to persist model reaction emoji")
		}
	}

	logger.Info().Str("old_model", oldModel).Msg("switched thread model")

	// Post a confirmation so the user sees the switch landed. Next-turn
	// semantics are important context; also remind them that only their
	// own reaction can be cleared by them (the bot can't remove a user's
	// reaction, so their old model emoji will linger until they click
	// to remove it).
	msg := fmt.Sprintf(":%s: Model set to `%s` — takes effect on the next message you send in this thread. _Your previous reaction stays until you unclick it._",
		ev.Reaction, newModel)
	if _, err := h.bot.PostMessage(session.ChannelID, msg, threadTS); err != nil {
		logger.Debug().Err(err).Msg("failed to post model switch confirmation")
	}
}

// handlePlanModeReaction is retained for history; plan-mode toggles are
// now driven by `applyPlanSet` via the `set` command. See the analogous
// note on handleModelReaction.
//
//lint:ignore U1000 retained for potential re-enable
func (h *Handler) handlePlanModeReaction(ctx context.Context, ev *slackevents.ReactionAddedEvent, enable bool) {
	logger := h.logger.With().
		Str("channel", ev.Item.Channel).
		Str("item_ts", ev.Item.Timestamp).
		Str("user", ev.User).
		Str("reaction", ev.Reaction).
		Bool("enable", enable).
		Logger()

	threadTS, err := h.getThreadTS(ev.Item.Channel, ev.Item.Timestamp)
	if err != nil {
		logger.Debug().Err(err).Msg("failed to get thread TS for plan-mode reaction")
		return
	}
	logger = logger.With().Str("thread_ts", threadTS).Logger()

	session := h.bot.sessions.Get(ev.Item.Channel, threadTS)
	if session == nil || session.TaskName == "" {
		logger.Debug().Msg("ignoring plan-mode reaction outside a bot-initiated thread")
		return
	}

	newMode := ""
	if enable {
		newMode = "plan"
	}
	if session.PermissionMode == newMode {
		logger.Debug().Msg("plan-mode reaction matches current state; nothing to do")
		return
	}

	h.bot.sessions.SetPermissionMode(ev.Item.Channel, threadTS, newMode)
	if err := h.bot.sessions.Save(); err != nil {
		logger.Error().Err(err).Msg("failed to save session after plan-mode change")
	}

	// Keep the bot's own indicator reaction in sync — add it when turning
	// on, remove it when turning off. User's reactions are per-user and
	// can't be removed by the bot, so we only manage our own.
	if session.ReactionAnchorTS != "" {
		if enable {
			if err := h.bot.AddReaction(ev.Item.Channel, session.ReactionAnchorTS, planModeEmoji); err != nil {
				logger.Debug().Err(err).Msg("failed to add plan-mode reaction")
			}
		} else {
			if err := h.bot.RemoveReaction(ev.Item.Channel, session.ReactionAnchorTS, planModeEmoji); err != nil {
				logger.Debug().Err(err).Msg("failed to remove plan-mode reaction")
			}
		}
	}

	logger.Info().Msg("toggled thread plan mode")

	// Post confirmation so the user sees the switch landed.
	var msg string
	if enable {
		msg = ":thought_balloon: *Plan mode ON* — the agent will propose changes for approval before editing. Takes effect on the next message you send in this thread."
	} else {
		msg = ":thought_balloon: *Plan mode OFF* — the agent can edit directly. Takes effect on the next message you send in this thread."
	}
	if _, err := h.bot.PostMessage(ev.Item.Channel, msg, threadTS); err != nil {
		logger.Debug().Err(err).Msg("failed to post plan-mode confirmation")
	}
}

// HandleReactionRemoved is a no-op for the same reason HandleReactionAdded
// is: thread state is controlled exclusively via `@bot set FIELD=VALUE`
// messages. User reactions don't mutate state.
func (h *Handler) HandleReactionRemoved(ctx context.Context, ev *slackevents.ReactionRemovedEvent) {
	// Intentionally blank.
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

// getThreadVerbosityFromReactions is retained for history. Verbosity is
// now set via `@bot set verbosity=...` so there's no reaction walk
// needed; `applyVerbositySet` writes the level directly.
//
//lint:ignore U1000 retained for potential re-enable
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

	// Look up the task. If discovery didn't find it, check whether the dir
	// simply doesn't exist yet or exists but lacks `.clod/`; either case
	// gets an interactive setup prompt rather than a "unknown task" error.
	taskPath, err := h.bot.tasks.Get(parsed.TaskName)
	if err != nil {
		if h.maybePromptInit(ev, threadTS, parsed, logger) {
			return
		}
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

	h.runNewTask(ctx, ev, threadTS, parsed.TaskName, taskPath, parsed.Instructions, logger)
}

// handleDMNewTask is the top-level-DM equivalent of HandleAppMention's
// `@bot :: <text>` (auto-named new task) path. No explicit mention is
// required because the user is already in a 1:1 / group DM with the
// bot — the whole conversation is implicitly directed at it. Thread
// replies underneath the resulting message continue the session
// through HandleMessage's existing flow.
// dispatchDMAsMention is the top-level-DM dispatcher. It looks up the
// most-recently-updated session for this DM channel (if any), rewrites
// the event into a synthetic @-mention targeting that session's
// thread, and hands off to HandleAppMention. Net effect: every DM
// command that works via @-mention in a channel (`close`, `set ...`,
// `allow @user`, free-form continuation text, shortcut prefixes) also
// works in a DM with no explicit @-mention.
//
// When no prior DM session exists for the channel, the event is
// synthesized with `:: <text>` (auto-name) unless the user typed an
// explicit shortcut — matching the first-message experience.
func (h *Handler) dispatchDMAsMention(
	ctx context.Context,
	ev *slackevents.MessageEvent,
	logger zerolog.Logger,
) {
	// Authorization: only bot-wide allowlist applies at this point —
	// there's no per-thread allowlist yet (no session exists).
	if !h.bot.auth.IsAuthorized(ev.User) {
		logger.Warn().Msg("unauthorized user DMed the bot")
		if _, err := h.bot.PostMessage(ev.Channel, h.bot.auth.RejectMessage(), ev.TimeStamp); err != nil {
			logger.Debug().Err(err).Msg("failed to post DM auth rejection")
		}
		return
	}

	text := strings.TrimSpace(ev.Text)
	if text == "" {
		return
	}

	// Top-level DMs only act on recognized commands now. A bare
	// "do thing X" message used to be auto-wrapped as `:: do thing X`
	// (new task) or routed as a continuation of the most-recent DM
	// session, which surprised users — they'd type a quick aside
	// and accidentally restart or extend a session. Instead, show
	// the usage info and let the user pick their next move with an
	// explicit prefix.
	if !hasDMCommandShape(text) {
		h.postDMUsageHint(ev, text, logger)
		return
	}

	// Recognized command: synthesize an AppMentionEvent and dispatch
	// through the normal mention router. Start shortcuts (`*:`,
	// `!:`, `::`, `<name>::`) always root a fresh thread at the
	// user's message; named-task `<name>:` commands also start
	// fresh since they target a specific task identity rather than
	// continuing a prior session.
	synthetic := &slackevents.AppMentionEvent{
		Channel:         ev.Channel,
		User:            ev.User,
		TimeStamp:       ev.TimeStamp,
		ThreadTimeStamp: ev.TimeStamp,
		Text:            "<@BOT> " + text,
	}
	logger = logger.With().Bool("dm_implicit_mention", true).Logger()
	h.HandleAppMention(ctx, synthetic)
}

// hasDMCommandShape reports whether a DM text is recognizable as a
// bot command worth dispatching. Recognized shapes:
//   - start shortcuts (`*:`, `!:`, `::`) — covered by hasStartShortcut
//   - `<template>:: <text>` (named-template auto-name)
//   - `<task>: <text>` (existing task or new-task init)
//
// Anything else (greetings, free-form questions, accidental DMs)
// triggers the usage hint instead of auto-creating / auto-resuming.
func hasDMCommandShape(text string) bool {
	if hasStartShortcut(text) {
		return true
	}
	// Both ParseNamedAutoMention and ParseMention require a
	// `<@BOT>` prefix in their regexes, so synthesize one for the
	// shape check. The actual dispatch synthesizes the same prefix
	// downstream.
	probe := "<@BOT> " + text
	if ParseNamedAutoMention(probe) != nil {
		return true
	}
	if ParseMention(probe) != nil {
		return true
	}
	return false
}

// postDMUsageHint posts the workspace help blocks (the same content
// rendered at the bottom of the Home tab) into the DM as a thread
// reply rooted at the user's message. Lets users discover the
// command vocabulary without interrupting any active session.
func (h *Handler) postDMUsageHint(ev *slackevents.MessageEvent, userText string, logger zerolog.Logger) {
	preview := userText
	const maxPreview = 200
	if len(preview) > maxPreview {
		preview = preview[:maxPreview] + "…"
	}
	intro := slack.NewSectionBlock(
		slack.NewTextBlockObject(
			"mrkdwn",
			fmt.Sprintf(":raised_hand: I didn't recognize `%s` as a bot command. Here's what I understand — pick a prefix and try again:",
				strings.ReplaceAll(preview, "`", "ʼ")),
			false, false,
		),
		nil, nil,
	)
	blocks := append([]slack.Block{intro}, buildHomeHelpBlocks()...)
	if _, err := h.bot.PostMessageBlocks(ev.Channel, blocks, ev.TimeStamp); err != nil {
		logger.Debug().Err(err).Msg("failed to post DM usage hint")
	}
}

// dmStartShortcuts are the mention prefixes that, when typed as the
// first bytes of a top-level DM, should be routed through the normal
// dispatcher rather than wrapped in an auto-name synthesis. Order
// doesn't matter; longest match wins naturally.
var dmStartShortcuts = []string{"*:", "!:", "::"}

// hasStartShortcut reports whether text begins with one of the
// recognized DM-shortcut prefixes. Used to decide whether to auto-
// prepend `:: ` (for auto-name) when a DM has no explicit shortcut.
func hasStartShortcut(text string) bool {
	for _, p := range dmStartShortcuts {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

// handleRootTask runs a task directly in the agents base directory
// rather than a subdirectory. The base dir itself is the task — `.clod/`
// lives at basePath, `taskPath = basePath`. If `.clod/` isn't set up
// yet, we fall through to the standard init prompt (with createDir=false
// since the base dir definitely exists).
func (h *Handler) handleRootTask(
	ctx context.Context,
	ev *slackevents.AppMentionEvent,
	threadTS string,
	instructions string,
	logger zerolog.Logger,
) {
	base := h.bot.tasks.BasePath()
	if base == "" {
		if _, err := h.bot.PostMessage(ev.Channel,
			":warning: Can't run a root task — `CLOD_BOT_AGENTS_PATH` isn't set.",
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post root-task error")
		}
		return
	}
	taskName := filepath.Base(base)
	logger = logger.With().
		Str("task", taskName).
		Str("task_path", base).
		Str("instructions", instructions).
		Bool("root_task", true).
		Logger()

	// If `.clod/system/run` is present, just run — skip the init prompt.
	if _, err := os.Stat(filepath.Join(base, ".clod", "system", "run")); err == nil {
		h.runNewTask(ctx, ev, threadTS, taskName, base, instructions, logger)
		return
	}

	// Otherwise show the init prompt with createDir=false (base dir
	// always exists, we just need to seed `.clod/`).
	if h.postInitPrompt(ev, threadTS, taskName, base, false, instructions, logger) {
		return
	}
	// Fall-through (shouldn't normally happen — postInitPrompt returns
	// false only on Slack post failure or a stacked prompt).
	if _, err := h.bot.PostMessage(ev.Channel,
		":warning: Couldn't start a root task.",
		threadTS); err != nil {
		logger.Debug().Err(err).Msg("failed to post root-task fallthrough error")
	}
}

// handleDangerousRootTask posts a confirmation dialog for `@bot !:` and
// stashes the pending state. Actual task launch happens in
// handleDangerousFinal after the user clicks Proceed. Cancel simply
// updates the dialog and drops the state.
//
// The `!:` form differs from `*:` only in that claude runs on the HOST
// rather than through clod (which would run it inside a docker
// container). Everything else — permissions, reactions, settings,
// file sync, MCP bridge — stays identical. The warning exists because
// losing container isolation means the agent can touch anything the
// bot process can touch.
// handleNamedTemplateAutoTask handles `@bot <template>:: <instructions>`:
// auto-name a new task, copy the named sibling's contents as a template,
// refresh the registry, and kick off the work. No init dialog — the
// user has already expressed intent by naming the template.
//
// Template validation: must exist under BasePath, must not be the
// generated auto-name, must not itself be an auto-named one-off. On
// any validation failure we post a friendly thread warning and bail
// without mutating state.
func (h *Handler) handleNamedTemplateAutoTask(
	ctx context.Context,
	ev *slackevents.AppMentionEvent,
	threadTS string,
	template string,
	instructions string,
	logger zerolog.Logger,
) {
	base := h.bot.tasks.BasePath()
	if base == "" {
		if _, err := h.bot.PostMessage(ev.Channel,
			":warning: Can't create a templated task — `CLOD_BOT_AGENTS_PATH` isn't set.",
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post named-auto base error")
		}
		return
	}

	// Template must exist on disk as a directory. We intentionally
	// don't require a `.clod/` — any sibling directory with content
	// can serve as a starting point (matches the init-prompt picker's
	// behavior).
	tplPath := filepath.Join(base, template)
	info, statErr := os.Stat(tplPath)
	switch {
	case os.IsNotExist(statErr):
		if _, err := h.bot.PostMessage(ev.Channel,
			fmt.Sprintf(":warning: Template `%s` doesn't exist. Available templates: %s",
				template, h.bot.tasks.ListFormatted()),
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post template-missing warning")
		}
		return
	case statErr != nil:
		logger.Warn().Err(statErr).Str("path", tplPath).Msg("template stat failed")
		if _, err := h.bot.PostMessage(ev.Channel,
			fmt.Sprintf(":warning: Couldn't read template `%s`: %v", template, statErr),
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post template-stat warning")
		}
		return
	case !info.IsDir():
		if _, err := h.bot.PostMessage(ev.Channel,
			fmt.Sprintf(":warning: `%s` exists but isn't a directory; can't use as template.", template),
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post template-not-dir warning")
		}
		return
	}
	// Refuse auto-named one-offs as templates; they're ephemeral
	// experiments per the template picker's filter and almost never
	// what the user means.
	if autoGeneratedTaskName.MatchString(template) {
		if _, err := h.bot.PostMessage(ev.Channel,
			fmt.Sprintf(":warning: `%s` is an auto-named one-off, not a reusable template. Pick a durable task name instead.", template),
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post template-auto warning")
		}
		return
	}

	name, err := generateTaskName(base)
	if err != nil {
		logger.Error().Err(err).Msg("failed to generate task name for named-template task")
		if _, perr := h.bot.PostMessage(ev.Channel,
			fmt.Sprintf(":warning: Couldn't generate a unique task name: %v", err),
			threadTS); perr != nil {
			logger.Debug().Err(perr).Msg("failed to post auto-name error")
		}
		return
	}
	newPath := filepath.Join(base, name)
	if err := materializeFromTemplate(tplPath, newPath, name); err != nil {
		logger.Error().Err(err).
			Str("template", template).
			Str("task_path", newPath).
			Msg("failed to materialize templated task")
		if _, perr := h.bot.PostMessage(ev.Channel,
			fmt.Sprintf(":warning: Failed to copy template `%s`: %v", template, err),
			threadTS); perr != nil {
			logger.Debug().Err(perr).Msg("failed to post template-copy error")
		}
		return
	}

	if _, err := h.bot.PostMessage(ev.Channel,
		fmt.Sprintf(":label: Auto-generated task name: `%s` (template: `%s`)", name, template),
		threadTS); err != nil {
		logger.Debug().Err(err).Msg("failed to post named-auto notice")
	}

	// Refresh the registry so the new task is discoverable to
	// later mentions / continuations. Not fatal if discovery has a
	// hiccup — the task's `.clod/` is on disk, so subsequent
	// mentions will still find it.
	if err := h.bot.tasks.Refresh(); err != nil {
		logger.Debug().Err(err).Msg("task registry refresh after named-template task")
	}

	logger = logger.With().
		Str("task", name).
		Str("template", template).
		Str("task_path", newPath).
		Logger()
	h.runNewTask(ctx, ev, threadTS, name, newPath, instructions, logger)
}

func (h *Handler) handleDangerousRootTask(
	ev *slackevents.AppMentionEvent,
	threadTS string,
	instructions string,
	logger zerolog.Logger,
) {
	base := h.bot.tasks.BasePath()
	if base == "" {
		if _, err := h.bot.PostMessage(ev.Channel,
			":warning: Can't run a host-direct task — `CLOD_BOT_AGENTS_PATH` isn't set.",
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post dangerous-root error")
		}
		return
	}

	progressKey := key(ev.Channel, threadTS)
	if _, already := h.pendingDangerous.Load(progressKey); already {
		if _, err := h.bot.PostMessage(ev.Channel,
			":warning: A confirmation dialog for this thread is already open — click Proceed or Cancel on it first.",
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post duplicate-dangerous warning")
		}
		return
	}

	allowValue := fmt.Sprintf(`{"k":%q,"b":"allow"}`, progressKey)
	denyValue := fmt.Sprintf(`{"k":%q,"b":"deny"}`, progressKey)

	header := slack.NewSectionBlock(
		slack.NewTextBlockObject(
			"mrkdwn",
			":rotating_light: *Run claude outside the container?*",
			false, false,
		),
		nil, nil,
	)
	// Truncate the echoed instructions so a runaway paste can't push the
	// section text past Slack's 3000-char block cap.
	preview := instructions
	const maxPreview = 1500
	if len(preview) > maxPreview {
		preview = preview[:maxPreview] + "…"
	}
	body := slack.NewSectionBlock(
		slack.NewTextBlockObject(
			"mrkdwn",
			fmt.Sprintf(
				"The `!:` form runs `claude` directly on the host in `%s`, "+
					"*without* the clod docker sandbox. The agent will have "+
					"the same filesystem / network / credential access the "+
					"bot process itself has.\n\nInstructions:\n>%s",
				base, strings.ReplaceAll(preview, "\n", "\n>"),
			),
			false, false,
		),
		nil, nil,
	)
	proceedBtn := slack.NewButtonBlockElement(
		"dangerous_proceed",
		allowValue,
		slack.NewTextBlockObject("plain_text", "Proceed (no container)", false, false),
	)
	proceedBtn.Style = "danger"
	cancelBtn := slack.NewButtonBlockElement(
		"dangerous_cancel",
		denyValue,
		slack.NewTextBlockObject("plain_text", "Cancel", false, false),
	)
	actions := slack.NewActionBlock("dangerous_actions", proceedBtn, cancelBtn)

	msgTS, err := h.bot.PostMessageBlocks(ev.Channel, []slack.Block{header, body, actions}, threadTS)
	if err != nil {
		logger.Error().Err(err).Msg("failed to post dangerous confirmation dialog")
		return
	}

	h.pendingDangerous.Store(progressKey, &pendingDangerous{
		Instructions: instructions,
		MessageTS:    msgTS,
		ChannelID:    ev.Channel,
		ThreadTS:     threadTS,
		MentionTS:    ev.TimeStamp,
		RequesterID:  ev.User,
	})
	logger.Info().Str("instructions", instructions).Msg("posted !: confirmation dialog")
}

// runNewTask performs the actual task start once taskName + taskPath are
// resolved: it downloads any attached files into the task dir, gathers
// prior thread context, posts the starting-a-task status, anchors the
// model / plan-mode reactions on the user's mention, and calls runClod.
// Shared by handleNewTask (subdir task via the registry) and
// handleRootTask (the agents dir itself as the task).
func (h *Handler) runNewTask(
	ctx context.Context,
	ev *slackevents.AppMentionEvent,
	threadTS string,
	taskName string,
	taskPath string,
	instructions string,
	logger zerolog.Logger,
) {
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
	prompt += instructions
	if len(downloadedFiles) > 0 {
		prompt += "\n\nAttached files have been saved to:\n"
		for _, path := range downloadedFiles {
			prompt += fmt.Sprintf("- %s\n", path)
		}
	}

	// Post initial status with verbosity + model info
	startMsg := fmt.Sprintf(startMsgTemplate, taskName)
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
	//
	// Resolution order for the initial model:
	//   1. Existing session preference (already set via @bot set model=X
	//      or a prior ingestion from claude's settings).
	//   2. Task-level claude settings (`.clod/claude/settings.json`'s
	//      `model` field) — claude-code writes this when the user runs
	//      `/model` inside claude. For templated tasks this carries the
	//      template's preference over to the new session automatically.
	//   3. Bot-wide default from the CLI flag.
	//   4. `fallbackModel` ("sonnet") if nothing else set.
	initialModel := h.bot.sessions.GetModel(ev.Channel, threadTS)
	if initialModel == "" {
		initialModel = readTaskClaudeSettingsModel(taskPath)
	}
	if initialModel == "" {
		initialModel = h.defaultModel
	}
	if initialModel == "" {
		initialModel = fallbackModel
	}
	session := h.bot.sessions.Get(ev.Channel, threadTS)
	newSession := session == nil
	if newSession {
		session = &SessionMapping{
			ChannelID: ev.Channel,
			ThreadTS:  threadTS,
			TaskName:  taskName,
			TaskPath:  taskPath,
			UserID:    ev.User,
			CreatedAt: time.Now(),
		}
	}
	session.ReactionAnchorTS = ev.TimeStamp
	session.Model = initialModel
	session.ModelReactionEmoji = emojiForModel(initialModel)
	// Root tasks (`*:` / `!:` — taskPath is the agents base dir)
	// touch every subdirectory; their defaults differ from subdir
	// tasks in two ways:
	//   1. filesync defaults OFF — the agent's churn across
	//      unrelated dirs would flood Slack with snippet uploads.
	//   2. plan mode defaults OFF — root-task use cases (status
	//      sweeps, multi-task orchestration) are usually
	//      exploratory / read-heavy and the ExitPlanMode round-
	//      trip is friction. Subdir tasks still default to plan on.
	// Both only apply to freshly-created sessions; existing sessions
	// keep whatever the user explicitly set. Toggle back via
	// `@bot set filesync=on` / `@bot set plan=on`.
	isRootTask := taskPath == h.bot.tasks.BasePath()
	if newSession && isRootTask {
		session.FileSyncDisabled = true
	}
	if session.PermissionMode == "" && !isRootTask {
		session.PermissionMode = "plan"
	}
	h.bot.sessions.Set(session)
	if err := h.bot.sessions.Save(); err != nil {
		logger.Error().Err(err).Msg("failed to save session after status post")
	}
	if err := h.bot.AddReaction(ev.Channel, ev.TimeStamp, session.ModelReactionEmoji); err != nil {
		logger.Debug().Err(err).Msg("failed to add model reaction")
	}
	if session.PermissionMode == "plan" {
		if err := h.bot.AddReaction(ev.Channel, ev.TimeStamp, planModeEmoji); err != nil {
			logger.Debug().Err(err).Msg("failed to add plan-mode reaction")
		}
	}

	// Resolve any Slack permalinks in the prompt. If a confirmation
	// dialog is needed (private or over-cap refs) the launch is
	// deferred until the user clicks a button; otherwise we splice
	// the referenced content and start immediately.
	launch := func(finalPrompt string) {
		h.runClod(
			ctx,
			ev.Channel,
			ev.User,
			taskPath,
			taskName,
			finalPrompt,
			"",
			threadTS,
			logger,
		)
	}
	cancel := func() {
		if _, err := h.bot.PostMessage(ev.Channel,
			":x: Task cancelled — referenced content couldn't be confirmed.",
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post slackref-cancel notice")
		}
	}
	finalPrompt, proceed := h.resolveAndRouteRefs(
		ev.Channel, threadTS, taskPath, prompt, ev.User, launch, cancel, logger,
	)
	if !proceed {
		// Dialog posted; button handler will take over.
		return
	}
	launch(finalPrompt)
}

// monitorStartPattern pulls the task id out of Monitor's success text
// ("Monitor started (task b85o0dvlc, timeout …"). It tolerates any suffix.
var monitorStartPattern = regexp.MustCompile(`Monitor started \(task ([a-z0-9]+)`)

// monitorCountEmojis maps a current count to the Slack reaction name we
// attach to the task's anchor message. Index 0 is unused (count 0 means
// no emoji); 1..10 are keycap digits; counts above 10 fall through to
// "1234" as the generic "many" marker.
var monitorCountEmojis = [...]string{
	"", "one", "two", "three", "four", "five",
	"six", "seven", "eight", "nine", "keycap_ten",
}

func monitorCountEmojiFor(count int) string {
	switch {
	case count <= 0:
		return ""
	case count < len(monitorCountEmojis):
		return monitorCountEmojis[count]
	default:
		return "1234"
	}
}

// syncMonitorCountEmoji brings the anchor-message reaction into line with
// the session's current ActiveMonitors count. Idempotent; safe to call
// whenever monitor state changes.
func (h *Handler) syncMonitorCountEmoji(channelID, threadTS string, logger zerolog.Logger) {
	session := h.bot.sessions.Get(channelID, threadTS)
	if session == nil || session.ReactionAnchorTS == "" {
		return
	}
	target := monitorCountEmojiFor(len(session.ActiveMonitors))
	if target == session.MonitorCountEmoji {
		return
	}
	if session.MonitorCountEmoji != "" {
		if err := h.bot.RemoveReaction(channelID, session.ReactionAnchorTS, session.MonitorCountEmoji); err != nil {
			logger.Debug().Err(err).Msg("failed to remove old monitor count emoji")
		}
	}
	if target != "" {
		if err := h.bot.AddReaction(channelID, session.ReactionAnchorTS, target); err != nil {
			logger.Debug().Err(err).Msg("failed to add monitor count emoji")
		}
	}
	h.bot.sessions.SetMonitorCountEmoji(channelID, threadTS, target)
	if err := h.bot.sessions.Save(); err != nil {
		logger.Debug().Err(err).Msg("failed to save session after monitor emoji update")
	}
}

// safeTaskNamePattern enforces a conservative set of characters for task
// names we're willing to materialize on disk. Anything else is treated as
// a malformed mention and falls through to the normal "unknown task" error.
var safeTaskNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// maybePromptInit posts an interactive setup prompt when the user mentions a
// task whose directory is missing or lacks `.clod/`. Returns true iff a
// prompt was posted and the caller should stop processing. Returns false to
// let the caller fall back to the stock "unknown task" error.
func (h *Handler) maybePromptInit(
	ev *slackevents.AppMentionEvent,
	threadTS string,
	parsed *ParsedMention,
	logger zerolog.Logger,
) bool {
	if !safeTaskNamePattern.MatchString(parsed.TaskName) {
		return false
	}
	base := h.bot.tasks.BasePath()
	if base == "" {
		return false
	}

	taskPath := filepath.Join(base, parsed.TaskName)

	// Distinguish "dir doesn't exist" from "dir exists but uninitialized".
	var createDir bool
	info, err := os.Stat(taskPath)
	switch {
	case os.IsNotExist(err):
		createDir = true
	case err != nil:
		logger.Warn().Err(err).Msg("stat of task path failed; not offering init")
		return false
	case !info.IsDir():
		logger.Warn().Str("path", taskPath).Msg("task path exists but isn't a directory; not offering init")
		return false
	default:
		// Directory exists. If `.clod/system/run` is present we should NOT
		// be here (discovery would have registered the task) — defensive.
		if _, err := os.Stat(filepath.Join(taskPath, ".clod", "system", "run")); err == nil {
			return false
		}
		createDir = false
	}

	return h.postInitPrompt(ev, threadTS, parsed.TaskName, taskPath, createDir, parsed.Instructions, logger)
}

// postInitPrompt builds the pendingInit state and posts the setup block
// message. Shared by maybePromptInit (subdirectory tasks) and
// handleRootTask (the agents dir itself). The caller is responsible for
// deciding whether an init prompt is warranted (whether `.clod/` is set
// up, whether the dir needs to be created) — this function just does
// the UI posting. Returns false if the prompt couldn't be posted.
func (h *Handler) postInitPrompt(
	ev *slackevents.AppMentionEvent,
	threadTS string,
	taskName, taskPath string,
	createDir bool,
	instructions string,
	logger zerolog.Logger,
) bool {
	progressKey := key(ev.Channel, threadTS)
	// If an init prompt is already pending for this thread, skip — don't
	// stack prompts.
	if _, already := h.pendingInits.Load(progressKey); already {
		logger.Debug().Msg("init prompt already pending for this thread")
		return true
	}

	base := h.bot.tasks.BasePath()
	packages := initPackageSuggestions(base, taskName)
	// Preselect the baseline defaults (by index) so one click gives a
	// reasonable setup.
	defaultsSet := map[string]bool{}
	for _, p := range defaultAptPackages {
		defaultsSet[p] = true
	}
	var selPkgs []string
	for i, p := range packages {
		if defaultsSet[p] {
			selPkgs = append(selPkgs, fmt.Sprintf("%d", i))
		}
	}

	// Preselect the bot's configured default model, falling back to the
	// "sonnet" option (which is the second entry and lines up with the
	// fallbackModel constant used elsewhere).
	selModel := h.defaultModel
	if selModel == "" {
		selModel = fallbackModel
	}
	templates := discoverTemplateTasks(base, taskName)
	pi := &pendingInit{
		ChannelID:    ev.Channel,
		ThreadTS:     threadTS,
		TaskName:     taskName,
		TaskPath:     taskPath,
		CreateDir:    createDir,
		Instructions: instructions,
		UserID:       ev.User,
		MentionTS:    ev.TimeStamp,
		Packages:     packages,
		Templates:    templates,
		SelImage:     initImageOptions[0].Value,
		SelSSH:       initSSHOptions[0].Value,
		SelModel:     selModel,
		SelTemplate:  "",
		SelPackages:  selPkgs,
	}

	// Two-step flow: when any template is available, post Step 1
	// (template-or-custom picker) first. Otherwise skip directly to
	// Step 2 so the user isn't stuck on a one-option radio.
	var blocks []slack.Block
	if len(templates) > 0 {
		pi.Phase = initPhaseTemplatePicker
		blocks = buildInitStep1Blocks(pi, progressKey)
	} else {
		pi.Phase = initPhaseCustomDetail
		blocks = buildInitPromptBlocks(pi, progressKey)
	}
	msgTS, err := h.bot.PostMessageBlocks(ev.Channel, blocks, threadTS)
	if err != nil {
		// Dump the rejected payload so we can see which block Slack
		// is complaining about. invalid_blocks keeps recurring for
		// reasons that aren't obvious from the error message alone.
		if dump, mErr := json.MarshalIndent(blocks, "", "  "); mErr == nil {
			logger.Error().Err(err).RawJSON("blocks", dump).Msg("failed to post init prompt")
		} else {
			logger.Error().Err(err).Msg("failed to post init prompt")
		}
		return false
	}
	pi.MessageTS = msgTS
	h.pendingInits.Store(progressKey, pi)
	logger.Info().
		Bool("create_dir", createDir).
		Str("task_path", taskPath).
		Str("message_ts", msgTS).
		Msg("posted task init prompt")
	return true
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

	launch := func(finalPrompt string) {
		h.runClod(
			ctx,
			ev.Channel,
			ev.User,
			session.TaskPath,
			session.TaskName,
			finalPrompt,
			session.SessionID,
			threadTS,
			logger,
		)
	}
	cancel := func() {
		if _, err := h.bot.PostMessage(ev.Channel,
			":x: Continuation cancelled — referenced content couldn't be confirmed.",
			threadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post slackref-cancel notice")
		}
	}
	finalPrompt, proceed := h.resolveAndRouteRefs(
		ev.Channel, threadTS, session.TaskPath, instructions, ev.User, launch, cancel, logger,
	)
	if proceed {
		launch(finalPrompt)
	}
}

// ResumeActiveSessions is called once at bot startup. Any session still
// flagged Active in sessions.json (the bot previously crashed / was killed
// / timed out without a clean completion) gets revived: we post a notice
// in the thread and spawn runClod with --resume + a nudge prompt that
// tells the agent to pick up where it left off. Sessions whose UpdatedAt
// is older than maxAge are considered stale — we clear the flag without
// resuming so stopped-then-left-overnight threads don't all wake up at
// once.
func (h *Handler) ResumeActiveSessions(ctx context.Context, maxAge time.Duration) {
	fresh, stale := h.bot.sessions.ActiveSessions(maxAge)

	logger := h.logger.With().
		Int("fresh", len(fresh)).
		Int("stale", len(stale)).
		Dur("max_age", maxAge).
		Logger()

	// Clear stale flags first so a subsequent crash during resume doesn't
	// leave them hanging.
	for _, s := range stale {
		age := time.Since(s.UpdatedAt)
		h.logger.Info().
			Str("channel", s.ChannelID).
			Str("thread_ts", s.ThreadTS).
			Str("task", s.TaskName).
			Dur("idle", age).
			Msg("skipping stale active session")
		h.bot.sessions.SetActive(s.ChannelID, s.ThreadTS, false)
	}
	if len(stale) > 0 {
		if err := h.bot.sessions.Save(); err != nil {
			logger.Error().Err(err).Msg("failed to persist cleared stale flags")
		}
	}

	if len(fresh) == 0 {
		logger.Debug().Msg("no fresh active sessions to resume")
		return
	}

	logger.Info().Msg("resuming active sessions after bot restart")

	for _, s := range fresh {
		s := s // capture for goroutine
		go h.resumeOneSession(ctx, s)
	}
}

// resumeOneSession revives a single previously-active thread. Posts a
// notice so the user knows what happened, then synthesizes a "continue
// where you left off" prompt and runs clod via the normal session-resume
// path. Gated per-thread by runningTasks so a duplicate resume doesn't
// race with a human @-mention that arrives at the same moment.
func (h *Handler) resumeOneSession(ctx context.Context, s *SessionMapping) {
	logger := h.logger.With().
		Str("channel", s.ChannelID).
		Str("thread_ts", s.ThreadTS).
		Str("task", s.TaskName).
		Str("session_id", s.SessionID).
		Logger()

	progressKey := key(s.ChannelID, s.ThreadTS)
	if _, alreadyRunning := h.runningTasks.Load(progressKey); alreadyRunning {
		logger.Debug().Msg("thread already has a running task; skipping auto-resume")
		return
	}

	// Don't resume if we have no session_id to resume against — claude
	// won't know what conversation to pick up. Clear the flag so we
	// don't keep trying.
	if s.SessionID == "" {
		logger.Warn().Msg("active session missing session_id; clearing flag without resuming")
		h.bot.sessions.SetActive(s.ChannelID, s.ThreadTS, false)
		if err := h.bot.sessions.Save(); err != nil {
			logger.Debug().Err(err).Msg("failed to clear active flag")
		}
		return
	}

	// Verify the task dir is still on disk (user may have rm'd it).
	if _, err := os.Stat(filepath.Join(s.TaskPath, ".clod", "system", "run")); err != nil {
		logger.Warn().Str("task_path", s.TaskPath).Err(err).
			Msg("active session's task path no longer exists; clearing flag without resuming")
		h.bot.sessions.SetActive(s.ChannelID, s.ThreadTS, false)
		if err := h.bot.sessions.Save(); err != nil {
			logger.Debug().Err(err).Msg("failed to clear active flag")
		}
		return
	}

	// Monitors from the previous run died with that container. Clear the
	// session's list and remove the count emoji so the anchor reflects
	// "none active yet"; the agent will re-announce any monitors it
	// restarts and we'll rebuild the count from those Monitor starts.
	if s.MonitorCountEmoji != "" {
		if err := h.bot.RemoveReaction(s.ChannelID, s.ReactionAnchorTS, s.MonitorCountEmoji); err != nil {
			logger.Debug().Err(err).Msg("failed to remove monitor count emoji on resume")
		}
	}
	h.bot.sessions.ClearMonitors(s.ChannelID, s.ThreadTS)
	h.bot.sessions.SetMonitorCountEmoji(s.ChannelID, s.ThreadTS, "")

	if _, err := h.bot.PostMessage(
		s.ChannelID,
		fmt.Sprintf(":arrows_counterclockwise: Resuming task `%s` after bot restart…", s.TaskName),
		s.ThreadTS,
	); err != nil {
		logger.Debug().Err(err).Msg("failed to post resume notice")
	}

	nudge := "The bot was restarted while this task was in progress. " +
		"Continue where you left off: if you had state worth saving, check it now; " +
		"if you were monitoring a background process, verify it's still alive and restart it if needed; " +
		"otherwise simply resume the work. Do not redo steps that already completed."

	h.runClod(
		ctx,
		s.ChannelID,
		s.UserID,
		s.TaskPath,
		s.TaskName,
		nudge,
		s.SessionID,
		s.ThreadTS,
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

	// Per-thread permission mode too (currently: plan mode on/off via
	// the :thought_balloon: reaction). Empty falls through to the
	// Runner's bot-wide default inside Start().
	permissionMode := h.bot.sessions.GetPermissionMode(channelID, threadTS)

	// Sticky `@bot !:` flag — keeps resumes and continuations in the
	// same execution mode the user originally confirmed.
	useClaudeDirect := h.bot.sessions.IsUseClaudeDirect(channelID, threadTS)

	// Start the task
	task, err := h.bot.runner.Start(ctx, taskPath, prompt, sessionID, model, permissionMode, useClaudeDirect)
	if err != nil {
		logger.Error().Err(err).Msg("failed to start clod")
		if _, postErr := h.bot.PostMessage(channelID, fmt.Sprintf(":x: Failed to start task: %v", err), threadTS); postErr != nil {
			logger.Error().Err(postErr).Msg("failed to post task start error message")
		}
		return
	}

	// Flag the session as actively running so a bot restart can resume it.
	// The flag is only cleared on clean completion below — shutdown, crash,
	// or timeout leaves it set so the next startup picks it up.
	h.bot.sessions.SetActive(channelID, threadTS, true)
	if err := h.bot.sessions.Save(); err != nil {
		logger.Debug().Err(err).Msg("failed to persist active flag")
	}

	// Register the running task
	progressKey := key(channelID, threadTS)
	h.runningTasks.Store(progressKey, task)
	defer h.runningTasks.Delete(progressKey)
	defer h.pendingPermissions.Delete(progressKey) // Clean up any pending permission state

	// Start watching for output files to upload to Slack.
	outputWatchDone := make(chan struct{})
	// shouldSync is polled every ~2s inside the watcher; honoring the
	// thread preference live means `@bot set filesync=off/on` takes
	// effect immediately without requiring a task restart.
	shouldSync := func() bool {
		return !h.bot.sessions.IsFileSyncDisabled(channelID, threadTS)
	}
	go h.bot.files.WatchOutputs(taskPath, channelID, threadTS, outputWatchDone, shouldSync)
	defer close(outputWatchDone)

	// Output batching
	const batchInterval = 2 * time.Second
	const maxBatchLen = 1500 // Leave room for formatting in Slack's 4000 char limit

	var outputBuffer strings.Builder
	ticker := time.NewTicker(batchInterval)
	defer ticker.Stop()
	// lastHeartbeat records when we last bumped the session's UpdatedAt
	// via Touch+Save. See the ticker case for the gating rationale.
	lastHeartbeat := time.Now()

	// Function to flush the buffer with message consolidation.
	threadKey := key(channelID, threadTS)
	flushBuffer := func() {
		if outputBuffer.Len() > 0 {
			// Trim only leading/trailing NEWLINES, tabs, and carriage
			// returns — not spaces. A streaming chunk often begins with
			// a single space because it's the continuation of the prior
			// chunk's sentence (claude sends "Models" then " loading.");
			// TrimSpace would eat that boundary space and the consolidation
			// join below would glue the two chunks into "Modelsloading."
			// since neither edge has a newline or fence. Tabs/newlines at
			// the message edges are still noise worth trimming for tidy
			// Slack posts.
			newContent := strings.Trim(outputBuffer.String(), "\n\r\t")
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
				//
				// runNewTask has already populated the session with
				// per-thread preferences (FileSyncDisabled,
				// PermissionMode, Model, ReactionAnchorTS, etc.); only
				// the session_id is new here, so we patch the existing
				// record rather than replace it. Early versions of this
				// branch constructed a fresh SessionMapping and
				// wrote-overwrote those preferences back to defaults
				// — specifically, that's how root-task defaults like
				// "filesync off" and "plan off" kept silently
				// disappearing a few seconds after task start.
				session := h.bot.sessions.Get(channelID, threadTS)
				if session == nil {
					session = &SessionMapping{
						ChannelID: channelID,
						ThreadTS:  threadTS,
						TaskName:  taskName,
						TaskPath:  taskPath,
						UserID:    userID,
						CreatedAt: time.Now(),
					}
				}
				session.SessionID = sid
				// Backfill any fields that might be missing on a
				// session predating the field; preserve any values
				// the session already has.
				if session.TaskName == "" {
					session.TaskName = taskName
				}
				if session.TaskPath == "" {
					session.TaskPath = taskPath
				}
				if session.UserID == "" {
					session.UserID = userID
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

			// Drop trivial tool results ("(Bash completed with no output)"
			// and similar) at default verbosity. At verbosity >= 1 we strip
			// the prefix and let the normal output path handle it. This
			// runs before any other prefix check because the underlying
			// content is a normal __SNIPPET__ / fenced block that those
			// checks expect to see raw.
			if strings.HasPrefix(content, "__TRIVIAL__") {
				verbosityLevel := h.bot.sessions.GetVerbosityLevel(channelID, threadTS)
				if verbosityLevel < 1 {
					continue
				}
				content = strings.TrimPrefix(content, "__TRIVIAL__")
			}

			// Surface wrapper/docker progress while the container is
			// being prepared. Update a single rolling message so the user
			// sees build steps happening instead of ~90s of silence on a
			// fresh task.
			if strings.HasPrefix(content, "__PROGRESS__") {
				line := strings.TrimPrefix(content, "__PROGRESS__")
				h.updateProgressMessage(channelID, threadTS, line, logger)
				continue
			}

			// Check for special stats message.
			if strings.HasPrefix(content, "__STATS__") {
				h.clearProgressMessage(channelID, threadTS, logger)
				flushBuffer() // Flush any pending output first.
				h.postStatsMessage(channelID, threadTS, content[9:]) // Skip "__STATS__" prefix.
				// Clear consolidation since stats message breaks the chain.
				h.lastOutputMsg.Delete(threadKey)
				continue
			}

			// Check for snippet message (tool output to upload as collapsible file).
			if strings.HasPrefix(content, "__SNIPPET__") {
				h.clearProgressMessage(channelID, threadTS, logger)
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

			// Real claude output arrived — the rolling "preparing
			// environment" message has served its purpose; finalize it.
			h.clearProgressMessage(channelID, threadTS, logger)

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
					// For ExitPlanMode with a long plan, upload the full
					// plan as a snippet first so the user can read the
					// portion that won't fit in the truncated prompt.
					planAttached := h.maybeUploadLongPlan(req, channelID, threadTS, logger)
					blocks := h.buildPermissionBlocks(req, progressKey, planAttached)
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
					planAttached := h.maybeUploadLongPlan(req, channelID, threadTS, logger)
					blocks := h.buildPermissionBlocks(req, progressKey, planAttached)
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
			// Periodic flush + heartbeat. The heartbeat bumps the
			// session's UpdatedAt so resume-on-restart can judge
			// whether an Active session died "just now" (worth
			// resuming) or "a long time ago" (stale — skip). Gated
			// at 30s so we're not hammering the sessions.json file
			// every 2s tick.
			flushBuffer()
			if time.Since(lastHeartbeat) >= 30*time.Second {
				h.bot.sessions.Touch(channelID, threadTS)
				if err := h.bot.sessions.Save(); err != nil {
					logger.Debug().Err(err).Msg("heartbeat save failed")
				}
				lastHeartbeat = time.Now()
			}

		case result := <-task.Done():
			// Task completed
			flushBuffer()
			h.finalizeTask(channelID, threadTS, taskName, taskPath, userID, result, logger)
			return
		}
	}

done:
	// Wait for final result if we exited via output channel close.
	result := <-task.Done()
	h.finalizeTask(channelID, threadTS, taskName, taskPath, userID, result, logger)
}

// finalizeTask posts the completion message, saves the session mapping,
// and clears the Active flag *only* on clean completion. An error exit
// (crash, timeout, shutdown cancel) leaves Active set so the next bot
// startup can resume-or-skip based on the staleness threshold.
func (h *Handler) finalizeTask(
	channelID, threadTS, taskName, taskPath, userID string,
	result *Result,
	logger zerolog.Logger,
) {
	var finalMsg string
	cleanExit := result.Error == nil
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

	// The task's container is gone regardless of exit code, so any
	// monitors that were running inside it are dead too. Drop them from
	// the session and clear the count emoji so the anchor doesn't show
	// a stale "N monitors active".
	if session := h.bot.sessions.Get(channelID, threadTS); session != nil && session.ReactionAnchorTS != "" && session.MonitorCountEmoji != "" {
		if err := h.bot.RemoveReaction(channelID, session.ReactionAnchorTS, session.MonitorCountEmoji); err != nil {
			logger.Debug().Err(err).Msg("failed to remove stale monitor count emoji")
		}
	}
	h.bot.sessions.ClearMonitors(channelID, threadTS)
	h.bot.sessions.SetMonitorCountEmoji(channelID, threadTS, "")

	if result.SessionID != "" {
		session := h.bot.sessions.Get(channelID, threadTS)
		if session == nil {
			session = &SessionMapping{
				ChannelID: channelID,
				ThreadTS:  threadTS,
				TaskName:  taskName,
				TaskPath:  taskPath,
				UserID:    userID,
				CreatedAt: time.Now(),
			}
		}
		session.SessionID = result.SessionID
		session.TaskName = taskName
		session.TaskPath = taskPath
		h.bot.sessions.Set(session)
		if cleanExit {
			// Route through SetActive so the transition is logged
			// alongside every other clear of the Active flag.
			h.bot.sessions.SetActive(channelID, threadTS, false)
		}
		if err := h.bot.sessions.Save(); err != nil {
			logger.Error().Err(err).Msg("failed to save sessions")
		}
	} else if cleanExit {
		// No session_id captured (e.g., failure before init). Clear
		// the Active flag anyway so we don't try to resume a task
		// claude never accepted.
		h.bot.sessions.SetActive(channelID, threadTS, false)
		if err := h.bot.sessions.Save(); err != nil {
			logger.Debug().Err(err).Msg("failed to clear active flag")
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
// maybeUploadLongPlan uploads an ExitPlanMode tool's `plan` field as a
// Slack snippet when it's too long to fit in the permission-prompt
// section (Slack's 3000-char section-text cap). Returns true if a snippet
// was successfully posted so the caller knows to swap the truncation
// marker for an "attached file" pointer. No-op for any other tool or for
// plans that fit inline.
const maxInlinePlanLen = 2800

func (h *Handler) maybeUploadLongPlan(req PermissionRequest, channelID, threadTS string, logger zerolog.Logger) bool {
	if req.ToolName != "ExitPlanMode" {
		return false
	}
	plan, ok := req.ToolInput["plan"].(string)
	if !ok || len(plan) <= maxInlinePlanLen {
		return false
	}
	if _, err := h.bot.files.UploadSnippet(plan, "plan.md", ":bookmark_tabs: Full proposed plan", channelID, threadTS); err != nil {
		logger.Debug().Err(err).Msg("failed to upload long plan snippet")
		return false
	}
	return true
}

func (h *Handler) buildPermissionBlocks(req PermissionRequest, progressKey string, planAttached bool) []slack.Block {
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
	case "ExitPlanMode":
		// ExitPlanMode's input carries the full plan markdown, which can
		// run many KB. Rendering it inline via the generic fallback
		// blows past Slack's 3000-char section-text cap and Slack rejects
		// the whole block with "invalid_blocks", dropping the permission
		// prompt. Render as-is (it's already markdown) and truncate to
		// fit the cap.
		if plan, ok := req.ToolInput["plan"].(string); ok {
			detailText = "*Proposed plan:*\n" + plan
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
		// Slack caps section text at 3000 chars. Hit that and the whole
		// prompt is rejected with "invalid_blocks". Hard-trim with an
		// indicator so the user sees *something* rather than nothing.
		const maxSectionText = 2900
		if len(detailText) > maxSectionText {
			marker := "_(truncated)_"
			if req.ToolName == "ExitPlanMode" && planAttached {
				marker = "_(truncated — full plan attached as file in the thread)_"
			}
			detailText = detailText[:maxSectionText] + "\n…" + marker
		}
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
	isInitSelect := action.ActionID == "init_image" ||
		action.ActionID == "init_ssh" ||
		action.ActionID == "init_model" ||
		action.ActionID == "init_template" ||
		action.ActionID == "init_step1_choice" ||
		action.ActionID == "init_packages"
	isInitFinal := action.ActionID == "init_create" ||
		action.ActionID == "init_cancel" ||
		action.ActionID == "init_step1_next"
	isDangerousFinal := action.ActionID == "dangerous_proceed" ||
		action.ActionID == "dangerous_cancel"
	isSlackRefFinal := action.ActionID == "slackref_inline" ||
		action.ActionID == "slackref_asset" ||
		action.ActionID == "slackref_skip" ||
		action.ActionID == "slackref_cancel"
	isHomeRefresh := action.ActionID == "home_refresh"
	if !isPermissionAction && !isAmbiguousAction && !isAskQuestionSelect && !isAskQuestionFinal && !isInitSelect && !isInitFinal && !isDangerousFinal && !isSlackRefFinal && !isHomeRefresh {
		logger.Debug().Msg("ignoring non-permission action")
		return
	}

	// Home-tab refresh is a self-contained interaction: re-render
	// the view for the clicking user. Short-circuits before the
	// permission-action decode since the action value is just a
	// static string rather than our PermissionActionValue JSON.
	if isHomeRefresh {
		h.handleHomeRefresh(ctx, callback, logger)
		return
	}

	if isAskQuestionSelect {
		h.handleAskQuestionSelect(callback, action, logger)
		return
	}
	if isInitSelect {
		h.handleInitSelect(callback, action, logger)
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

	if isInitFinal {
		h.handleInitFinal(ctx, callback, action, actionValue, logger)
		return
	}

	if isDangerousFinal {
		h.handleDangerousFinal(ctx, callback, action, actionValue, logger)
		return
	}

	if isSlackRefFinal {
		h.handleSlackRefFinal(callback, action, actionValue, logger)
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

	// Post-resolution hooks (plan-mode exit, etc.).
	h.afterPermissionResolved(pending.ChannelID, pending.ThreadTS, pending.ToolName, actionValue.Behavior, logger)
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

// updateProgressMessage surfaces a wrapper/docker-build progress line to
// Slack as a single rolling message in the thread. The message shows the
// last few lines so the user can see progress without scroll noise. First
// call posts the message; subsequent calls edit it in place.
func (h *Handler) updateProgressMessage(channelID, threadTS, line string, logger zerolog.Logger) {
	const maxLines = 6
	k := key(channelID, threadTS)
	val, _ := h.progressMessages.LoadOrStore(k, &progressMsg{})
	pm := val.(*progressMsg)

	// Trim long lines so a single docker step with a huge RUN doesn't
	// blow past Slack's per-message limits.
	const maxLineLen = 300
	if len(line) > maxLineLen {
		line = line[:maxLineLen-1] + "…"
	}
	pm.Lines = append(pm.Lines, line)
	if len(pm.Lines) > maxLines {
		pm.Lines = pm.Lines[len(pm.Lines)-maxLines:]
	}

	body := ":hourglass_flowing_sand: *Preparing environment*\n```\n" +
		strings.Join(pm.Lines, "\n") + "\n```"

	if pm.MessageTS == "" {
		ts, err := h.bot.PostMessage(channelID, body, threadTS)
		if err != nil {
			logger.Debug().Err(err).Msg("failed to post progress message")
			return
		}
		pm.MessageTS = ts
		return
	}
	if err := h.bot.UpdateMessage(channelID, pm.MessageTS, body); err != nil {
		logger.Debug().Err(err).Msg("failed to update progress message")
	}
}

// clearProgressMessage finalizes the rolling progress message once real
// claude output arrives, so the "Preparing environment" status stops
// competing with streamed content. We leave the final line history in
// place (with a ":white_check_mark:") as a breadcrumb rather than deleting
// it.
func (h *Handler) clearProgressMessage(channelID, threadTS string, logger zerolog.Logger) {
	k := key(channelID, threadTS)
	val, ok := h.progressMessages.LoadAndDelete(k)
	if !ok {
		return
	}
	pm := val.(*progressMsg)
	if pm.MessageTS == "" {
		return
	}
	body := ":white_check_mark: *Environment ready*\n```\n" +
		strings.Join(pm.Lines, "\n") + "\n```"
	if err := h.bot.UpdateMessage(channelID, pm.MessageTS, body); err != nil {
		logger.Debug().Err(err).Msg("failed to finalize progress message")
	}
}

// handleInitSelect records an image/ssh/packages change on an outstanding
// init prompt. State is keyed by the prompt's Slack MessageTS; Slack doesn't
// round-trip our thread key on radio/checkbox events the way it does with
// button values.
func (h *Handler) handleInitSelect(
	callback *slack.InteractionCallback,
	action *slack.BlockAction,
	logger zerolog.Logger,
) {
	msgTS := callback.Container.MessageTs
	var state *pendingInit
	h.pendingInits.Range(func(k, v any) bool {
		s := v.(*pendingInit)
		if s.MessageTS == msgTS {
			state = s
			return false
		}
		return true
	})
	if state == nil {
		logger.Debug().Str("message_ts", msgTS).Msg("no pending init for this message; prompt may be stale")
		return
	}

	switch action.ActionID {
	case "init_image":
		if action.SelectedOption.Value != "" {
			state.SelImage = action.SelectedOption.Value
		}
	case "init_ssh":
		if action.SelectedOption.Value != "" {
			state.SelSSH = action.SelectedOption.Value
		}
	case "init_model":
		if action.SelectedOption.Value != "" {
			state.SelModel = action.SelectedOption.Value
		}
	case "init_template":
		// Slack option values can't be empty strings, so the "(none)"
		// option carries a sentinel value — translate it back to ""
		// here since downstream logic keys "no template selected" off
		// an empty SelTemplate.
		v := action.SelectedOption.Value
		if v == noneTemplateSentinel {
			v = ""
		}
		state.SelTemplate = v
	case "init_step1_choice":
		// Step 1 radio: either a template name or the customSetupSentinel.
		// Translate the sentinel to empty so downstream code can
		// branch on "template chosen" vs "custom requested".
		v := action.SelectedOption.Value
		if v == customSetupSentinel {
			v = ""
		}
		state.SelTemplate = v
	case "init_packages":
		picks := make([]string, 0, len(action.SelectedOptions))
		for _, opt := range action.SelectedOptions {
			picks = append(picks, opt.Value)
		}
		state.SelPackages = picks
	}
	logger.Debug().
		Str("action_id", action.ActionID).
		Str("image", state.SelImage).
		Str("ssh", state.SelSSH).
		Str("model", state.SelModel).
		Str("template", state.SelTemplate).
		Strs("packages", state.SelPackages).
		Msg("recorded init selection")
}

// handleInitFinal handles Create/Cancel clicks on the init prompt. On Create
// it materializes `.clod/` with the chosen config, refreshes the task
// registry so the new task is discoverable, updates the prompt message, and
// kicks off the originally-requested task.
func (h *Handler) handleInitFinal(
	ctx context.Context,
	callback *slack.InteractionCallback,
	action *slack.BlockAction,
	actionValue PermissionActionValue,
	logger zerolog.Logger,
) {
	stateVal, ok := h.pendingInits.LoadAndDelete(actionValue.ThreadKey)
	if !ok {
		logger.Warn().Msg("no pending init prompt found; button is stale")
		if err := h.bot.UpdateMessage(
			callback.Channel.ID, callback.Message.Timestamp,
			":warning: This init prompt is no longer active.",
		); err != nil {
			logger.Error().Err(err).Msg("failed to update stale init message")
		}
		return
	}
	state := stateVal.(*pendingInit)

	if action.ActionID == "init_cancel" {
		outcome := fmt.Sprintf(":x: Setup cancelled by <@%s>.", callback.User.ID)
		if err := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, outcome); err != nil {
			logger.Error().Err(err).Msg("failed to update cancelled init message")
		}
		return
	}

	// Step 1 Next: branch on the user's template/custom choice.
	// When a template was chosen we run the single-shot template
	// materialization; when Custom was chosen we re-render the same
	// message as Step 2 and put the state back so the next click
	// finds it.
	if action.ActionID == "init_step1_next" {
		if state.SelTemplate != "" {
			h.completeInitWithTemplate(ctx, state, callback.User.ID, logger)
			return
		}
		// Custom setup: transition to Step 2 and preserve state.
		state.Phase = initPhaseCustomDetail
		progressKey := actionValue.ThreadKey
		blocks := buildInitPromptBlocks(state, progressKey)
		if err := h.bot.UpdateMessageBlocks(state.ChannelID, state.MessageTS, blocks); err != nil {
			logger.Error().Err(err).Msg("failed to update init prompt to step 2")
			// Fall through — on failure we've lost the state, warn the user.
			if _, perr := h.bot.PostMessage(state.ChannelID,
				":warning: Couldn't advance the setup prompt. Try the mention again.",
				state.ThreadTS); perr != nil {
				logger.Debug().Err(perr).Msg("failed to post advance-failed notice")
			}
			return
		}
		h.pendingInits.Store(progressKey, state)
		return
	}

	// Give the user immediate visual feedback that the click registered.
	// Copying a large template + the subsequent clod build can take
	// several seconds, and without this the prompt sits unchanged and
	// looks like the button didn't fire. The final outcome message
	// below replaces this placeholder.
	pendingMsg := fmt.Sprintf(":hourglass_flowing_sand: *Setting up `%s`…* (by <@%s>)", state.TaskName, callback.User.ID)
	if state.SelTemplate != "" {
		pendingMsg += fmt.Sprintf("\n_Copying template `%s`…_", state.SelTemplate)
	}
	if err := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, pendingMsg); err != nil {
		logger.Debug().Err(err).Msg("failed to post immediate setup-in-progress update")
	}

	// Materialize the task.
	var chosenPkgs []string
	for _, idxStr := range state.SelPackages {
		var i int
		if _, err := fmt.Sscanf(idxStr, "%d", &i); err != nil {
			continue
		}
		if i < 0 || i >= len(state.Packages) {
			continue
		}
		chosenPkgs = append(chosenPkgs, state.Packages[i])
	}

	// If the user picked a template, clone its contents into the new
	// task directory BEFORE writing `.clod/` config — writeInitFiles
	// overwrites the .clod/ files from the user's pickers, so template
	// .clod choices yield to the explicit UI selections.
	if state.SelTemplate != "" {
		base := h.bot.tasks.BasePath()
		srcPath := filepath.Join(base, state.SelTemplate)
		// Ensure the new task dir exists so the copy has a target.
		if err := os.MkdirAll(state.TaskPath, 0o755); err != nil {
			logger.Error().Err(err).Str("dst", state.TaskPath).Msg("failed to create task dir for template copy")
			msg := fmt.Sprintf(":x: Couldn't create the task directory: %v", err)
			if updErr := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, msg); updErr != nil {
				logger.Error().Err(updErr).Msg("failed to update init prompt after mkdir error")
			}
			return
		}
		if err := copyTaskTemplate(srcPath, state.TaskPath); err != nil {
			logger.Error().Err(err).Str("template", state.SelTemplate).Msg("failed to copy template")
			msg := fmt.Sprintf(":x: Couldn't copy template `%s`: %v", state.SelTemplate, err)
			if updErr := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, msg); updErr != nil {
				logger.Error().Err(updErr).Msg("failed to update init prompt after copy error")
			}
			return
		}
		// The copy may have placed a task dir; mark CreateDir=false so
		// writeInitFiles doesn't try to MkdirAll again (it's idempotent
		// but this keeps intent clear).
		state.CreateDir = false
	}

	if err := writeInitFiles(state, state.SelImage, state.SelSSH, chosenPkgs); err != nil {
		logger.Error().Err(err).Msg("failed to write init files")
		msg := fmt.Sprintf(":x: Couldn't create the task setup: %v", err)
		if updErr := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, msg); updErr != nil {
			logger.Error().Err(updErr).Msg("failed to update init prompt after write error")
		}
		return
	}

	// Refresh the registry so the new task is visible for subsequent
	// discovery. Registry.Refresh requires `.clod/system/run` to exist,
	// which it doesn't yet — clod generates that on first invocation. We
	// bypass discovery for this one call by manually constructing the
	// task path and invoking runClod directly.
	if err := h.bot.tasks.Refresh(); err != nil {
		logger.Warn().Err(err).Msg("failed to refresh task registry after init")
	}

	// Store the chosen model on the session so runClod + the anchor
	// indicator both pick it up when startTaskAfterInit fires.
	if state.SelModel != "" {
		h.bot.sessions.SetModel(state.ChannelID, state.ThreadTS, state.SelModel)
		if err := h.bot.sessions.Save(); err != nil {
			logger.Debug().Err(err).Msg("failed to persist chosen model")
		}
	}

	// Update the prompt with the outcome.
	var pkgLine, tplLine string
	if len(chosenPkgs) > 0 {
		pkgLine = fmt.Sprintf("\n• Packages: `%s`", strings.Join(chosenPkgs, ", "))
	}
	if state.SelTemplate != "" {
		tplLine = fmt.Sprintf("\n• Template: `%s`", state.SelTemplate)
	}
	outcome := fmt.Sprintf(
		":white_check_mark: *Task `%s` initialized* by <@%s>\n• Image: `%s`\n• SSH: `%s`\n• Model: `%s`%s%s\n_Starting the task now…_",
		state.TaskName, callback.User.ID, state.SelImage, state.SelSSH, state.SelModel, tplLine, pkgLine,
	)
	if err := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, outcome); err != nil {
		logger.Error().Err(err).Msg("failed to update init prompt after success")
	}

	// Kick off the originally-requested task.
	h.startTaskAfterInit(ctx, state, logger)
}

// handleDangerousFinal resolves a `@bot !:` confirmation dialog. On
// Proceed it marks the session as "run claude directly", rewrites the
// prompt message, and kicks off the task through runNewTask — the same
// path `@bot *:` uses, just with UseClaudeDirect sticky. On Cancel it
// updates the message and drops the pending state.
func (h *Handler) handleDangerousFinal(
	ctx context.Context,
	callback *slack.InteractionCallback,
	action *slack.BlockAction,
	actionValue PermissionActionValue,
	logger zerolog.Logger,
) {
	stateVal, ok := h.pendingDangerous.LoadAndDelete(actionValue.ThreadKey)
	if !ok {
		logger.Warn().Msg("no pending dangerous prompt found; button is stale")
		if err := h.bot.UpdateMessage(
			callback.Channel.ID, callback.Message.Timestamp,
			":warning: This confirmation is no longer active.",
		); err != nil {
			logger.Error().Err(err).Msg("failed to update stale dangerous message")
		}
		return
	}
	state := stateVal.(*pendingDangerous)

	// Only the user who asked for `!:` can confirm — a different person
	// clicking Proceed shouldn't authorize a sandbox bypass on someone
	// else's behalf. Anyone can Cancel (including the requester
	// themselves).
	if action.ActionID == "dangerous_proceed" && callback.User.ID != state.RequesterID {
		logger.Warn().
			Str("clicked_by", callback.User.ID).
			Str("requester", state.RequesterID).
			Msg("non-requester tried to proceed on dangerous prompt")
		// Put the state back so the requester can still confirm.
		h.pendingDangerous.Store(actionValue.ThreadKey, state)
		return
	}

	if action.ActionID == "dangerous_cancel" {
		outcome := fmt.Sprintf(":x: Host-direct run cancelled by <@%s>.", callback.User.ID)
		if err := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, outcome); err != nil {
			logger.Error().Err(err).Msg("failed to update cancelled dangerous message")
		}
		return
	}

	// Proceed. Update the prompt to reflect the decision, mark the
	// session as claude-direct, then run through the normal root-task
	// path.
	outcome := fmt.Sprintf(
		":rotating_light: Host-direct run approved by <@%s>. Running claude without the container sandbox.",
		callback.User.ID,
	)
	if err := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, outcome); err != nil {
		logger.Error().Err(err).Msg("failed to update dangerous message after proceed")
	}

	base := h.bot.tasks.BasePath()
	if base == "" {
		if _, err := h.bot.PostMessage(state.ChannelID,
			":warning: Can't run a host-direct task — `CLOD_BOT_AGENTS_PATH` isn't set.",
			state.ThreadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post dangerous-final base-missing error")
		}
		return
	}
	taskName := filepath.Base(base)

	h.bot.sessions.SetUseClaudeDirect(state.ChannelID, state.ThreadTS, true)
	if err := h.bot.sessions.Save(); err != nil {
		logger.Debug().Err(err).Msg("failed to persist UseClaudeDirect flag")
	}

	// Synthesize an AppMentionEvent so we can reuse runNewTask verbatim.
	// Only the fields runNewTask reads are populated (Channel, TimeStamp,
	// User); ThreadTimeStamp is set to the real threadTS so any stray
	// read is consistent.
	synthetic := &slackevents.AppMentionEvent{
		Channel:         state.ChannelID,
		User:            state.RequesterID,
		TimeStamp:       state.MentionTS,
		ThreadTimeStamp: state.ThreadTS,
	}
	logger = logger.With().
		Str("task", taskName).
		Str("task_path", base).
		Bool("claude_direct", true).
		Logger()
	h.runNewTask(ctx, synthetic, state.ThreadTS, taskName, base, state.Instructions, logger)
}

// handleSlackRefFinal resolves a slack-permalink-expansion dialog. The
// four button variants:
//
//   - slackref_inline — splice the confirm-needed refs' text into the
//     prompt (like auto-inline public refs).
//   - slackref_asset — save each confirm-needed ref as a conversation
//     asset under the task dir and splice a pointer into the prompt.
//   - slackref_skip — drop the confirm-needed refs entirely (leave the
//     URLs in the prompt as plain text).
//   - slackref_cancel — cancel the whole turn; calls state.OnCancel.
//
// Only the original requester can click Proceed-style buttons; anyone
// authorized in the thread can Cancel.
func (h *Handler) handleSlackRefFinal(
	callback *slack.InteractionCallback,
	action *slack.BlockAction,
	actionValue PermissionActionValue,
	logger zerolog.Logger,
) {
	stateVal, ok := h.pendingSlackRefs.LoadAndDelete(actionValue.ThreadKey)
	if !ok {
		logger.Warn().Msg("no pending slackref dialog found; button is stale")
		if err := h.bot.UpdateMessage(
			callback.Channel.ID, callback.Message.Timestamp,
			":warning: This reference confirmation is no longer active.",
		); err != nil {
			logger.Error().Err(err).Msg("failed to update stale slackref message")
		}
		return
	}
	state := stateVal.(*pendingSlackRefState)

	// Only the requester can take a Proceed-style action; anyone can Cancel.
	if action.ActionID != "slackref_cancel" && callback.User.ID != state.RequesterID {
		logger.Warn().
			Str("clicked_by", callback.User.ID).
			Str("requester", state.RequesterID).
			Msg("non-requester tried to resolve slackref dialog")
		h.pendingSlackRefs.Store(actionValue.ThreadKey, state)
		return
	}

	switch action.ActionID {
	case "slackref_cancel":
		outcome := fmt.Sprintf(":x: Reference handling cancelled by <@%s>.", callback.User.ID)
		if err := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, outcome); err != nil {
			logger.Error().Err(err).Msg("failed to update cancelled slackref message")
		}
		if state.OnCancel != nil {
			state.OnCancel()
		}
		return

	case "slackref_skip":
		outcome := fmt.Sprintf(":arrow_right: References skipped by <@%s>.", callback.User.ID)
		if err := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, outcome); err != nil {
			logger.Debug().Err(err).Msg("failed to update skipped slackref message")
		}
		finalPrompt := buildPromptWithRefs(state.PromptBase, state.InlineRefs, nil, h.userCacheMap(), h.bot.client, logger)
		state.OnFinalize(finalPrompt)
		return

	case "slackref_inline":
		// Per the dialog construction, Include-inline is only offered
		// when nothing is over-cap — merge the confirm refs with the
		// auto-inline set.
		allInline := append([]*SlackRefResult{}, state.InlineRefs...)
		allInline = append(allInline, state.ConfirmRefs...)
		outcome := fmt.Sprintf(":eyes: References included inline by <@%s>.", callback.User.ID)
		if err := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, outcome); err != nil {
			logger.Debug().Err(err).Msg("failed to update inline slackref message")
		}
		finalPrompt := buildPromptWithRefs(state.PromptBase, allInline, nil, h.userCacheMap(), h.bot.client, logger)
		state.OnFinalize(finalPrompt)
		return

	case "slackref_asset":
		cache := h.userCacheMap()
		var assetNotes []string
		for _, r := range state.ConfirmRefs {
			assetDir, err := SaveConversationAsset(
				state.TaskPath, r, cache, h.bot.client, logger,
			)
			if err != nil {
				logger.Error().Err(err).Str("permalink", r.Ref.Permalink).Msg("failed to save conversation asset")
				if _, perr := h.bot.PostMessage(state.ChannelID,
					fmt.Sprintf(":warning: Couldn't save conversation asset for <%s>: %v", r.Ref.Permalink, err),
					state.ThreadTS); perr != nil {
					logger.Debug().Err(perr).Msg("failed to post asset-save error")
				}
				continue
			}
			relDir, relErr := filepath.Rel(state.TaskPath, assetDir)
			if relErr != nil {
				relDir = assetDir
			}
			assetNotes = append(assetNotes, fmt.Sprintf(
				"Referenced Slack conversation saved to `%s` (permalink: %s). Read `%s/thread.md` and any files in `%s/files/` for the full content.",
				relDir, r.Ref.Permalink, relDir, relDir,
			))
			if _, perr := h.bot.PostMessage(state.ChannelID,
				fmt.Sprintf(":floppy_disk: Saved referenced thread to `%s` (<%s|permalink>).", relDir, r.Ref.Permalink),
				state.ThreadTS); perr != nil {
				logger.Debug().Err(perr).Msg("failed to post asset-saved confirmation")
			}
		}
		h.mergeUserCache(cache)
		outcome := fmt.Sprintf(":floppy_disk: References saved as conversation assets by <@%s>.", callback.User.ID)
		if err := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, outcome); err != nil {
			logger.Debug().Err(err).Msg("failed to update asset slackref message")
		}
		finalPrompt := buildPromptWithRefs(state.PromptBase, state.InlineRefs, assetNotes, h.userCacheMap(), h.bot.client, logger)
		state.OnFinalize(finalPrompt)
		return
	}
}

// startTaskAfterInit posts the normal "Starting a task" message, anchors the
// model reaction on the user's mention, and runs clod with the instructions
// the user originally sent. Mirrors the tail of handleNewTask (post-registry
// lookup).
// completeInitWithTemplate finalizes an init prompt when Step 1's
// template radio was the selected action. Skips writeInitFiles
// entirely — the template's `.clod/` comes over as-is, with just the
// name patched — and hands off to the common task-start tail.
func (h *Handler) completeInitWithTemplate(
	ctx context.Context,
	state *pendingInit,
	userID string,
	logger zerolog.Logger,
) {
	pendingMsg := fmt.Sprintf(":hourglass_flowing_sand: *Setting up `%s`…* (by <@%s>)\n_Copying template `%s`…_", state.TaskName, userID, state.SelTemplate)
	if err := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, pendingMsg); err != nil {
		logger.Debug().Err(err).Msg("failed to post step1 setup-in-progress update")
	}

	base := h.bot.tasks.BasePath()
	tplPath := filepath.Join(base, state.SelTemplate)
	if err := materializeFromTemplate(tplPath, state.TaskPath, state.TaskName); err != nil {
		logger.Error().Err(err).
			Str("template", state.SelTemplate).
			Str("task_path", state.TaskPath).
			Msg("failed to materialize templated task")
		msg := fmt.Sprintf(":x: Couldn't copy template `%s`: %v", state.SelTemplate, err)
		if updErr := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, msg); updErr != nil {
			logger.Error().Err(updErr).Msg("failed to update init prompt after template copy error")
		}
		return
	}

	if err := h.bot.tasks.Refresh(); err != nil {
		logger.Debug().Err(err).Msg("task registry refresh after templated init")
	}

	outcome := fmt.Sprintf(":white_check_mark: *Task `%s` created* (by <@%s>) from template `%s`.", state.TaskName, userID, state.SelTemplate)
	if err := h.bot.UpdateMessage(state.ChannelID, state.MessageTS, outcome); err != nil {
		logger.Error().Err(err).Msg("failed to update init prompt after templated success")
	}

	h.startTaskAfterInit(ctx, state, logger)
}

func (h *Handler) startTaskAfterInit(ctx context.Context, p *pendingInit, logger zerolog.Logger) {
	startMsg := fmt.Sprintf(startMsgTemplate, p.TaskName)
	if _, err := h.bot.PostMessage(p.ChannelID, startMsg, p.ThreadTS); err != nil {
		logger.Error().Err(err).Msg("failed to post task start message after init")
	}

	// Same resolution order as runNewTask — see that function's
	// comment for rationale.
	initialModel := h.bot.sessions.GetModel(p.ChannelID, p.ThreadTS)
	if initialModel == "" {
		initialModel = readTaskClaudeSettingsModel(p.TaskPath)
	}
	if initialModel == "" {
		initialModel = h.defaultModel
	}
	if initialModel == "" {
		initialModel = fallbackModel
	}
	session := h.bot.sessions.Get(p.ChannelID, p.ThreadTS)
	if session == nil {
		session = &SessionMapping{
			ChannelID: p.ChannelID,
			ThreadTS:  p.ThreadTS,
			TaskName:  p.TaskName,
			TaskPath:  p.TaskPath,
			UserID:    p.UserID,
			CreatedAt: time.Now(),
		}
	}
	session.ReactionAnchorTS = p.MentionTS
	session.Model = initialModel
	session.ModelReactionEmoji = emojiForModel(initialModel)
	// Default plan mode on for a freshly-initialized task — same policy
	// as handleNewTask for tasks that already had a `.clod/` present.
	if session.PermissionMode == "" {
		session.PermissionMode = "plan"
	}
	session.TaskPath = p.TaskPath
	session.TaskName = p.TaskName
	h.bot.sessions.Set(session)
	if err := h.bot.sessions.Save(); err != nil {
		logger.Error().Err(err).Msg("failed to save session after init")
	}
	if err := h.bot.AddReaction(p.ChannelID, p.MentionTS, session.ModelReactionEmoji); err != nil {
		logger.Debug().Err(err).Msg("failed to add model reaction after init")
	}
	if session.PermissionMode == "plan" {
		if err := h.bot.AddReaction(p.ChannelID, p.MentionTS, planModeEmoji); err != nil {
			logger.Debug().Err(err).Msg("failed to add plan-mode reaction after init")
		}
	}

	launch := func(finalPrompt string) {
		h.runClod(
			ctx,
			p.ChannelID,
			p.UserID,
			p.TaskPath,
			p.TaskName,
			finalPrompt,
			"",
			p.ThreadTS,
			logger,
		)
	}
	cancel := func() {
		if _, err := h.bot.PostMessage(p.ChannelID,
			":x: Task cancelled — referenced content couldn't be confirmed.",
			p.ThreadTS); err != nil {
			logger.Debug().Err(err).Msg("failed to post slackref-cancel notice")
		}
	}
	finalPrompt, proceed := h.resolveAndRouteRefs(
		p.ChannelID, p.ThreadTS, p.TaskPath, p.Instructions, p.UserID, launch, cancel, logger,
	)
	if !proceed {
		return
	}
	launch(finalPrompt)
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
		// Post-resolution hooks (plan-mode exit, etc.).
		h.afterPermissionResolved(pending.ChannelID, pending.ThreadTS, pending.ToolName, resp.Behavior, logger)
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
	case "ExitPlanMode":
		if plan, ok := perm.ToolInput["plan"].(string); ok {
			detailText = "*Proposed plan:*\n" + plan
		}
	default:
		var parts []string
		for k, v := range perm.ToolInput {
			parts = append(parts, fmt.Sprintf("*%s:* `%v`", k, v))
		}
		detailText = strings.Join(parts, "\n")
	}

	if detailText != "" {
		const maxSectionText = 2900
		if len(detailText) > maxSectionText {
			detailText = detailText[:maxSectionText] + "\n…_(truncated)_"
		}
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
//
// Each stream-json `result` event carries per-interaction stats (one
// user message's cost/turns/duration), NOT a running total for the
// claude process — confirmed both by the Agent SDK cost-tracking docs
// and empirically (e.g. successive `num_turns` values of 79, 22, 1 in
// one process's results would be non-monotonic if cumulative). So we
// add each event into the session's running totals and render those.
// That scheme also survives process restarts: a `--resume` run's new
// per-interaction stats simply keep adding to the prior totals.
func (h *Handler) postStatsMessage(channelID, threadTS, statsJSON string) {
	var stats TaskStats
	if err := json.Unmarshal([]byte(statsJSON), &stats); err != nil {
		h.logger.Error().Err(err).Str("json", statsJSON).Msg("failed to parse stats JSON")
		return
	}

	cumulativeCost, cumulativeTurns := h.bot.sessions.AddStats(channelID, threadTS, stats.CostUSD, stats.NumTurns)
	if err := h.bot.sessions.Save(); err != nil {
		h.logger.Debug().Err(err).Msg("save after AddStats")
	}
	// Persist the just-appended usage sample to the sidecar. Cheap
	// — the file only grows with `result` events, not with
	// heartbeats.
	if err := h.bot.sessions.SaveUsage(); err != nil {
		h.logger.Debug().Err(err).Msg("save usage sidecar after AddStats")
	}

	// Format duration (this turn only — duration is not aggregated).
	duration := time.Duration(stats.DurationMS) * time.Millisecond
	var durationStr string
	if duration >= time.Minute {
		durationStr = fmt.Sprintf("%dm %ds", int(duration.Minutes()), int(duration.Seconds())%60)
	} else {
		durationStr = fmt.Sprintf("%.1fs", duration.Seconds())
	}

	costStr := fmt.Sprintf("$%.4f", cumulativeCost)

	var statusEmoji string
	if stats.IsError {
		statusEmoji = ":warning:"
	} else {
		statusEmoji = ":chart_with_upwards_trend:"
	}

	contextElements := []slack.MixedElement{
		slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("%s *Task Stats*", statusEmoji), false, false),
		slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("⏱️ %s (this turn)", durationStr), false, false),
		slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("🔄 %d turns total", cumulativeTurns), false, false),
		slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("💰 %s total", costStr), false, false),
	}

	blocks := []slack.Block{slack.NewContextBlock("", contextElements...)}

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

// maybeHandleMonitorResult registers a successful Monitor start and, at
// default verbosity, posts a one-liner instead of uploading the full
// boilerplate text ("Monitor started (task X, timeout Yms). You will be
// notified on each event. Keep working — do not poll…"). Returns true if
// the result was handled (caller should NOT fall through to snippet
// upload); false otherwise.
func (h *Handler) maybeHandleMonitorResult(
	channelID, threadTS, content string,
	input map[string]any,
	verbosityLevel int,
	logger zerolog.Logger,
) bool {
	m := monitorStartPattern.FindStringSubmatch(content)
	if len(m) < 2 {
		// Not a success message (likely tool_use_error). Fall through so
		// the user sees what went wrong in the normal snippet path.
		return false
	}
	taskID := m[1]
	count := h.bot.sessions.AddMonitor(channelID, threadTS, taskID)
	if err := h.bot.sessions.Save(); err != nil {
		logger.Debug().Err(err).Msg("save after AddMonitor")
	}
	h.syncMonitorCountEmoji(channelID, threadTS, logger)

	if verbosityLevel >= 1 {
		// Verbose mode: let the default path upload the full confirmation.
		return false
	}

	// Terse: describe the monitor in one line. If the caller supplied a
	// `description`, use it; else show the first line of the command.
	desc := ""
	if d, ok := input["description"].(string); ok {
		desc = d
	}
	if desc == "" {
		if cmd, ok := input["command"].(string); ok {
			desc = cmd
			if idx := strings.Index(desc, "\n"); idx != -1 {
				desc = desc[:idx] + "…"
			}
			if len(desc) > 80 {
				desc = desc[:77] + "…"
			}
		}
	}
	var summary string
	if desc != "" {
		summary = fmt.Sprintf(":satellite_antenna: Monitor started `%s` — %s (now %d active)", taskID, desc, count)
	} else {
		summary = fmt.Sprintf(":satellite_antenna: Monitor started `%s` (now %d active)", taskID, count)
	}
	if _, err := h.bot.PostMessage(channelID, summary, threadTS); err != nil {
		logger.Debug().Err(err).Msg("failed to post terse monitor start")
	}
	return true
}

// maybeHandleTaskStopResult parses the TaskStop JSON result and renders a
// tidy Slack message (emoji + task id + command in a code span) instead
// of uploading the raw JSON as a collapsible snippet. Also deregisters
// the monitor from the thread's active set so the count reaction stays
// accurate. Returns true if handled.
func (h *Handler) maybeHandleTaskStopResult(
	channelID, threadTS, content string,
	verbosityLevel int,
	logger zerolog.Logger,
) bool {
	var parsed struct {
		Message  string `json:"message"`
		TaskID   string `json:"task_id"`
		TaskType string `json:"task_type"`
		Command  string `json:"command"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil || parsed.TaskID == "" {
		// Not a success payload (e.g., <tool_use_error> saying the id
		// wasn't found). Let the default path handle it.
		return false
	}

	count := h.bot.sessions.RemoveMonitor(channelID, threadTS, parsed.TaskID)
	if err := h.bot.sessions.Save(); err != nil {
		logger.Debug().Err(err).Msg("save after RemoveMonitor")
	}
	h.syncMonitorCountEmoji(channelID, threadTS, logger)

	if verbosityLevel < 0 {
		// Silent mode: skip entirely.
		return true
	}

	cmd := parsed.Command
	// Keep the command displayed on one line so the Slack post stays
	// tidy; if it's truly long the full thing is still in the transcript.
	if idx := strings.Index(cmd, "\n"); idx != -1 {
		cmd = cmd[:idx] + "…"
	}
	if len(cmd) > 200 {
		cmd = cmd[:197] + "…"
	}

	var msg string
	if cmd != "" {
		msg = fmt.Sprintf(":octagonal_sign: Stopped monitor `%s` (%d active) — `%s`", parsed.TaskID, count, cmd)
	} else {
		msg = fmt.Sprintf(":octagonal_sign: Stopped monitor `%s` (%d active)", parsed.TaskID, count)
	}
	if _, err := h.bot.PostMessage(channelID, msg, threadTS); err != nil {
		logger.Debug().Err(err).Msg("failed to post TaskStop summary")
	}
	return true
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

	// Monitor and TaskStop emit structured results that we render
	// specially: Monitor's success text is verbose boilerplate best
	// hidden at default verbosity, and TaskStop returns JSON that's
	// nicer parsed than uploaded as a snippet. Both also adjust the
	// "active monitor count" reaction on the thread's anchor message.
	switch toolName {
	case "Monitor":
		if h.maybeHandleMonitorResult(channelID, threadTS, content, input, verbosityLevel, logger) {
			return
		}
	case "TaskStop":
		if h.maybeHandleTaskStopResult(channelID, threadTS, content, verbosityLevel, logger) {
			return
		}
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
