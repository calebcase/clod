package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
)

type Flags struct {
	Log struct {
		Level  zerolog.Level `kong:"default='info',enum='trace,debug,info,warn,error,fatal,panic',env='LOG_LEVEL'"`
		Format string        `kong:"default='json',enum='json,console',env='LOG_FORMAT'"`
	} `kong:"embed,prefix='log.'"`

	SlackBotToken string `kong:"required,env='SLACK_BOT_TOKEN',help='Slack bot token (xoxb-...)'"`
	SlackAppToken string `kong:"required,env='SLACK_APP_TOKEN',help='Slack app token for Socket Mode (xapp-...)'"`

	AllowedUsers []string `kong:"env='ALLOWED_USERS',sep=',',help='Comma-separated list of allowed Slack user IDs'"`

	SessionStorePath string `kong:"default='sessions.json',env='SESSION_STORE_PATH',help='Path to session store JSON file'"`

	AgentsPath string `kong:"default='.',env='AGENTS_PATH',help='Base path to search for agent directories'"`

	ClodTimeout time.Duration `kong:"default='30m',env='CLOD_TIMEOUT',help='Timeout for clod execution'"`

	PermissionMode string `kong:"default='default',env='PERMISSION_MODE',help='Claude permission mode (default, acceptEdits, bypassPermissions)'"`

	VerboseTools []string `kong:"default='Read,Glob,Grep,WebFetch,WebSearch',env='VERBOSE_TOOLS',sep=',',help='Tools affected by verbosity toggle'"`

	GracefulShutdownTTL time.Duration `kong:"default='30s',env='GRACEFUL_SHUTDOWN_TTL',help='Time to wait for graceful shutdown'"`
}

type CLI struct {
	Flags
}

func (cli *CLI) Run(ctx *context.Context, logger zerolog.Logger) (err error) {
	logger.Info().
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

	sessions, err := NewSessionStore(cli.SessionStorePath)
	if err != nil {
		return err
	}
	logger.Info().
		Int("session_count", sessions.Count()).
		Str("path", cli.SessionStorePath).
		Msg("loaded sessions from storage")

	runner := NewRunner(cli.ClodTimeout, cli.PermissionMode, logger)

	// Create and start the bot
	bot, err := NewBot(
		cli.SlackBotToken,
		cli.SlackAppToken,
		auth,
		tasks,
		sessions,
		runner,
		cli.VerboseTools,
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
