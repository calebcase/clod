package main

import (
	"regexp"
	"strings"
)

// ParsedMention represents a parsed bot mention.
type ParsedMention struct {
	TaskName     string // e.g., "deprecation"
	Instructions string // e.g., "Follow the instructions in upstream-deprecation.md"
}

// mentionPattern matches: <@BOT_ID> task_name: instructions
// Group 1: task name (non-whitespace characters before colon)
// Group 2: instructions (everything after colon)
var mentionPattern = regexp.MustCompile(`<@[A-Z0-9]+>\s+(\S+?):\s*(.+)`)

// ParseMention parses a Slack message containing a bot mention.
// Expected format: "@Bot task_name: instructions here"
// Returns nil if the message doesn't match the expected format.
func ParseMention(text string) *ParsedMention {
	matches := mentionPattern.FindStringSubmatch(text)
	if len(matches) < 3 {
		return nil
	}

	return &ParsedMention{
		TaskName:     strings.ToLower(matches[1]),
		Instructions: strings.TrimSpace(matches[2]),
	}
}

// modelPrefixPattern matches `<@BOT> <model> <rest>` where `<model>` is
// a recognised family name (`opus`, `sonnet`, `haiku`, plus the
// `best`/`default`/`opusplan` aliases) or a specific point release
// (`claude-(opus|sonnet|haiku)-X.Y...`), with an optional `[1m]`
// 1M-context suffix. The pattern is constrained on purpose: a free-
// form first-word match would silently swallow ordinary user prose
// that happened to start with a real word.
//
// Group 1: bot mention (preserved so callers can rebuild the message
// without the model token but with the same `<@BOT>` prefix).
// Group 2: model token (with optional `[1m]`).
// Group 3: rest of the message.
var modelPrefixPattern = regexp.MustCompile(
	`(?i)^(<@[A-Z0-9]+>)\s+((?:opus|sonnet|haiku|best|default|opusplan|claude-(?:opus|sonnet|haiku)-[\w.-]+)(?:\[1m\])?)\s+(.+)$`,
)

// ParseModelPrefix peeks at the first whitespace-delimited word after
// the bot mention and, when it's a recognised model name, returns
// `(rewritten_text_without_model, model_token)`. Otherwise returns
// `(text, "")`.
//
// The rewritten text retains the original `<@BOT>` mention so it can
// be re-fed to any of the existing start-pattern parsers (mention,
// auto-name, named-template, root, dangerous-root) without further
// massaging. Callers should still verify the rewritten text matches a
// start pattern before applying the model — for non-start commands
// (`close`, `set …`, etc.) a leading model word is meaningless and
// should not be silently swallowed.
func ParseModelPrefix(text string) (rewritten string, model string) {
	m := modelPrefixPattern.FindStringSubmatch(text)
	if len(m) < 4 {
		return text, ""
	}
	return m[1] + " " + m[3], strings.ToLower(m[2])
}

// ParseContinuation parses a follow-up message in a thread (no task prefix needed).
// Just strips the bot mention and returns the rest as instructions.
var continuationPattern = regexp.MustCompile(`<@[A-Z0-9]+>\s*(.*)`)

func ParseContinuation(text string) string {
	matches := continuationPattern.FindStringSubmatch(text)
	if len(matches) < 2 {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(matches[1])
}

// autoNamePattern matches `<@BOT> :: instructions` — the shorthand for
// "start a new task with an auto-generated memorable name".
var autoNamePattern = regexp.MustCompile(`<@[A-Z0-9]+>\s+::\s*(.+)`)

// namedAutoPattern matches `<@BOT> <template>:: instructions` — the
// shorthand for "auto-name a new task, copy the named sibling as the
// template, skip the init dialog entirely". Template name must match
// the same safe shape we accept for user-named tasks (alphanumerics
// plus _-, up to 64 chars). `::` must sit directly after the name with
// no separating whitespace — that's what disambiguates this form from
// `<@BOT> <name>: instructions` (explicit task) and `<@BOT> :: ...`
// (no template).
var namedAutoPattern = regexp.MustCompile(`<@[A-Z0-9]+>\s+([a-zA-Z0-9][a-zA-Z0-9_-]{0,63})::\s*(.+)`)

// rootMentionPattern matches `<@BOT> *: instructions` — the shorthand
// for "run clod directly in the workspace root" (rather than a
// subdirectory task). The base dir itself is treated as the task; it
// gets its own `.clod/` that clod initializes when missing.
var rootMentionPattern = regexp.MustCompile(`<@[A-Z0-9]+>\s+\*:\s*(.+)`)

// ParseRootMention returns the instructions from a `@bot *: ...`
// message, or empty string when the text doesn't match.
func ParseRootMention(text string) string {
	m := rootMentionPattern.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// dangerousRootMentionPattern matches `<@BOT> !: instructions` — the
// shorthand for "run claude directly on the host, outside any docker
// sandbox, in the workspace root". The `!:` form is visually
// distinct from `*:` to reinforce that the user is opting out of the
// container isolation.
var dangerousRootMentionPattern = regexp.MustCompile(`<@[A-Z0-9]+>\s+!:\s*(.+)`)

// ParseDangerousRootMention returns the instructions from a `@bot !: ...`
// message, or empty string when the text doesn't match.
func ParseDangerousRootMention(text string) string {
	m := dangerousRootMentionPattern.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// ParseAutoNameMention extracts the instructions from a `@bot :: ...`
// message. Returns an empty string when the message doesn't match.
func ParseAutoNameMention(text string) string {
	m := autoNamePattern.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// NamedAutoMention is a parsed `@bot <template>:: <instructions>`
// command. Template is the name of the sibling task to copy; it's
// validated downstream (must exist, must not be the generated
// auto-name, must not itself be an auto-named one-off).
type NamedAutoMention struct {
	Template     string
	Instructions string
}

// ParseNamedAutoMention extracts (template, instructions) from a
// `@bot <template>:: <instructions>` message. Returns nil when the
// text doesn't match the shape.
func ParseNamedAutoMention(text string) *NamedAutoMention {
	m := namedAutoPattern.FindStringSubmatch(text)
	if len(m) < 3 {
		return nil
	}
	return &NamedAutoMention{
		Template:     m[1],
		Instructions: strings.TrimSpace(m[2]),
	}
}

// hasStartPattern reports whether `text` matches any of the start-
// session shapes (explicit-domain, template auto-name, bare auto-name,
// workspace root, or host-direct). Used by HandleAppMention to gate
// the optional model-prefix preprocessor: model-stripping only takes
// effect when the rewritten text would actually start a session.
func hasStartPattern(text string) bool {
	if ParseDangerousRootMention(text) != "" {
		return true
	}
	if ParseRootMention(text) != "" {
		return true
	}
	if ParseNamedAutoMention(text) != nil {
		return true
	}
	if ParseAutoNameMention(text) != "" {
		return true
	}
	if ParseMention(text) != nil {
		return true
	}
	return false
}

// closeMentionPattern matches `<@BOT> close` (optionally with trailing
// text we ignore). Case-insensitive.
var closeMentionPattern = regexp.MustCompile(`(?i)<@[A-Z0-9]+>\s+close\b`)

// ParseCloseCommand reports whether the mention is an explicit
// "close this session" command.
func ParseCloseCommand(text string) bool {
	return closeMentionPattern.MatchString(text)
}

// uploadCommandPattern matches `<@BOT> upload <path>`. Path can
// contain spaces; we capture greedily and let the handler trim. The
// path is taken verbatim — no shell-style quoting / escaping.
var uploadCommandPattern = regexp.MustCompile(`(?i)<@[A-Z0-9]+>\s+upload\s+(.+?)\s*$`)

// ParseUploadCommand extracts the target path from an upload
// command mention. Returns empty string when the message isn't an
// upload command.
func ParseUploadCommand(text string) string {
	m := uploadCommandPattern.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// AllowCommand represents a parsed `@bot allow @user` or
// `@bot disallow @user` message. Action is "allow" or "disallow".
// UserID is the target Slack user id (without the `<@...>` wrapping).
type AllowCommand struct {
	Action string
	UserID string
}

// allowCommandPattern matches: <@BOT> (allow|disallow) <@USERID>
var allowCommandPattern = regexp.MustCompile(`(?i)<@[A-Z0-9]+>\s+(allow|disallow)\s+<@([A-Z0-9]+)>`)

// ParseAllowCommand extracts an allow/disallow command from a bot
// mention. Returns nil if the message doesn't match the shape.
func ParseAllowCommand(text string) *AllowCommand {
	m := allowCommandPattern.FindStringSubmatch(text)
	if len(m) < 3 {
		return nil
	}
	return &AllowCommand{
		Action: strings.ToLower(m[1]),
		UserID: m[2],
	}
}

// SetCommand represents a parsed `@bot set FIELD=VALUE` message.
type SetCommand struct {
	Field string // normalized: "verbosity", "model", "plan"
	Value string // raw value token (whitespace trimmed)
}

// setCommandPattern matches: <@BOT> set FIELD=VALUE
// Group 1: field name. Group 2: value (greedy rest of line).
// Field names are alphanumeric + dash/underscore. Value can contain
// anything including emoji markup like :musical_score:.
var setCommandPattern = regexp.MustCompile(`(?i)<@[A-Z0-9]+>\s+set\s+([a-z_-]+)\s*=\s*(.+)`)

// ParseSetCommand extracts a `set field=value` command from a bot mention.
// Returns nil if the message isn't a set command. The field is
// lowercased; the value is whitespace-trimmed but otherwise preserved
// (callers interpret it based on the field — e.g. emoji markup vs
// literal values vs "+"/"-" deltas).
func ParseSetCommand(text string) *SetCommand {
	m := setCommandPattern.FindStringSubmatch(text)
	if len(m) < 3 {
		return nil
	}
	return &SetCommand{
		Field: strings.ToLower(strings.TrimSpace(m[1])),
		Value: strings.TrimSpace(m[2]),
	}
}
