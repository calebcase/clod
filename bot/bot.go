package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/calebcase/oops"
	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// Bot manages the Slack connection and event handling.
type Bot struct {
	client        *slack.Client
	socket        *socketmode.Client
	socketHandler *socketmode.SocketmodeHandler
	auth          *Authorizer
	tasks         *TaskRegistry
	sessions      *SessionStore
	runner        *Runner
	files         *FileHandler
	logger        zerolog.Logger
	handler       *Handler
}

// NewBot creates a new Bot instance.
func NewBot(
	botToken string,
	appToken string,
	auth *Authorizer,
	tasks *TaskRegistry,
	sessions *SessionStore,
	runner *Runner,
	verboseTools []string,
	verbosityLevel int,
	defaultModel string,
	logger zerolog.Logger,
) (*Bot, error) {
	client := slack.New(
		botToken,
		slack.OptionAppLevelToken(appToken),
	)

	socket := socketmode.New(
		client,
		socketmode.OptionDebug(logger.GetLevel() <= zerolog.DebugLevel),
	)

	// Create the socketmode handler for registering event callbacks
	socketHandler := socketmode.NewSocketmodeHandler(socket)

	bot := &Bot{
		client:        client,
		socket:        socket,
		socketHandler: socketHandler,
		auth:          auth,
		tasks:         tasks,
		sessions:      sessions,
		runner:        runner,
		files:         NewFileHandler(client, logger),
		logger:        logger.With().Str("component", "bot").Logger(),
	}

	bot.handler = NewHandler(bot, verboseTools, verbosityLevel, defaultModel)

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

func (b *Bot) Run(ctx context.Context) error {
	b.logger.Info().Msg("starting socket mode connection")

	// Use the socketmode handler instead of manually reading from Events channel
	err := b.socketHandler.RunEventLoopContext(ctx)
	if err != nil && ctx.Err() == nil {
		return oops.Trace(err)
	}

	return nil
}

// Shutdown gracefully shuts down the bot.
func (b *Bot) Shutdown() {
	b.logger.Info().Msg("shutting down bot")
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
		b.handler.HandleAppMention(ctx, ev)
	case *slackevents.MessageEvent:
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

// PostMessage sends a message to a channel.
func (b *Bot) PostMessage(channelID, text string, threadTS string) (string, error) {
	opts := []slack.MsgOption{
		slack.MsgOptionText(text, false),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}

	_, ts, err := b.client.PostMessage(channelID, opts...)
	if err != nil {
		return "", oops.Trace(err)
	}
	return ts, nil
}

// UpdateMessage updates an existing message.
func (b *Bot) UpdateMessage(channelID, ts, text string) error {
	_, _, _, err := b.client.UpdateMessage(
		channelID,
		ts,
		slack.MsgOptionText(text, false),
	)
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
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(blocks...),
	}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}

	_, ts, err := b.client.PostMessage(channelID, opts...)
	if err != nil {
		return "", oops.Trace(err)
	}
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
