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
