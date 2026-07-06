package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/rs/zerolog"
)

// Version is the bot version. Update this when releasing.
const Version = "0.36.3"

type Flags struct {
	Log struct {
		Level  zerolog.Level `kong:"default='info',enum='trace,debug,info,warn,error,fatal,panic',env='CLOD_BOT_LOG_LEVEL'"`
		Format string        `kong:"default='json',enum='json,console',env='CLOD_BOT_LOG_FORMAT'"`
	} `kong:"embed,prefix='log.'"`

	SlackBotToken string `kong:"required,env='SLACK_BOT_TOKEN',help='Slack bot token (xoxb-...)'"`
	SlackAppToken string `kong:"required,env='SLACK_APP_TOKEN',help='Slack app token for Socket Mode (xapp-...)'"`

	AllowedUsers []string `kong:"env='CLOD_BOT_ALLOWED_USERS',sep=',',help='Comma-separated list of allowed Slack user IDs'"`

	SessionStorePath string `kong:"default='sessions.json',env='CLOD_BOT_SESSION_STORE_PATH',help='Path to session store JSON file'"`

	WorkspacePath string `kong:"default='.',env='CLOD_BOT_WORKSPACE_PATH',help='Base path to search for domain directories. Each subdirectory is a domain of work; the workspace is the dir itself.'"`

	DomainReadme string `kong:"default='README.md',env='CLOD_BOT_DOMAIN_README',help='Path to per-domain README, relative to the domain dir or absolute. Inlined into the system prompt as domain-specific guidance. Empty disables.'"`

	WorkspaceReadme string `kong:"default='README.md',env='CLOD_BOT_WORKSPACE_README',help='Path to the workspace-root README, relative to the workspace dir (CLOD_BOT_WORKSPACE_PATH) or absolute. Inlined into the system prompt as workspace-wide guidance shared across every domain. Empty disables.'"`

	ClodTimeout time.Duration `kong:"default='24h',env='CLOD_BOT_TIMEOUT',help='Timeout for clod execution'"`

	PermissionMode string `kong:"default='bypassPermissions',env='CLOD_BOT_PERMISSION_MODE',help='Claude permission mode (default, acceptEdits, plan, bypassPermissions). Defaults to bypassPermissions since clod runs claude inside an isolated docker container — matching the official recommendation for confined environments.'"`

	VerboseTools []string `kong:"default='Read,Glob,Grep,WebFetch,WebSearch,TodoWrite,Write,Edit,EnterPlanMode',env='CLOD_BOT_VERBOSE_TOOLS',sep=',',help='Tools affected by verbosity toggle'"`

	VerbosityLevel int `kong:"default='0',env='CLOD_BOT_VERBOSITY_LEVEL',help='Default verbosity level: -1 (silent), 0 (summary), 1 (full)'"`

	DefaultModel string `kong:"default='',env='CLOD_BOT_DEFAULT_MODEL',help='Default claude --model to use (e.g. fable, opus, sonnet, claude-fable-5, claude-opus-4-8, claude-haiku-4-5). Empty defers to claude default.'"`

	GracefulShutdownTTL time.Duration `kong:"default='2m',env='CLOD_BOT_GRACEFUL_SHUTDOWN_TTL',help='Time to wait for graceful shutdown'"`

	ResumeStaleAfter time.Duration `kong:"default='30m',env='CLOD_BOT_RESUME_STALE_AFTER',help='Active sessions older than this are treated as stale on startup (flag cleared, no auto-resume). Set to 0 to disable auto-resume entirely.'"`
}

type CLI struct {
	Flags
}

func (cli *CLI) Run(ctx *context.Context, logger zerolog.Logger) (err error) {
	logger.Info().
		Str("version", Version).
		Str("workspace_path", cli.WorkspacePath).
		Str("session_store", cli.SessionStorePath).
		Int("allowed_users", len(cli.AllowedUsers)).
		Msg("starting clod slack bot")

	// Initialize components
	auth := NewAuthorizer(cli.AllowedUsers)

	domains, err := NewDomainRegistry(cli.WorkspacePath)
	if err != nil {
		return err
	}

	domainNames := domains.List()
	logger.Info().Strs("domains", domainNames).Msg("discovered domains")

	sessions, err := NewSessionStore(cli.SessionStorePath, cli.VerbosityLevel, logger)
	if err != nil {
		return err
	}
	logger.Info().
		Int("session_count", sessions.Count()).
		Str("path", cli.SessionStorePath).
		Msg("loaded sessions from storage")

	// SchedulingRegistry backs the cron_* / bg_* MCP shim served by
	// bot/schedbridge in each session container. Storage lives next to
	// sessions.json under a `crons.json` sibling (see mcp-shim.md §4.3).
	// Ticker starts inside NewSchedulingRegistry's Start so persisted
	// crons resume at boot.
	scheduling, err := NewSchedulingRegistry(deriveCronsPath(cli.SessionStorePath), logger)
	if err != nil {
		return err
	}
	scheduling.Start()
	logger.Info().
		Str("path", deriveCronsPath(cli.SessionStorePath)).
		Msg("scheduling registry loaded")

	// Resolve the workspace README path relative to the workspace
	// dir when it isn't already absolute, so the default
	// `README.md` lands at `<WorkspacePath>/README.md`.
	workspaceReadmePath := cli.WorkspaceReadme
	if workspaceReadmePath != "" && !filepath.IsAbs(workspaceReadmePath) && cli.WorkspacePath != "" {
		workspaceReadmePath = filepath.Join(cli.WorkspacePath, workspaceReadmePath)
	}
	runner := NewRunner(cli.ClodTimeout, cli.PermissionMode, cli.DomainReadme, workspaceReadmePath, logger)

	// Create and start the bot
	bot, err := NewBot(
		cli.SlackBotToken,
		cli.SlackAppToken,
		auth,
		domains,
		sessions,
		scheduling,
		runner,
		cli.VerboseTools,
		cli.VerbosityLevel,
		cli.DefaultModel,
		cli.GracefulShutdownTTL,
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

	// SIGUSR1 handler: dump every goroutine's stack to a
	// timestamped file under /tmp so a live wedge can be inspected
	// without killing the process. Handy for the recurring perm-loop
	// wedge (eerie-eagle 2026-06 to 2026-07) where the goroutine is
	// visibly alive but not selecting from its channel — we need
	// the stack to know where it's actually parked.
	//
	// Usage: kill -USR1 <bot pid>. Output goes to
	// /tmp/clod-bot-stacks-<UTC timestamp>.txt.
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	go func() {
		for range usr1 {
			ts := time.Now().UTC().Format("20060102T150405Z")
			path := fmt.Sprintf("/tmp/clod-bot-stacks-%s.txt", ts)
			// Grow the buffer until the full stack fits. Starts
			// at 1 MiB; goroutine dumps rarely exceed a few MiB
			// but caps at 64 MiB so a runaway can't OOM the box.
			bufSize := 1 << 20
			const maxBuf = 64 << 20
			var out []byte
			for {
				buf := make([]byte, bufSize)
				n := runtime.Stack(buf, true)
				if n < bufSize || bufSize >= maxBuf {
					out = buf[:n]
					break
				}
				bufSize *= 2
			}
			if err := os.WriteFile(path, out, 0o644); err != nil {
				logger.Error().Err(err).Str("path", path).Msg("SIGUSR1: failed to write goroutine stack dump")
				continue
			}
			logger.Warn().
				Str("path", path).
				Int("bytes", len(out)).
				Msg("SIGUSR1: wrote goroutine stack dump")
		}
	}()

	select {
	case <-signals:
		start := time.Now()
		logger.Warn().
			Float64("ttl", cli.GracefulShutdownTTL.Seconds()).
			Msg("shutting down gracefully (send again to force)")

		bot.Shutdown(*ctx)

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

	// Stop the scheduling engine so no more ticks fire mid-shutdown.
	// Note: this does NOT cancel per-session sockets; those are
	// tied to runClod lifecycle and torn down when the containers exit.
	// It only stops the cron engine goroutine itself.
	scheduling.Stop()

	// Save sessions before exit
	if saveErr := sessions.Save(); saveErr != nil {
		logger.Error().Err(saveErr).Msg("failed to save sessions")
		if err == nil {
			err = saveErr
		}
	}
	// Save scheduling registry too. UpdateFireResult saves per-tick
	// already, so this is a belt-and-suspenders capture of any
	// last-second Add/Delete that didn't survive to disk yet.
	if saveErr := scheduling.Save(); saveErr != nil {
		logger.Error().Err(saveErr).Msg("failed to save scheduling registry")
		if err == nil {
			err = saveErr
		}
	}

	return err
}
