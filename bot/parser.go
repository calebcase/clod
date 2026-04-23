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
// for "run clod directly in the agents base directory" (rather than a
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
// sandbox, in the agents base directory". The `!:` form is visually
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

// closeMentionPattern matches `<@BOT> close` (optionally with trailing
// text we ignore). Case-insensitive.
var closeMentionPattern = regexp.MustCompile(`(?i)<@[A-Z0-9]+>\s+close\b`)

// ParseCloseCommand reports whether the mention is an explicit
// "close this session" command.
func ParseCloseCommand(text string) bool {
	return closeMentionPattern.MatchString(text)
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
