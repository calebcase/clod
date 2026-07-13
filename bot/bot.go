package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/calebcase/oops"
	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// slackHTTPTimeout is the per-request deadline applied to every
// Slack JSON API call (chat.postMessage, chat.update, files.upload,
// etc.) via the shared *http.Client. Slack's p99 for these endpoints
// is comfortably under 5s; anything past 30s is a hung connection
// or a backend stall, not slow processing.
//
// Why this matters: the runClod main loop (handlers.go:3788+) does
// PostMessage / UpdateMessage inline from its task.Output() case
// body. Without a deadline a single hung HTTP call wedges the entire
// select — including the permRequests case — so AskUserQuestion
// prompts never reach Slack and the agent stalls. Observed in
// eerie-eagle on 2026-06-26: a flushBuffer post hung at 11:23, the
// agent's tool_use:AskUserQuestion that came 3 seconds later sat
// unprocessed for 16 minutes until manual FIFO injection.
//
// WebSocket connections are hijacked from net/http after upgrade and
// are NOT subject to this deadline, so Socket Mode stays connected
// indefinitely; only the JSON API hop inherits the limit.
const slackHTTPTimeout = 30 * time.Second

// Bot manages the Slack connection and event handling.
type Bot struct {
	client        *slack.Client
	socket        *socketmode.Client
	socketHandler *socketmode.SocketmodeHandler
	auth          *Authorizer
	domains       *DomainRegistry
	sessions      *SessionStore
	scheduling    *SchedulingRegistry
	runner        *Runner
	files         *FileHandler
	logger        zerolog.Logger
	handler       *Handler
	// gracefulShutdownTTL bounds how long any single graceful task
	// shutdown may take before the runner forces a kill. Used by
	// Bot.Shutdown for SIGTERM-driven stops, and by per-task
	// shutdowns (close command, model/effort restart) so all paths
	// share a single tunable. Configured via
	// CLOD_BOT_GRACEFUL_SHUTDOWN_TTL.
	gracefulShutdownTTL time.Duration
	// latestPostTS tracks the TS of the most-recent bot post per
	// (channel, thread). Updated by every helper that posts a new
	// message (PostMessage, PostMessageBlocks, file uploads); NOT
	// updated by UpdateMessage (edits don't change a message's
	// position in the thread). Consulted by the file sync watcher
	// to decide whether to edit its previous file-sync message in
	// place (still the latest) or post a new one (something else
	// was posted after).
	latestPostTS sync.Map // key "channel:thread" -> string messageTS

	// lastEventAt is the unix-nano timestamp of the most-recent
	// Slack event routed through the socketmode middleware
	// (excluding pings, which flow through slack-go internally and
	// don't reach our handlers). Used by the event-starvation
	// watchdog as a cheap first-pass indicator that something MIGHT
	// be wrong — a real starvation is then confirmed by polling
	// Slack directly (see runEventStarvationWatchdog) so a quiet
	// Sunday afternoon doesn't fire the warn every minute.
	lastEventAt atomic.Int64

	// lastMessageTSByThread tracks the highest inbound Slack
	// message TS we've observed on each session thread, keyed by
	// "channelID:threadTS". Updated when handleCallbackEvent
	// dispatches a MessageEvent or AppMentionEvent. Used by the
	// starvation watchdog to poll conversations.replies with
	// oldest=<this> and detect messages Slack has that we
	// haven't received via Socket Mode.
	lastMessageTSByThread sync.Map // key "channel:thread" -> string TS

	// permalinkCache memoizes chat.getPermalink lookups so the
	// Home-tab renderer doesn't re-hit the API on every publish.
	// Key: "channel:ts". Value: string url. Permalinks for a
	// given (channel, ts) are stable forever, so the cache is
	// append-only.
	permalinkCache sync.Map
}

// NewBot creates a new Bot instance.
func NewBot(
	botToken string,
	appToken string,
	auth *Authorizer,
	domains *DomainRegistry,
	sessions *SessionStore,
	scheduling *SchedulingRegistry,
	runner *Runner,
	verboseTools []string,
	verbosityLevel int,
	defaultModel string,
	gracefulShutdownTTL time.Duration,
	logger zerolog.Logger,
) (*Bot, error) {
	client := slack.New(
		botToken,
		slack.OptionAppLevelToken(appToken),
		slack.OptionHTTPClient(&http.Client{Timeout: slackHTTPTimeout}),
	)

	socket := socketmode.New(
		client,
		socketmode.OptionDebug(logger.GetLevel() <= zerolog.DebugLevel),
	)

	// Create the socketmode handler for registering event callbacks
	socketHandler := socketmode.NewSocketmodeHandler(socket)

	bot := &Bot{
		client:              client,
		socket:              socket,
		socketHandler:       socketHandler,
		auth:                auth,
		domains:             domains,
		sessions:            sessions,
		scheduling:          scheduling,
		runner:              runner,
		files:               NewFileHandler(client, logger),
		logger:              logger.With().Str("component", "bot").Logger(),
		gracefulShutdownTTL: gracefulShutdownTTL,
	}

	bot.handler = NewHandler(bot, verboseTools, verbosityLevel, defaultModel)
	bot.files.AttachBot(bot)

	// Register event handlers using the socketmode handler pattern
	bot.registerEventHandlers()

	return bot, nil
}

// Run starts the bot and processes events until the context is cancelled.
// ResumeActiveSessions asks the handler to revive any sessions left
// flagged Active from a previous run. Delegates to Handler so cli.go
// doesn't need a handle on internal handler state.
func (b *Bot) ResumeActiveSessions(ctx context.Context, maxAge time.Duration) {
	if b.handler == nil {
		return
	}
	b.handler.ResumeActiveSessions(ctx, maxAge)
}

// slackEventStarvationThreshold is how long we allow the event
// pipe to be silent (no non-ping events) before we start actively
// probing Slack for missed messages. 5 min is well past any
// normal quiet period.
const slackEventStarvationThreshold = 5 * time.Minute

// runEventStarvationWatchdog probes Slack directly to confirm real
// starvation rather than relying on a naked timeout. Every minute,
// if the local event pipe has been silent longer than
// slackEventStarvationThreshold, iterate the currently-running
// tasks and call conversations.replies on each session's thread
// with oldest = the last message TS we processed via Socket Mode.
// Any returned messages are ones Slack has but we don't — the
// smoking gun for socket-mode starvation.
//
// Design choices to keep noise low:
//  - Only probes while at least one task is actively running.
//    A truly idle bot has no expected events, so silence is
//    normal and shouldn't fire.
//  - Only warns when Slack itself confirms missed messages. Time
//    alone is not enough to justify a warn.
//  - Each thread warns at most once per starvation episode; the
//    warn is repeated only after a fresh event arrives (which
//    clears the lastWarnedTS memory).
//
// Runs bounded by ctx so it exits cleanly on Shutdown.
func (b *Bot) runEventStarvationWatchdog(ctx context.Context) {
	// Prime lastEventAt so a startup silence isn't reported.
	b.lastEventAt.Store(time.Now().UnixNano())
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	// lastWarnedForTS tracks the last TS we already warned about
	// per thread so we don't repeat the same warn every minute for
	// the same stuck message. Cleared implicitly when a newer
	// message arrives and updates lastMessageTSByThread.
	var lastWarnedForTS sync.Map // key "channel:thread" -> string TS

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			last := time.Unix(0, b.lastEventAt.Load())
			since := now.Sub(last)
			if since < slackEventStarvationThreshold {
				continue
			}
			// Only probe if the bot has any running task.
			// Idle bot → silence is expected → don't waste
			// Slack API calls confirming the obvious.
			if b.handler == nil {
				continue
			}
			activeCount := 0
			b.handler.runningTasks.Range(func(k, v any) bool {
				activeCount++
				return true
			})
			if activeCount == 0 {
				continue
			}
			b.probeMissedMessages(ctx, since, &lastWarnedForTS)
		}
	}
}

// probeMissedMessages walks the currently-running tasks and asks
// Slack directly via conversations.replies whether it has messages
// beyond the last TS we've processed. Any returned messages are
// proof of a socket-mode starvation and get logged at Warn.
//
// Errors from the Slack API are logged at Debug — the probe is
// diagnostic, so a transient failure isn't itself an alarm.
func (b *Bot) probeMissedMessages(ctx context.Context, silence time.Duration, lastWarnedForTS *sync.Map) {
	if b.client == nil {
		return
	}
	b.handler.runningTasks.Range(func(k, v any) bool {
		key, _ := k.(string)
		// key format is "channelID:threadTS"; split on the first
		// colon. threadTS itself doesn't contain a colon.
		colon := strings.Index(key, ":")
		if colon < 0 {
			return true
		}
		channelID := key[:colon]
		threadTS := key[colon+1:]

		lastTSAny, _ := b.lastMessageTSByThread.Load(key)
		lastTS, _ := lastTSAny.(string)
		if lastTS == "" {
			// No baseline to compare against — we haven't
			// observed any message on this thread yet.
			// Fall back to the thread's own TS so we can at
			// least see if there's ANY message we've missed.
			lastTS = threadTS
		}

		params := &slack.GetConversationRepliesParameters{
			ChannelID: channelID,
			Timestamp: threadTS,
			Oldest:    lastTS,
			Limit:     20,
		}
		msgs, _, _, err := b.client.GetConversationRepliesContext(ctx, params)
		if err != nil {
			b.logger.Debug().
				Err(err).
				Str("channel", channelID).
				Str("thread", threadTS).
				Msg("starvation probe: conversations.replies failed")
			return true
		}
		// Slack returns messages >= oldest inclusive AND
		// conversations.replies ALWAYS includes the thread parent
		// as the first item regardless of oldest. Filter to
		// strictly-newer TS so we don't false-positive on either.
		// Slack TS is "seconds.microseconds" as a fixed-width
		// string, so plain string > compares correctly.
		var missed []slack.Message
		for i := range msgs {
			if msgs[i].Timestamp > lastTS {
				missed = append(missed, msgs[i])
			}
		}
		if len(missed) == 0 {
			return true
		}
		// Highest TS in the missed set — used as the "already
		// warned for this" cursor so we don't repeat every
		// minute for a persistent starvation.
		highestMissed := missed[0].Timestamp
		for i := 1; i < len(missed); i++ {
			if missed[i].Timestamp > highestMissed {
				highestMissed = missed[i].Timestamp
			}
		}
		if prevAny, ok := lastWarnedForTS.Load(key); ok {
			if prev, _ := prevAny.(string); prev == highestMissed {
				return true
			}
		}
		lastWarnedForTS.Store(key, highestMissed)

		// Summarize the missed messages for the log — user_id
		// + text head so the operator can see what got stranded.
		summaries := make([]string, 0, len(missed))
		for _, m := range missed {
			text := m.Text
			if len(text) > 80 {
				text = text[:77] + "..."
			}
			summaries = append(summaries, fmt.Sprintf("ts=%s user=%s text=%q", m.Timestamp, m.User, text))
		}
		b.logger.Warn().
			Str("channel", channelID).
			Str("thread", threadTS).
			Dur("silence", silence).
			Str("last_seen_ts", lastTS).
			Int("missed_count", len(missed)).
			Strs("missed", summaries).
			Msg("Slack event pipe starvation CONFIRMED: Slack has messages we did not receive")
		return true
	})
}

func (b *Bot) Run(ctx context.Context) error {
	b.logger.Info().Msg("starting socket mode connection")

	// Event-starvation watchdog. Exits when ctx cancels.
	go b.runEventStarvationWatchdog(ctx)

	// Use the socketmode handler instead of manually reading from Events channel
	err := b.socketHandler.RunEventLoopContext(ctx)
	if err != nil && ctx.Err() == nil {
		return oops.Trace(err)
	}

	return nil
}

// Shutdown gracefully shuts down the bot. Drives every running task
// through the same save-state-then-force-kill pattern used for
// per-thread close/restart, capped by the configured
// gracefulShutdownTTL. Returns when all tasks have either exited
// gracefully or been forcefully killed; cli.go's outer
// time.After(GracefulShutdownTTL) bounds the whole shutdown so a
// single stuck task can't block process exit.
func (b *Bot) Shutdown(ctx context.Context) {
	b.logger.Info().Dur("grace_period", b.gracefulShutdownTTL).Msg("shutting down bot")
	if b.handler == nil {
		return
	}
	b.handler.ShutdownAllTasks(ctx, b.gracefulShutdownTTL)
}

// registerEventHandlers sets up all the socketmode handler callbacks.
func (b *Bot) registerEventHandlers() {
	// Handle Events API events (app_mention, message, etc.)
	b.socketHandler.Handle(socketmode.EventTypeEventsAPI, b.handleEventsAPIMiddleware)

	// Handle interactive events (button clicks, etc.)
	b.socketHandler.Handle(socketmode.EventTypeInteractive, b.handleInteractiveMiddleware)

	// Handle connection events
	b.socketHandler.Handle(socketmode.EventTypeConnecting, func(evt *socketmode.Event, client *socketmode.Client) {
		b.logger.Info().Msg("connecting to Slack...")
	})

	b.socketHandler.Handle(socketmode.EventTypeConnected, func(evt *socketmode.Event, client *socketmode.Client) {
		b.logger.Info().Msg("connected to Slack")
	})

	b.socketHandler.Handle(socketmode.EventTypeConnectionError, func(evt *socketmode.Event, client *socketmode.Client) {
		b.logger.Error().Msg("connection error")
	})

	b.socketHandler.Handle(socketmode.EventTypeHello, func(evt *socketmode.Event, client *socketmode.Client) {
		b.logger.Debug().Msg("received hello from Slack")
	})
}

// handleEventsAPIMiddleware is the socketmode handler for Events API events.
func (b *Bot) handleEventsAPIMiddleware(evt *socketmode.Event, client *socketmode.Client) {
	b.lastEventAt.Store(time.Now().UnixNano())
	fmt.Printf(">>> EVENTS API: %+v\n", evt.Type)

	eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		b.logger.Warn().
			Interface("data", evt.Data).
			Msg("failed to cast EventsAPI event")
		return
	}

	client.Ack(*evt.Request)
	b.handleEventsAPIEvent(context.Background(), eventsAPIEvent)
}

// handleInteractiveMiddleware is the socketmode handler for interactive events.
func (b *Bot) handleInteractiveMiddleware(evt *socketmode.Event, client *socketmode.Client) {
	b.lastEventAt.Store(time.Now().UnixNano())
	fmt.Printf(">>> INTERACTIVE EVENT: %+v\n", evt.Type)
	b.logger.Info().Msg("received interactive event via socketmode handler")

	callback, ok := evt.Data.(slack.InteractionCallback)
	if !ok {
		b.logger.Warn().
			Interface("data", evt.Data).
			Msg("failed to cast interactive callback")
		return
	}

	client.Ack(*evt.Request)
	b.handleInteractiveCallback(context.Background(), callback)
}

// handleEventsAPIEvent processes Events API events.
func (b *Bot) handleEventsAPIEvent(ctx context.Context, evt slackevents.EventsAPIEvent) {
	b.logger.Debug().
		Str("type", evt.Type).
		Str("inner_type", evt.InnerEvent.Type).
		Msg("handling Events API event")

	switch evt.Type {
	case slackevents.CallbackEvent:
		b.handleCallbackEvent(ctx, evt.InnerEvent)
	default:
		b.logger.Debug().
			Str("type", evt.Type).
			Msg("unhandled Events API event type")
	}
}

// handleCallbackEvent processes callback events.
func (b *Bot) handleCallbackEvent(ctx context.Context, innerEvent slackevents.EventsAPIInnerEvent) {
	switch ev := innerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		b.bumpLastMessageTS(ev.Channel, ev.ThreadTimeStamp, ev.TimeStamp)
		b.handler.HandleAppMention(ctx, ev)
	case *slackevents.MessageEvent:
		b.bumpLastMessageTS(ev.Channel, ev.ThreadTimeStamp, ev.TimeStamp)
		b.handler.HandleMessage(ctx, ev)
	case *slackevents.ReactionAddedEvent:
		b.handler.HandleReactionAdded(ctx, ev)
	case *slackevents.ReactionRemovedEvent:
		b.handler.HandleReactionRemoved(ctx, ev)
	case *slackevents.AppHomeOpenedEvent:
		b.handler.HandleAppHomeOpened(ctx, ev)
	default:
		b.logger.Debug().
			Str("type", innerEvent.Type).
			Msg("unhandled callback event type")
	}
}

// bumpLastMessageTS records the TS of a Slack message the bot
// received via Socket Mode, keyed by the thread it belongs to. The
// starvation watchdog polls conversations.replies with
// oldest=<this> to confirm Slack has no additional messages we
// haven't seen. threadTS falls back to messageTS for top-level
// messages (Slack's convention for treating a lone message as its
// own thread parent).
func (b *Bot) bumpLastMessageTS(channelID, threadTS, messageTS string) {
	if channelID == "" || messageTS == "" {
		return
	}
	if threadTS == "" {
		threadTS = messageTS
	}
	k := channelID + ":" + threadTS
	// Store the max — Slack event delivery is generally ordered
	// but a defensive max avoids regressing on out-of-order events.
	if prev, ok := b.lastMessageTSByThread.Load(k); ok {
		if s, _ := prev.(string); slackTSGreater(s, messageTS) {
			return
		}
	}
	b.lastMessageTSByThread.Store(k, messageTS)
}

// slackTSGreater returns true if a > b as Slack timestamps.
// Slack TS is "seconds.microseconds" as a string; string
// comparison works because both halves are fixed-width.
func slackTSGreater(a, b string) bool {
	return a > b
}

// slackPostRetryBackoffs is the retry schedule for transient Slack
// API failures on PostMessage / UpdateMessage. Total wall clock
// ~3.5s across 3 retries. Slack's server side handles rate limits
// with 429 + Retry-After; slack-go's client honors that
// transparently, so this outer retry is for network blips and 5xx
// only. Kept small enough that a real outage doesn't stall the
// output loop for long. The 2026-07-13 "response got lost after
// restart" bug traced to handlers.go swallowing PostMessage errors
// at Debug — with retries here the transient failures self-heal
// before the caller sees an error at all.
var slackPostRetryBackoffs = []time.Duration{
	500 * time.Millisecond,
	1500 * time.Millisecond,
	3000 * time.Millisecond,
}

// PostMessage sends a message to a channel with retry-on-transient
// -failure. Bot.PostMessage is the single fan-in for claude → Slack
// traffic; every call is logged (Info on success, Warn on retry,
// Error on final failure) so a "response got lost" investigation
// can grep one file. Preview capped at 80 chars keeps the log
// readable without dumping full model output.
func (b *Bot) PostMessage(channelID, text string, threadTS string) (string, error) {
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	preview := text
	if len(preview) > 80 {
		preview = preview[:80] + "…"
	}
	start := time.Now()
	var ts string
	var err error
	for attempt := 0; attempt <= len(slackPostRetryBackoffs); attempt++ {
		if attempt > 0 {
			time.Sleep(slackPostRetryBackoffs[attempt-1])
		}
		_, ts, err = b.client.PostMessage(channelID, opts...)
		if err == nil {
			break
		}
		if attempt < len(slackPostRetryBackoffs) {
			b.logger.Warn().
				Err(err).
				Int("attempt", attempt+1).
				Str("channel", channelID).
				Str("thread", threadTS).
				Int("bytes", len(text)).
				Str("preview", preview).
				Msg("PostMessage failed, retrying")
		}
	}
	elapsed := time.Since(start)
	if err != nil {
		b.logger.Error().
			Err(err).
			Str("channel", channelID).
			Str("thread", threadTS).
			Int("bytes", len(text)).
			Str("preview", preview).
			Dur("elapsed", elapsed).
			Msg("PostMessage failed after all retries; message lost")
		return "", oops.Trace(err)
	}
	b.logger.Info().
		Str("channel", channelID).
		Str("thread", threadTS).
		Str("ts", ts).
		Int("bytes", len(text)).
		Str("preview", preview).
		Dur("elapsed", elapsed).
		Msg("PostMessage ok")
	b.recordPost(channelID, threadTS, ts)
	return ts, nil
}

// recordPost stores ts as the latest bot-originated post for
// (channel, thread). The file sync watcher reads this back to decide
// whether its previous sync message is still "at the end" of the
// thread (edit-eligible) or was superseded (post-new-required). A
// zero thread argument is normalized to the root-post ts so top-
// level posts and their thread replies share the same bucket.
//
// Also mirrored onto the persisted SessionMapping so the Home tab's
// `[latest →]` link survives a bot restart. Without persistence,
// idle/closed sessions (which never post again to repopulate the
// in-memory map) lose their jump link forever after a restart.
func (b *Bot) recordPost(channelID, threadTS, messageTS string) {
	if messageTS == "" {
		return
	}
	k := channelID + ":" + threadTS
	normalizedThreadTS := threadTS
	if threadTS == "" {
		k = channelID + ":" + messageTS
		normalizedThreadTS = messageTS
	}
	b.latestPostTS.Store(k, messageTS)
	if b.sessions != nil {
		b.sessions.SetLatestPostTS(channelID, normalizedThreadTS, messageTS)
	}
}

// LatestPostTS returns the TS of the most-recent post tracked for
// (channel, thread). Prefers the in-memory sync.Map (hot path used
// by the file sync watcher on every message) and falls back to the
// persisted SessionMapping.LatestPostTS when the map is cold — the
// map empties on every bot restart, so without the fallback every
// idle/closed session's `[latest →]` link would vanish on the next
// bounce. Empty string if the bot has posted nothing in this bucket
// and no persisted value exists either.
func (b *Bot) LatestPostTS(channelID, threadTS string) string {
	v, _ := b.latestPostTS.Load(channelID + ":" + threadTS)
	if s, _ := v.(string); s != "" {
		return s
	}
	if b.sessions != nil {
		return b.sessions.LatestPostTS(channelID, threadTS)
	}
	return ""
}

// LatestPermalinkFor returns a clickable Slack permalink to the
// most-recent bot post the bot is aware of for (channel, thread).
// Combines LatestPostTS + PermalinkFor in one call so the Home tab
// renderer can ask "where's the latest activity here?" without
// plumbing both lookups separately. Returns empty string when no
// post has been tracked yet (older sessions predating the tracker,
// freshly-resumed sessions before the first new post).
func (b *Bot) LatestPermalinkFor(channelID, threadTS string) string {
	ts := b.LatestPostTS(channelID, threadTS)
	if ts == "" {
		return ""
	}
	return b.PermalinkFor(channelID, ts)
}

// PermalinkFor returns a clickable Slack permalink for a specific
// message, memoized in-process so repeated Home-tab renders don't
// repeatedly hit chat.getPermalink. Permalinks for a given
// (channel, ts) never change, so the cache is append-only. Returns
// empty string on any failure — callers render a plain label in
// that case.
func (b *Bot) PermalinkFor(channelID, messageTS string) string {
	if channelID == "" || messageTS == "" {
		return ""
	}
	k := channelID + ":" + messageTS
	if v, ok := b.permalinkCache.Load(k); ok {
		s, _ := v.(string)
		return s
	}
	url, err := b.client.GetPermalink(&slack.PermalinkParameters{
		Channel: channelID,
		Ts:      messageTS,
	})
	if err != nil {
		b.logger.Debug().Err(err).Str("channel", channelID).Str("ts", messageTS).Msg("failed to fetch permalink")
		return ""
	}
	b.permalinkCache.Store(k, url)
	return url
}

// UpdateMessage updates an existing message with the same retry
// policy as PostMessage.
func (b *Bot) UpdateMessage(channelID, ts, text string) error {
	preview := text
	if len(preview) > 80 {
		preview = preview[:80] + "…"
	}
	start := time.Now()
	var err error
	for attempt := 0; attempt <= len(slackPostRetryBackoffs); attempt++ {
		if attempt > 0 {
			time.Sleep(slackPostRetryBackoffs[attempt-1])
		}
		_, _, _, err = b.client.UpdateMessage(channelID, ts, slack.MsgOptionText(text, false))
		if err == nil {
			break
		}
		if attempt < len(slackPostRetryBackoffs) {
			b.logger.Warn().
				Err(err).
				Int("attempt", attempt+1).
				Str("channel", channelID).
				Str("ts", ts).
				Int("bytes", len(text)).
				Str("preview", preview).
				Msg("UpdateMessage failed, retrying")
		}
	}
	elapsed := time.Since(start)
	if err != nil {
		b.logger.Error().
			Err(err).
			Str("channel", channelID).
			Str("ts", ts).
			Int("bytes", len(text)).
			Str("preview", preview).
			Dur("elapsed", elapsed).
			Msg("UpdateMessage failed after all retries; edit lost")
		return oops.Trace(err)
	}
	b.logger.Info().
		Str("channel", channelID).
		Str("ts", ts).
		Int("bytes", len(text)).
		Str("preview", preview).
		Dur("elapsed", elapsed).
		Msg("UpdateMessage ok")
	return nil
}

// DeleteMessage deletes a message the bot posted. Used by the
// output-file consolidator when swapping a prior fileshare bundle
// for a fresh one containing more files.
func (b *Bot) DeleteMessage(channelID, ts string) error {
	_, _, err := b.client.DeleteMessage(channelID, ts)
	if err != nil {
		return oops.Trace(err)
	}
	return nil
}

// UpdateMessageBlocks updates an existing message with blocks.
func (b *Bot) UpdateMessageBlocks(channelID, ts string, blocks []slack.Block) error {
	_, _, _, err := b.client.UpdateMessage(
		channelID,
		ts,
		slack.MsgOptionBlocks(blocks...),
	)
	if err != nil {
		return oops.Trace(err)
	}
	return nil
}

// botUserIDOnce caches the result of auth.test so we don't hit the
// Slack API on every reaction-filter lookup. The user id is stable
// for the lifetime of the bot token; refreshing requires a restart
// anyway. UserID() resolves it lazily.
var (
	botUserIDOnce sync.Once
	botUserID     string
	botUserIDErr  error
)

// UserID returns the bot's Slack user id (the "U…" string a reaction
// payload's `users` array contains for bot-added reactions). Cached
// after first successful call; subsequent calls are free.
func (b *Bot) UserID() (string, error) {
	botUserIDOnce.Do(func() {
		resp, err := b.client.AuthTest()
		if err != nil {
			botUserIDErr = oops.Trace(err)
			return
		}
		botUserID = resp.UserID
	})
	return botUserID, botUserIDErr
}

// AddReaction adds an emoji reaction to a message. name is without colons
// (e.g. "musical_score", not ":musical_score:"). Returns nil if the
// reaction already exists ("already_reacted"), since that's the desired
// end state.
func (b *Bot) AddReaction(channelID, messageTS, name string) error {
	err := b.client.AddReaction(name, slack.ItemRef{
		Channel:   channelID,
		Timestamp: messageTS,
	})
	if err == nil {
		return nil
	}
	// Idempotent: ignore "already_reacted".
	if err.Error() == "already_reacted" {
		return nil
	}
	return oops.Trace(err)
}

// RemoveReaction removes an emoji reaction from a message. Idempotent: a
// missing reaction ("no_reaction") is not an error.
func (b *Bot) RemoveReaction(channelID, messageTS, name string) error {
	err := b.client.RemoveReaction(name, slack.ItemRef{
		Channel:   channelID,
		Timestamp: messageTS,
	})
	if err == nil {
		return nil
	}
	if err.Error() == "no_reaction" {
		return nil
	}
	return oops.Trace(err)
}

// PostMessageBlocks sends a message with blocks to a channel.
func (b *Bot) PostMessageBlocks(channelID string, blocks []slack.Block, threadTS string) (string, error) {
	return b.PostMessageBlocksContext(context.Background(), channelID, blocks, threadTS)
}

// PostMessageBlocksContext is the context-aware variant. Use this from
// hot loops (like the permission-request handler in runClod) where a
// hung Slack call would otherwise wedge the entire loop and stop the
// bot from servicing other events on the thread. Pass a deadline-
// bounded context so transient Slack issues don't wedge the bot.
func (b *Bot) PostMessageBlocksContext(ctx context.Context, channelID string, blocks []slack.Block, threadTS string) (string, error) {
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(blocks...),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}

	_, ts, err := b.client.PostMessageContext(ctx, channelID, opts...)
	if err != nil {
		return "", oops.Trace(err)
	}
	b.recordPost(channelID, threadTS, ts)
	return ts, nil
}

// handleInteractiveCallback processes interactive component callbacks (button clicks, etc).
func (b *Bot) handleInteractiveCallback(ctx context.Context, callback slack.InteractionCallback) {
	b.logger.Info().
		Str("type", string(callback.Type)).
		Str("callback_id", callback.CallbackID).
		Int("num_actions", len(callback.ActionCallback.BlockActions)).
		Str("channel_id", callback.Channel.ID).
		Str("user_id", callback.User.ID).
		Msg("handling interactive callback")

	switch callback.Type {
	case slack.InteractionTypeBlockActions:
		if len(callback.ActionCallback.BlockActions) == 0 {
			b.logger.Warn().Msg("no block actions found in callback")
			return
		}
		for _, action := range callback.ActionCallback.BlockActions {
			b.logger.Info().
				Str("action_id", action.ActionID).
				Str("value", action.Value).
				Msg("processing block action")
			b.handler.HandleBlockAction(ctx, &callback, action)
		}
	case slack.InteractionTypeViewSubmission:
		b.logger.Info().
			Str("view_callback_id", callback.View.CallbackID).
			Msg("processing view submission")
		b.handler.HandleViewSubmission(ctx, &callback)
	default:
		b.logger.Debug().
			Str("type", string(callback.Type)).
			Msg("unhandled interactive callback type")
	}
}

// savePermissionRule saves a permission pattern to the task's claude.json file.
// This allows the permission to be remembered for future requests.
// It saves to both allowedTools (for bot reading) and permissions.allow (for Claude).
func (b *Bot) savePermissionRule(taskPath, pattern string) error {
	configPath := filepath.Join(taskPath, ".clod", "claude", "claude.json")

	b.logger.Info().
		Str("task_path", taskPath).
		Str("config_path", configPath).
		Str("pattern", pattern).
		Msg("saving permission rule")

	// Read existing config
	data, err := os.ReadFile(configPath)
	if err != nil {
		return oops.Trace(err)
	}

	// Parse as generic JSON to preserve all fields
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return oops.Trace(err)
	}

	// Get or create projects map
	projects, ok := config["projects"].(map[string]any)
	if !ok {
		projects = make(map[string]any)
		config["projects"] = projects
	}

	// Log existing project keys
	var projectKeys []string
	for k := range projects {
		projectKeys = append(projectKeys, k)
	}
	b.logger.Info().
		Strs("existing_project_keys", projectKeys).
		Msg("existing projects in claude.json")

	// Get or create project entry for this task path
	project, ok := projects[taskPath].(map[string]any)
	if !ok {
		project = map[string]any{}
		projects[taskPath] = project
	}

	// Get or create allowedTools array (for bot reading)
	allowedTools, ok := project["allowedTools"].([]any)
	if !ok {
		allowedTools = []any{}
	}

	// Get or create permissions.allow array (for Claude reading)
	permissions, ok := project["permissions"].(map[string]any)
	if !ok {
		permissions = map[string]any{}
		project["permissions"] = permissions
	}
	allowRules, ok := permissions["allow"].([]any)
	if !ok {
		allowRules = []any{}
	}

	// Check if pattern already exists in either array
	for _, t := range allowedTools {
		if t == pattern {
			b.logger.Debug().
				Str("pattern", pattern).
				Msg("permission pattern already exists in allowedTools, skipping")
			return nil
		}
	}

	// Add the new pattern to both arrays
	allowedTools = append(allowedTools, pattern)
	allowRules = append(allowRules, pattern)
	project["allowedTools"] = allowedTools
	permissions["allow"] = allowRules

	// Write back to file with nice formatting
	newData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return oops.Trace(err)
	}

	if err := os.WriteFile(configPath, newData, 0644); err != nil {
		return oops.Trace(err)
	}

	b.logger.Info().
		Str("pattern", pattern).
		Str("config_path", configPath).
		Str("task_path", taskPath).
		Msg("saved permission rule to claude.json")

	return nil
}
