package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rs/zerolog"
)

// Version is the bot version. Update this when releasing.
const Version = "0.30.0"

type Flags struct {
	Log struct {
		Level  zerolog.Level `kong:"default='info',enum='trace,debug,info,warn,error,fatal,panic',env='CLOD_BOT_LOG_LEVEL'"`
		Format string        `kong:"default='json',enum='json,console',env='CLOD_BOT_LOG_FORMAT'"`
	} `kong:"embed,prefix='log.'"`

	SlackBotToken string `kong:"required,env='SLACK_BOT_TOKEN',help='Slack bot token (xoxb-...)'"`
	SlackAppToken string `kong:"required,env='SLACK_APP_TOKEN',help='Slack app token for Socket Mode (xapp-...)'"`

	AllowedUsers []string `kong:"env='CLOD_BOT_ALLOWED_USERS',sep=',',help='Comma-separated list of allowed Slack user IDs'"`

	SessionStorePath string `kong:"default='sessions.json',env='CLOD_BOT_SESSION_STORE_PATH',help='Path to session store JSON file'"`

	AgentsPath string `kong:"default='.',env='CLOD_BOT_AGENTS_PATH',help='Base path to search for agent directories'"`

	AgentsPromptPath string `kong:"default='README.md',env='CLOD_BOT_AGENTS_PROMPT_PATH',help='Path to per-task agent prompt file (relative to task dir or absolute). Empty disables.'"`

	AgentsSharedPromptPath string `kong:"default='AGENTS.md',env='CLOD_BOT_AGENTS_SHARED_PROMPT_PATH',help='Path to workspace-wide agent prompt file (relative to the agents base dir or absolute). Copied into every task alongside the per-task prompt. Empty disables.'"`

	ClodTimeout time.Duration `kong:"default='24h',env='CLOD_BOT_TIMEOUT',help='Timeout for clod execution'"`

	PermissionMode string `kong:"default='bypassPermissions',env='CLOD_BOT_PERMISSION_MODE',help='Claude permission mode (default, acceptEdits, plan, bypassPermissions). Defaults to bypassPermissions since clod runs claude inside an isolated docker container — matching the official recommendation for confined environments.'"`

	VerboseTools []string `kong:"default='Read,Glob,Grep,WebFetch,WebSearch,TodoWrite,Write,Edit,EnterPlanMode',env='CLOD_BOT_VERBOSE_TOOLS',sep=',',help='Tools affected by verbosity toggle'"`

	VerbosityLevel int `kong:"default='0',env='CLOD_BOT_VERBOSITY_LEVEL',help='Default verbosity level: -1 (silent), 0 (summary), 1 (full)'"`

	DefaultModel string `kong:"default='',env='CLOD_BOT_DEFAULT_MODEL',help='Default claude --model to use (e.g. opus, sonnet, claude-haiku-4-5). Empty defers to claude default.'"`

	GracefulShutdownTTL time.Duration `kong:"default='30s',env='CLOD_BOT_GRACEFUL_SHUTDOWN_TTL',help='Time to wait for graceful shutdown'"`

	ResumeStaleAfter time.Duration `kong:"default='30m',env='CLOD_BOT_RESUME_STALE_AFTER',help='Active sessions older than this are treated as stale on startup (flag cleared, no auto-resume). Set to 0 to disable auto-resume entirely.'"`
}

type CLI struct {
	Flags
}

func (cli *CLI) Run(ctx *context.Context, logger zerolog.Logger) (err error) {
	logger.Info().
		Str("version", Version).
		Str("agents_path", cli.AgentsPath).
		Str("session_store", cli.SessionStorePath).
		Int("allowed_users", len(cli.AllowedUsers)).
		Msg("starting clod slack bot")

	// Initialize components
	auth := NewAuthorizer(cli.AllowedUsers)

	tasks, err := NewTaskRegistry(cli.AgentsPath)
	if err != nil {
		return err
	}

	taskNames := tasks.List()
	logger.Info().Strs("tasks", taskNames).Msg("discovered tasks")

	sessions, err := NewSessionStore(cli.SessionStorePath, cli.VerbosityLevel, logger)
	if err != nil {
		return err
	}
	logger.Info().
		Int("session_count", sessions.Count()).
		Str("path", cli.SessionStorePath).
		Msg("loaded sessions from storage")

	// Resolve the shared prompt path relative to the agents base
	// dir when it isn't already absolute, so `AGENTS.md` (the
	// default) lands at `<AgentsPath>/AGENTS.md`.
	sharedPromptPath := cli.AgentsSharedPromptPath
	if sharedPromptPath != "" && !filepath.IsAbs(sharedPromptPath) && cli.AgentsPath != "" {
		sharedPromptPath = filepath.Join(cli.AgentsPath, sharedPromptPath)
	}
	runner := NewRunner(cli.ClodTimeout, cli.PermissionMode, cli.AgentsPromptPath, sharedPromptPath, logger)

	// Create and start the bot
	bot, err := NewBot(
		cli.SlackBotToken,
		cli.SlackAppToken,
		auth,
		tasks,
		sessions,
		runner,
		cli.VerboseTools,
		cli.VerbosityLevel,
		cli.DefaultModel,
		logger,
	)
	if err != nil {
		return err
	}

	// Run bot in background
	errors := make(chan error, 1)
	go func() {
		errors <- bot.Run(*ctx)
	}()

	// Resume any sessions that were mid-task when the previous bot
	// process exited. A zero staleness threshold disables the feature
	// entirely (explicit opt-out).
	if cli.ResumeStaleAfter > 0 {
		bot.ResumeActiveSessions(*ctx, cli.ResumeStaleAfter)
	}

	// Signal handling (buffer of 2 to catch second signal for force exit)
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	select {
	case <-signals:
		start := time.Now()
		logger.Warn().
			Float64("ttl", cli.GracefulShutdownTTL.Seconds()).
			Msg("shutting down gracefully (send again to force)")

		bot.Shutdown()

		select {
		case err = <-errors:
		case <-signals:
			logger.Warn().
				Float64("elapsed", time.Since(start).Seconds()).
				Msg("received second signal: forcing immediate exit")
			os.Exit(1)
		case <-time.After(cli.GracefulShutdownTTL):
			logger.Error().
				Float64("elapsed", time.Since(start).Seconds()).
				Msg("graceful shutdown timeout: forcing exit")
			os.Exit(1)
		}

		logger.Info().
			Float64("elapsed", time.Since(start).Seconds()).
			Msg("graceful shutdown complete")

	case err = <-errors:
		if err != nil {
			logger.Error().Err(err).Msg("bot error")
		}
	}

	// Save sessions before exit
	if saveErr := sessions.Save(); saveErr != nil {
		logger.Error().Err(saveErr).Msg("failed to save sessions")
		if err == nil {
			err = saveErr
		}
	}

	return err
}
