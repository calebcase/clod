package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/calebcase/oops"
	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
)

// Per-reference inclusion caps. Thread is considered "over cap" once it
// exceeds either the message-count or the character budget; the user is
// then asked whether to materialize it as a conversation asset.
const (
	slackRefMaxMessages = 50
	slackRefMaxChars    = 8000
)

// Permalink shape:
//
//	https://<workspace>.slack.com/archives/<channel>/p<16-digit-ts>[?thread_ts=<float>&...]
//
// The `p...` form is seconds+microseconds joined without the dot. Group 1:
// channel id. Group 2: raw p-ts. Group 3 (optional): thread root ts (already
// in `float` form).
var slackPermalinkPattern = regexp.MustCompile(
	`https?://[a-z0-9][a-z0-9-]*\.slack\.com/archives/([A-Z0-9]+)/p(\d{16})(?:[?&]thread_ts=(\d+\.\d+))?`,
)

// SlackRef is a parsed permalink pointing at a specific Slack message
// (possibly inside a thread).
type SlackRef struct {
	ChannelID string
	MessageTS string // ts of the specific message (seconds.microseconds)
	ThreadTS  string // non-empty if the permalink carries ?thread_ts=
	Permalink string
}

// RootTS returns the TS to pass to conversations.replies. If the permalink
// names a thread reply we want the root; otherwise the permalink's own ts
// acts as both root and leaf (single-message threads).
func (r SlackRef) RootTS() string {
	if r.ThreadTS != "" {
		return r.ThreadTS
	}
	return r.MessageTS
}

// FindSlackRefs extracts every slack permalink from the text, deduplicated
// by URL. Order is preserved (first occurrence wins).
func FindSlackRefs(text string) []SlackRef {
	matches := slackPermalinkPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	refs := make([]SlackRef, 0, len(matches))
	for _, m := range matches {
		if seen[m[0]] {
			continue
		}
		seen[m[0]] = true
		raw := m[2]
		if len(raw) < 7 {
			continue
		}
		ts := raw[:len(raw)-6] + "." + raw[len(raw)-6:]
		refs = append(refs, SlackRef{
			ChannelID: m[1],
			MessageTS: ts,
			ThreadTS:  m[3],
			Permalink: m[0],
		})
	}
	return refs
}

// SlackRefResult is a resolved reference — either a full thread (success)
// or an Err describing why we couldn't read it.
type SlackRefResult struct {
	Ref         SlackRef
	ChannelName string // channel name without leading "#", or "dm"/"group" marker
	IsPrivate   bool   // is_private || is_im || is_mpim
	IsDM        bool
	Messages    []slack.Message
	MsgCount    int  // len(Messages)
	CharCount   int  // total chars across message text
	Joined      bool // bot auto-joined the channel to read this ref
	Err         error
	ErrReason   string // user-facing summary of Err
}

// OverCap reports whether the resolved content exceeds either cap.
func (r *SlackRefResult) OverCap() bool {
	return r.MsgCount > slackRefMaxMessages || r.CharCount > slackRefMaxChars
}

// NeedsConfirm reports whether the ref requires user approval before
// inclusion. Private or DM channels always prompt; over-cap threads always
// prompt; public under-cap refs include silently.
func (r *SlackRefResult) NeedsConfirm() bool {
	return r.Err == nil && (r.IsPrivate || r.OverCap())
}

// resolveSlackRef looks up channel info + thread replies for a ref and
// returns a fully-populated result. Err is set (and returned nil) on any
// Slack API failure; the caller branches on Err.
func resolveSlackRef(client *slack.Client, ref SlackRef, logger zerolog.Logger) *SlackRefResult {
	res := &SlackRefResult{Ref: ref}

	info, err := client.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID:     ref.ChannelID,
		IncludeLocale: false,
	})
	if err != nil {
		res.Err = err
		res.ErrReason = humanizeSlackErr(err, "", scopeHintForInfo(ref.ChannelID))
		logger.Warn().Err(err).Str("channel", ref.ChannelID).Msg("failed to load channel info for ref")
		return res
	}
	res.ChannelName = info.Name
	res.IsPrivate = info.IsPrivate || info.IsIM || info.IsMpIM
	res.IsDM = info.IsIM || info.IsMpIM
	if res.ChannelName == "" {
		switch {
		case info.IsIM:
			res.ChannelName = "direct message"
		case info.IsMpIM:
			res.ChannelName = "group DM"
		default:
			res.ChannelName = ref.ChannelID
		}
	}

	msgs, err := fetchThreadReplies(client, ref)
	if err != nil && isNotInChannelErr(err) && isJoinableChannel(info) {
		// Bot lacks membership in a public channel it could join.
		// channels:join is in the manifest, so try auto-join and
		// retry once. This keeps the "bot isn't in #X" error
		// reserved for private channels / DMs where no scope or
		// auto-join can fix the situation.
		if _, _, _, joinErr := client.JoinConversation(ref.ChannelID); joinErr != nil {
			logger.Warn().Err(joinErr).Str("channel", ref.ChannelID).Msg("auto-join failed for ref channel")
			// Fall through — surface the original not_in_channel
			// error since the join attempt didn't succeed.
		} else {
			logger.Info().Str("channel", ref.ChannelID).Str("channel_name", res.ChannelName).Msg("auto-joined public channel to read ref")
			res.Joined = true
			msgs, err = fetchThreadReplies(client, ref)
		}
	}
	if err != nil {
		res.Err = err
		res.ErrReason = humanizeSlackErr(err, res.ChannelName, scopeHintForHistory(ref.ChannelID, res.IsDM, info.IsMpIM))
		logger.Warn().Err(err).Str("channel", ref.ChannelID).Msg("failed to load thread replies for ref")
		return res
	}

	res.Messages = msgs
	res.MsgCount = len(msgs)
	for _, m := range msgs {
		res.CharCount += len(m.Text)
	}
	return res
}

// fetchThreadReplies is a thin wrapper so we can retry cleanly after
// an auto-join without duplicating the parameter struct.
func fetchThreadReplies(client *slack.Client, ref SlackRef) ([]slack.Message, error) {
	msgs, _, _, err := client.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: ref.ChannelID,
		Timestamp: ref.RootTS(),
		Limit:     1000,
	})
	return msgs, err
}

// isNotInChannelErr reports whether a Slack API error indicates that
// the bot isn't a member of the target channel. Slack returns this
// via the bare `not_in_channel` error string.
func isNotInChannelErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "not_in_channel")
}

// isJoinableChannel reports whether the channel is one the bot can
// auto-join via conversations.join. Limited to public channels —
// private channels require an explicit invite (no scope fixes that),
// IMs and MpIMs aren't joinable at all, and #general / archived
// channels fail the API call anyway (Slack returns specific errors
// we'll surface instead of silently swallowing).
func isJoinableChannel(c *slack.Channel) bool {
	if c == nil {
		return false
	}
	if c.IsPrivate || c.IsIM || c.IsMpIM {
		return false
	}
	if c.IsArchived {
		return false
	}
	return c.IsChannel
}

// humanizeSlackErr turns a slack-go error into a short user-friendly
// sentence suitable for a thread post. scopeHint, when non-empty,
// names the OAuth scope the failing call requires — we derive it
// from the channel ID prefix (public / private / im / mpim) plus
// which call failed (info vs history). slack-go's error type doesn't
// surface the `needed:` field from the raw Slack response, so the
// hint is based on our own table rather than Slack's self-report.
func humanizeSlackErr(err error, channelName, scopeHint string) string {
	msg := err.Error()
	ch := channelName
	if ch != "" {
		ch = "#" + ch
	}
	switch {
	case strings.Contains(msg, "not_in_channel"), strings.Contains(msg, "channel_not_found"):
		if ch != "" {
			return fmt.Sprintf("bot isn't in %s (invite it there or copy the content over)", ch)
		}
		return "bot can't read that channel"
	case strings.Contains(msg, "thread_not_found"), strings.Contains(msg, "message_not_found"):
		return "message not found (deleted or moved?)"
	case strings.Contains(msg, "missing_scope"):
		if scopeHint != "" {
			return fmt.Sprintf("bot is missing the `%s` OAuth scope (reinstall the app after adding it in the Slack app config)", scopeHint)
		}
		return "bot is missing an OAuth scope required to read that channel"
	default:
		return msg
	}
}

// scopeHintForInfo returns the conversations.info scope required for a
// channel, inferred from the ID prefix. `C` = public, `G` = private
// channel or group-DM, `D` = 1:1 DM. `G` is ambiguous so we name both
// options; the caller can't distinguish mpim vs legacy-private before
// conversations.info returns.
func scopeHintForInfo(channelID string) string {
	switch {
	case strings.HasPrefix(channelID, "C"):
		return "channels:read"
	case strings.HasPrefix(channelID, "D"):
		return "im:read"
	case strings.HasPrefix(channelID, "G"):
		return "groups:read (or mpim:read for group DMs)"
	default:
		return ""
	}
}

// scopeHintForHistory returns the conversations.replies / history scope
// required for a channel. At this call site we already have
// conversations.info output, so the caller can distinguish mpim from
// legacy-private and pass the correct flags.
func scopeHintForHistory(channelID string, isIM, isMpIM bool) string {
	switch {
	case isIM:
		return "im:history"
	case isMpIM:
		return "mpim:history"
	case strings.HasPrefix(channelID, "C"):
		return "channels:history"
	case strings.HasPrefix(channelID, "G"):
		return "groups:history"
	default:
		return ""
	}
}

// FormatRefInline renders an under-cap result as a labeled block suitable
// for splicing into the agent prompt.
func FormatRefInline(res *SlackRefResult, userCache map[string]string, client *slack.Client, logger zerolog.Logger) string {
	var b strings.Builder
	tag := "#" + res.ChannelName
	if res.IsDM {
		tag = res.ChannelName
	}
	fmt.Fprintf(&b, "Referenced Slack conversation (%s — %s):\n", tag, res.Ref.Permalink)
	for _, m := range res.Messages {
		line := formatMessageLine(m, userCache, client, logger)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// formatMessageLine renders one message as "HH:MM <author>: text".
func formatMessageLine(m slack.Message, userCache map[string]string, client *slack.Client, logger zerolog.Logger) string {
	author := resolveUserName(m, userCache, client, logger)
	ts := slackTSToTime(m.Timestamp).UTC().Format("2006-01-02 15:04")
	text := strings.ReplaceAll(m.Text, "\n", "\n    ")
	return fmt.Sprintf("  [%s] %s: %s", ts, author, text)
}

// resolveUserName looks up a friendly display name for the message author,
// memoizing through userCache. Falls back to the raw id / "Bot" / "Unknown".
func resolveUserName(m slack.Message, userCache map[string]string, client *slack.Client, logger zerolog.Logger) string {
	if m.User == "" {
		if m.BotID != "" {
			return "Bot"
		}
		return "Unknown"
	}
	if cached, ok := userCache[m.User]; ok {
		return cached
	}
	name := m.User
	if u, err := client.GetUserInfo(m.User); err == nil && u != nil {
		if u.RealName != "" {
			name = u.RealName
		} else if u.Name != "" {
			name = u.Name
		}
	} else if err != nil {
		logger.Debug().Err(err).Str("user", m.User).Msg("failed to resolve user for ref formatting")
	}
	userCache[m.User] = name
	return name
}

// slackTSToTime converts a slack ts ("1712345678.123456") to time.Time.
// Best-effort; returns zero time on a malformed input.
func slackTSToTime(ts string) time.Time {
	dot := strings.IndexByte(ts, '.')
	if dot <= 0 {
		return time.Time{}
	}
	secs := ts[:dot]
	var s int64
	for _, c := range secs {
		if c < '0' || c > '9' {
			return time.Time{}
		}
		s = s*10 + int64(c-'0')
	}
	return time.Unix(s, 0)
}

// SaveConversationAsset materializes a resolved ref into a directory at
// <taskPath>/slack-<YYYYMMDDHHMMSS>-<slug>/ containing a thread.md render of
// all messages plus a files/ subdir with any downloaded attachments.
// Returns the absolute asset directory path on success.
func SaveConversationAsset(
	taskPath string,
	res *SlackRefResult,
	userCache map[string]string,
	client *slack.Client,
	logger zerolog.Logger,
) (string, error) {
	slug := slugifyMessage(res.Messages)
	stamp := time.Now().UTC().Format("20060102150405")
	dirName := fmt.Sprintf("slack-%s-%s", stamp, slug)
	assetDir := filepath.Join(taskPath, dirName)
	if err := os.MkdirAll(assetDir, 0755); err != nil {
		return "", oops.Trace(err)
	}

	// thread.md — headered render of every message.
	var md strings.Builder
	tag := "#" + res.ChannelName
	if res.IsDM {
		tag = res.ChannelName
	}
	fmt.Fprintf(&md, "# Slack thread (%s)\n\n", tag)
	fmt.Fprintf(&md, "- Permalink: %s\n", res.Ref.Permalink)
	fmt.Fprintf(&md, "- Messages: %d\n", res.MsgCount)
	if res.IsPrivate {
		fmt.Fprintf(&md, "- Source: *private conversation* — treat contents as sensitive\n")
	}
	md.WriteString("\n---\n\n")
	for _, m := range res.Messages {
		author := resolveUserName(m, userCache, client, logger)
		ts := slackTSToTime(m.Timestamp).UTC().Format("2006-01-02 15:04:05 UTC")
		fmt.Fprintf(&md, "### %s — %s\n\n", author, ts)
		if m.Text != "" {
			md.WriteString(m.Text)
			md.WriteString("\n\n")
		}
		if len(m.Files) > 0 {
			md.WriteString("_Attachments:_\n")
			for _, f := range m.Files {
				fmt.Fprintf(&md, "- `%s`\n", f.Name)
			}
			md.WriteString("\n")
		}
	}
	threadPath := filepath.Join(assetDir, "thread.md")
	if err := os.WriteFile(threadPath, []byte(md.String()), 0644); err != nil {
		return "", oops.Trace(err)
	}

	// Downloaded files, keyed by original Name. Name collisions get a
	// numeric suffix so we never clobber.
	filesDir := filepath.Join(assetDir, "files")
	var filesToFetch []slack.File
	for _, m := range res.Messages {
		filesToFetch = append(filesToFetch, m.Files...)
	}
	if len(filesToFetch) > 0 {
		if err := os.MkdirAll(filesDir, 0755); err != nil {
			return "", oops.Trace(err)
		}
		taken := make(map[string]int)
		for _, f := range filesToFetch {
			name := f.Name
			if name == "" {
				name = f.ID
			}
			if n := taken[name]; n > 0 {
				ext := filepath.Ext(name)
				base := strings.TrimSuffix(name, ext)
				name = fmt.Sprintf("%s-%d%s", base, n, ext)
			}
			taken[f.Name] = taken[f.Name] + 1
			if err := downloadSlackFileVia(client, f, filepath.Join(filesDir, name), logger); err != nil {
				logger.Warn().Err(err).Str("file", f.Name).Msg("failed to download asset attachment")
			}
		}
	}

	return assetDir, nil
}

// downloadSlackFileVia fetches a Slack file's content to dst using the
// slack-go client (which handles auth via the bot token already stored
// on the client). The client's GetFile signs the request correctly for
// both public and private files, avoiding an out-of-band token copy.
func downloadSlackFileVia(client *slack.Client, f slack.File, dst string, logger zerolog.Logger) error {
	url := f.URLPrivateDownload
	if url == "" {
		url = f.URLPrivate
	}
	if url == "" {
		return oops.New("no download URL on slack file")
	}
	out, err := os.Create(dst)
	if err != nil {
		return oops.Trace(err)
	}
	defer func() { _ = out.Close() }()
	if err := client.GetFile(url, out); err != nil {
		return oops.Trace(err)
	}
	logger.Debug().Str("dst", dst).Msg("downloaded referenced slack file")
	return nil
}

// slugifyMessage derives a short lowercase slug from the first message's
// text so asset dir names are vaguely memorable. Falls back to "thread" on
// empty input.
func slugifyMessage(msgs []slack.Message) string {
	var seed string
	if len(msgs) > 0 {
		seed = msgs[0].Text
	}
	return slugify(seed)
}

// slugify takes arbitrary text and returns a short [a-z0-9-] slug of at
// most ~5 tokens. Returns "thread" if the input yields no usable tokens.
func slugify(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "thread"
	}
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		tokens = append(tokens, cur.String())
		cur.Reset()
	}
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			cur.WriteRune(unicode.ToLower(r))
		default:
			flush()
			if len(tokens) >= 5 {
				break
			}
		}
	}
	flush()
	if len(tokens) > 5 {
		tokens = tokens[:5]
	}
	if len(tokens) == 0 {
		return "thread"
	}
	return strings.Join(tokens, "-")
}
