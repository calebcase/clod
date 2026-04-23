package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// homeTabMaxSessions caps how many per-session rows we render in each
// section. Slack limits a view to 100 blocks total; with a header /
// divider / summary per section that leaves plenty of headroom, but
// keeping the list short also keeps the tab scannable.
const homeTabMaxSessions = 10

// buildHomeTabView assembles the Block Kit view the app's Home tab
// will show for `userID`. The personal section always appears;
// the workspace-wide section is appended only when
// `includeWorkspace` is true (i.e. the user is on the bot-wide
// allowlist).
func buildHomeTabView(
	sessions []*SessionMapping,
	userID string,
	includeWorkspace bool,
	botVersion string,
) slack.HomeTabViewRequest {
	now := time.Now()

	var blocks []slack.Block

	// Header + refresh button in a single row. Action is
	// `home_refresh`; the value field is unused (handler reads the
	// caller's user id straight from the callback).
	refreshBtn := slack.NewButtonBlockElement(
		"home_refresh",
		"refresh",
		slack.NewTextBlockObject("plain_text", "Refresh", false, false),
	)
	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject(
			"mrkdwn",
			fmt.Sprintf(":wave: *Clod usage* — hi <@%s>", userID),
			false, false,
		),
		nil,
		slack.NewAccessory(refreshBtn),
	))

	// Personal section.
	mine := filterByUser(sessions, userID)
	blocks = append(blocks, slack.NewDividerBlock())
	blocks = append(blocks, buildUsageHeader("Your sessions", mine))
	blocks = append(blocks, buildSessionRows(mine, now, false)...)

	if includeWorkspace {
		blocks = append(blocks, slack.NewDividerBlock())
		blocks = append(blocks, buildUsageHeader("Workspace", sessions))
		blocks = append(blocks, buildSessionRows(sessions, now, true)...)
	}

	// "How to use" reference.
	blocks = append(blocks, slack.NewDividerBlock())
	blocks = append(blocks, buildHomeHelpBlocks()...)

	// Footer.
	blocks = append(blocks, slack.NewDividerBlock())
	blocks = append(blocks, slack.NewContextBlock(
		"home_footer",
		slack.NewTextBlockObject(
			"mrkdwn",
			fmt.Sprintf("_Clod Bot v%s · refreshed %s_",
				botVersion, now.UTC().Format("2006-01-02 15:04 UTC")),
			false, false,
		),
	))

	return slack.HomeTabViewRequest{
		Type:   slack.VTHomeTab,
		Blocks: slack.Blocks{BlockSet: blocks},
	}
}

// buildUsageHeader renders the two-line header for a section: a
// bold title plus a one-liner with aggregate counts across the
// given sessions.
func buildUsageHeader(title string, sessions []*SessionMapping) slack.Block {
	totalTurns := 0
	totalCost := 0.0
	active := 0
	users := make(map[string]struct{})
	for _, s := range sessions {
		totalTurns += s.CumulativeTurns
		totalCost += s.CumulativeCostUSD
		if s.Active {
			active++
		}
		if s.UserID != "" {
			users[s.UserID] = struct{}{}
		}
	}
	line := fmt.Sprintf(
		"%d sessions (%d active) · %d turns · $%.2f",
		len(sessions), active, totalTurns, totalCost,
	)
	if len(users) > 1 {
		line = fmt.Sprintf("%d users · ", len(users)) + line
	}
	return slack.NewSectionBlock(
		slack.NewTextBlockObject(
			"mrkdwn",
			fmt.Sprintf("*%s*\n%s", title, line),
			false, false,
		),
		nil, nil,
	)
}

// buildSessionRows returns up to homeTabMaxSessions one-section-per
// -session blocks, most-recently-updated first. When `showUser` is
// true each row prefixes the owner with <@UID>, for the workspace
// section where rows span users.
func buildSessionRows(sessions []*SessionMapping, now time.Time, showUser bool) []slack.Block {
	sorted := append([]*SessionMapping(nil), sessions...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].UpdatedAt.After(sorted[j].UpdatedAt)
	})
	if len(sorted) > homeTabMaxSessions {
		sorted = sorted[:homeTabMaxSessions]
	}
	if len(sorted) == 0 {
		return []slack.Block{
			slack.NewContextBlock(
				"",
				slack.NewTextBlockObject("mrkdwn", "_No sessions yet._", false, false),
			),
		}
	}
	rows := make([]slack.Block, 0, len(sorted))
	for _, s := range sorted {
		rows = append(rows, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", formatSessionLine(s, now, showUser), false, false),
			nil, nil,
		))
	}
	return rows
}

// formatSessionLine renders the mrkdwn text for a single session row.
func formatSessionLine(s *SessionMapping, now time.Time, showUser bool) string {
	status := ":white_circle: idle"
	if s.Active {
		status = ":large_green_circle: active"
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("*`%s`*", s.TaskName))
	parts = append(parts, status)
	if showUser && s.UserID != "" {
		parts = append(parts, fmt.Sprintf("by <@%s>", s.UserID))
	}
	parts = append(parts, fmt.Sprintf("%d turns", s.CumulativeTurns))
	parts = append(parts, fmt.Sprintf("$%.2f", s.CumulativeCostUSD))
	parts = append(parts, fmt.Sprintf("updated %s", humanizeDuration(now.Sub(s.UpdatedAt))))
	return strings.Join(parts, " · ")
}

// humanizeDuration renders a time.Duration as a short human-readable
// string, e.g. "3m ago", "2h ago", "4d ago". Zero and negative
// durations render as "just now".
func humanizeDuration(d time.Duration) string {
	if d <= 30*time.Second {
		return "just now"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/24/30))
	}
}

// filterByUser returns only those sessions owned by userID.
func filterByUser(sessions []*SessionMapping, userID string) []*SessionMapping {
	out := make([]*SessionMapping, 0, len(sessions))
	for _, s := range sessions {
		if s.UserID == userID {
			out = append(out, s)
		}
	}
	return out
}

// buildHomeHelpBlocks returns the "How to use" reference rendered as
// a sequence of section blocks. Split into multiple sections so each
// stays under Slack's 3000-char per-block cap and so the renderer can
// reflow them individually as the catalog grows. Mrkdwn formatting
// (bold headers, code spans, bullet lists) renders the same way as
// thread messages elsewhere in the bot.
func buildHomeHelpBlocks() []slack.Block {
	starting := "*Starting tasks*\n" +
		"• `@bot <task>: <instructions>` — run an existing task; opens an init dialog when the task dir doesn't have `.clod/` yet\n" +
		"• `@bot <template>:: <instructions>` — auto-named task using `<template>` as the starting point (no dialog)\n" +
		"• `@bot :: <instructions>` — auto-named task; pick a template or Custom setup in the two-step init dialog\n" +
		"• `@bot *: <instructions>` — run in the agents base dir itself (no per-task subdirectory). Filesync and plan mode default off.\n" +
		"• `@bot !: <instructions>` — run claude directly on the host (no docker sandbox; confirmation required)"

	perThread := "*Per-thread commands* (any active session)\n" +
		"• `@bot close` — stop the running task and close the session. Auto-resume on bot restart is disabled until you @-mention again.\n" +
		"• `@bot allow @user` / `@bot disallow @user` — manage who else can drive this thread\n" +
		"• `@bot set model=opus|sonnet|haiku` — switch model. `+` / `-` to cycle, or send 🎼 / 📜 / 🌸\n" +
		"• `@bot set verbosity=0|1|-1` — silent / summary / full. Or 🙈 / 💬\n" +
		"• `@bot set plan=on|off` — toggle plan mode. Or `+` / `-` / 💭\n" +
		"• `@bot set filesync=on|off` — toggle file syncing for the task dir back to Slack"

	dms := "*DMs with the bot*\n" +
		"• In a DM the `@bot` prefix is implicit on top-level messages — just type the command or instructions\n" +
		"• First top-level DM starts a new task; subsequent top-level DMs continue the most-recent session\n" +
		"• Use `:: <text>` (or `*:` / `!:`) inside a DM to start a fresh task instead of continuing\n" +
		"• Bot commands (`close`, `set ...`, `allow @user`) work in DM thread replies via `<@bot> <command>`"

	refs := "*Slack references*\n" +
		"• Paste a Slack permalink (channel or thread link) and the bot will expand the referenced thread into the prompt\n" +
		"• Public channels: bot auto-joins if it isn't a member and posts a notice in the active thread\n" +
		"• Private channels: invite the bot first with `/invite @<bot>` — no scope can grant private-channel access without an invite\n" +
		"• Large or private references trigger a confirmation dialog before inclusion (Include inline, Save as asset, Skip, Cancel)"

	prompts := "*Per-task agent prompt*\n" +
		"• Each task's `README.md` is appended to claude's system prompt as `AGENT.md` on every run — edit it to give the agent persistent task context\n" +
		"• A workspace-wide `AGENTS.md` at the agents base dir applies to every task. Task-specific guidance overrides workspace-wide on conflict.\n" +
		"• File names are configurable via `CLOD_BOT_AGENTS_PROMPT_PATH` and `CLOD_BOT_AGENTS_SHARED_PROMPT_PATH`"

	mkSec := func(text string) slack.Block {
		return slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil,
		)
	}

	return []slack.Block{
		mkSec(":books: *How to use this bot*"),
		mkSec(starting),
		mkSec(perThread),
		mkSec(dms),
		mkSec(refs),
		mkSec(prompts),
	}
}
