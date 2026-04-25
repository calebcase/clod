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

// personalSectionRecency is the staleness threshold for the "Your
// recent sessions" list. Sessions whose UpdatedAt is older than
// this are hidden so the view focuses on active work. They aren't
// deleted — sessions.json keeps them and @-mentioning in the
// thread still resumes.
const personalSectionRecency = 7 * 24 * time.Hour

// usageRollupWindows is the set of time windows the Workspace
// section rolls usage up over. Must stay in ascending order —
// UsageRollup aligns its result slices with this order.
var usageRollupWindows = []time.Duration{
	24 * time.Hour,
	7 * 24 * time.Hour,
	30 * 24 * time.Hour,
	90 * 24 * time.Hour,
	365 * 24 * time.Hour,
}

// usageRollupWindowLabels are the short labels rendered in the
// Workspace table, aligned with usageRollupWindows.
var usageRollupWindowLabels = []string{"24h", "7d", "30d", "90d", "365d"}

// buildHomeTabView assembles the Block Kit view the app's Home tab
// will show for `userID`. The personal section always appears;
// the workspace-wide section is appended only when
// `includeWorkspace` is true (i.e. the user is on the bot-wide
// allowlist).
//
// permalinkFor resolves a (channel, message-ts) into a Slack
// permalink string — used to turn session task names into
// clickable links back to each thread's anchor message.
// latestPermalinkFor resolves a (channel, thread-ts) into a
// permalink for the most recently tracked bot post in that thread,
// rendered as a "(latest)" link beside the task name. Either
// resolver may be nil, and either may return empty string for any
// given input; the formatter falls back to plain text in those
// cases.
func buildHomeTabView(
	sessions []*SessionMapping,
	rollup map[string][]UsageTotals,
	permalinkFor func(channelID, messageTS string) string,
	latestPermalinkFor func(channelID, threadTS string) string,
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

	// Personal section — recent sessions only. "Recent" means touched
	// within the last 7 days; stale sessions (idle >7d) roll off
	// this list so the view doesn't become a lifetime archive. They
	// still exist in sessions.json and resume normally if the user
	// @-mentions in the thread.
	mine := filterByUser(sessions, userID)
	recent := filterRecent(mine, now, personalSectionRecency)
	blocks = append(blocks, slack.NewDividerBlock())
	blocks = append(blocks, buildUsageHeader("Your recent sessions (active in the last 7 days)", recent))
	blocks = append(blocks, buildSessionRows(recent, now, false, permalinkFor, latestPermalinkFor)...)

	if includeWorkspace {
		blocks = append(blocks, slack.NewDividerBlock())
		blocks = append(blocks, buildWorkspaceRollupBlocks(rollup)...)
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
func buildSessionRows(sessions []*SessionMapping, now time.Time, showUser bool, permalinkFor func(channelID, messageTS string) string, latestPermalinkFor func(channelID, threadTS string) string) []slack.Block {
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
			slack.NewTextBlockObject("mrkdwn", formatSessionLine(s, now, showUser, permalinkFor, latestPermalinkFor), false, false),
			nil, nil,
		))
	}
	return rows
}

// formatSessionLine renders the mrkdwn text for a single session row.
// When permalinkFor yields a URL for the session's anchor message,
// the task name becomes a clickable link to that message in Slack;
// otherwise it falls back to a plain bold label. When
// latestPermalinkFor yields a URL for the thread's last tracked
// bot post, a `[latest →]` link is rendered alongside the task
// name so users can jump straight to the most recent activity
// without scrolling from the anchor.
func formatSessionLine(s *SessionMapping, now time.Time, showUser bool, permalinkFor func(channelID, messageTS string) string, latestPermalinkFor func(channelID, threadTS string) string) string {
	status := ":white_circle: idle"
	if s.Active {
		status = ":large_green_circle: active"
	}

	// Prefer the reaction anchor (the user's @-mention that kicked
	// off the task) as the link target. For older sessions that
	// predate ReactionAnchorTS, fall back to the thread root.
	anchor := s.ReactionAnchorTS
	if anchor == "" {
		anchor = s.ThreadTS
	}
	var label string
	var url string
	if permalinkFor != nil {
		url = permalinkFor(s.ChannelID, anchor)
	}
	if url != "" {
		// Slack mrkdwn doesn't render code spans inside a link
		// label, so we drop the backticks and keep the outer
		// asterisks for bold weight. `\| escapes the separator;
		// task names are validated as [a-zA-Z0-9_-] by
		// safeTaskNamePattern so they don't contain pipes, but
		// belt-and-suspenders.
		label = fmt.Sprintf("*<%s|%s>*", url, strings.ReplaceAll(s.TaskName, "|", `\|`))
	} else {
		label = fmt.Sprintf("*`%s`*", s.TaskName)
	}

	// Append a `[latest →]` jump link when the bot has tracked a
	// post in this thread (since the latestPostTS table was added).
	// Skipped silently for older sessions or when the latest post
	// IS the anchor (same destination as the task-name link).
	if latestPermalinkFor != nil {
		if latestURL := latestPermalinkFor(s.ChannelID, s.ThreadTS); latestURL != "" && latestURL != url {
			label += fmt.Sprintf(" <%s|[latest →]>", latestURL)
		}
	}

	var parts []string
	parts = append(parts, label)
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

// filterRecent returns only sessions whose UpdatedAt falls within
// the window back from now. Used to hide stale sessions from the
// personal list so the Home tab doesn't turn into a lifetime
// archive — stale entries still exist in sessions.json and resume
// on @-mention.
func filterRecent(sessions []*SessionMapping, now time.Time, window time.Duration) []*SessionMapping {
	cutoff := now.Add(-window)
	out := make([]*SessionMapping, 0, len(sessions))
	for _, s := range sessions {
		if s.UpdatedAt.After(cutoff) {
			out = append(out, s)
		}
	}
	return out
}

// buildWorkspaceRollupBlocks renders the Workspace usage rollup: one
// row per user who has any activity in the last 365d, showing cost
// and turn totals for each of the standard windows (24h, 7d, 30d,
// 90d, 365d). Users are ordered by 30-day cost descending so the
// most active show up first.
//
// When no user has any activity, renders a context block noting the
// empty state rather than a bare header.
func buildWorkspaceRollupBlocks(rollup map[string][]UsageTotals) []slack.Block {
	var blocks []slack.Block
	blocks = append(blocks, slack.NewSectionBlock(
		slack.NewTextBlockObject(
			"mrkdwn",
			"*Workspace — per-user usage*\n_Totals over the last 24h · 7d · 30d · 90d · 365d. Only counts activity recorded on or after 2026-04-24 when sample tracking began._",
			false, false,
		),
		nil, nil,
	))

	if len(rollup) == 0 {
		blocks = append(blocks, slack.NewContextBlock(
			"",
			slack.NewTextBlockObject("mrkdwn", "_No activity recorded yet._", false, false),
		))
		return blocks
	}

	// Sort users by their 30-day cost (index 2 in usageRollupWindows)
	// descending, with total 365d cost as a tiebreaker. Stable order
	// so refreshes don't shuffle the list when the user-level totals
	// haven't changed.
	type userRow struct {
		UserID string
		Totals []UsageTotals
	}
	rows := make([]userRow, 0, len(rollup))
	for u, t := range rollup {
		rows = append(rows, userRow{UserID: u, Totals: t})
	}
	sortKey := func(r userRow) (float64, float64) {
		if len(r.Totals) >= 3 {
			var last float64
			if n := len(r.Totals); n > 0 {
				last = r.Totals[n-1].CostUSD
			}
			return r.Totals[2].CostUSD, last
		}
		return 0, 0
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, aa := sortKey(rows[i])
		b, bb := sortKey(rows[j])
		if a != b {
			return a > b
		}
		return aa > bb
	})

	// Render one section block per user. Row text is built as a
	// dot-separated sequence `<label> $X.XX (Y turns)` so each
	// window's cost + turn-count stays visually paired. Users who
	// have zero totals across every window are omitted — they'd
	// appear only if they had activity once long ago that expired
	// out of the 365d window between rollups.
	for _, r := range rows {
		if allZero(r.Totals) {
			continue
		}
		parts := make([]string, 0, len(usageRollupWindowLabels))
		for i, label := range usageRollupWindowLabels {
			if i >= len(r.Totals) {
				break
			}
			t := r.Totals[i]
			parts = append(parts, fmt.Sprintf("%s $%.2f (%d)", label, t.CostUSD, t.Turns))
		}
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(
				"mrkdwn",
				fmt.Sprintf("*<@%s>* — %s", r.UserID, strings.Join(parts, " · ")),
				false, false,
			),
			nil, nil,
		))
	}
	return blocks
}

func allZero(totals []UsageTotals) bool {
	for _, t := range totals {
		if t.CostUSD != 0 || t.Turns != 0 {
			return false
		}
	}
	return true
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
		"• `@bot upload <path>` — upload a host-filesystem file (or directory, with a recursive-vs-top-level prompt) into this thread. >5 files get zipped to /tmp first.\n" +
		"• `@bot allow @user` / `@bot disallow @user` — manage who else can drive this thread\n" +
		"• `@bot set model=opus|sonnet|haiku` — switch model. `+` / `-` to cycle, or send 🎼 / 📜 / 🌸\n" +
		"• `@bot set verbosity=0|1|-1` — silent / summary / full. Or 🙈 / 💬\n" +
		"• `@bot set plan=on|off` — toggle plan mode. Or `+` / `-` / 💭\n" +
		"• `@bot set filesync=on|off` — toggle file syncing for the task dir back to Slack"

	dms := "*DMs with the bot*\n" +
		"• Top-level DMs need an explicit prefix (`*:`, `!:`, `::`, `<template>::`, or `<task>:`) — the `@bot` mention is implicit. Anything else returns this usage info instead of starting or continuing a task.\n" +
		"• Inside an active session's thread, just type to send input to the running task. No prefix needed.\n" +
		"• Bot commands inside a thread (`close`, `set ...`, `allow @user`) need an explicit `<@bot> <command>` so they reach the command router rather than the agent."

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
