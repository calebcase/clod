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
