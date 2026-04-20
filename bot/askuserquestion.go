package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/slack-go/slack"
)

// askUserQuestion is one parsed question from an AskUserQuestion tool input.
type askUserQuestion struct {
	Header      string
	Question    string
	MultiSelect bool
	Options     []askOption
}

// askOption is one selectable answer for an AskUserQuestion question.
type askOption struct {
	Label       string
	Description string
}

// askUserQuestionState tracks an in-flight AskUserQuestion prompt while the
// user picks answers. The underlying permission is NOT resolved until the user
// clicks Submit.
type askUserQuestionState struct {
	MessageTS string // TS of the prompt message (for update after submit)
	ChannelID string
	ThreadTS  string
	Questions []askUserQuestion
	// Selections[i] is the list of picked option *indices* (as strings) for
	// question i. Single-select questions have at most one entry; multi-select
	// can have multiple. Empty slice = unanswered.
	Selections [][]string
}

// parseAskUserQuestionInput extracts the questions array from the tool input
// map. Returns an empty slice (not an error) if the shape doesn't match — the
// caller should fall back to the generic permission prompt in that case.
func parseAskUserQuestionInput(input map[string]any) []askUserQuestion {
	raw, ok := input["questions"].([]any)
	if !ok {
		return nil
	}

	questions := make([]askUserQuestion, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		q := askUserQuestion{
			Header:      strString(m["header"]),
			Question:    strString(m["question"]),
			MultiSelect: strBool(m["multiSelect"]),
		}
		if opts, ok := m["options"].([]any); ok {
			for _, o := range opts {
				om, ok := o.(map[string]any)
				if !ok {
					continue
				}
				q.Options = append(q.Options, askOption{
					Label:       strString(om["label"]),
					Description: strString(om["description"]),
				})
			}
		}
		questions = append(questions, q)
	}
	return questions
}

func strString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func strBool(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

// truncateForSlackText trims a string to fit the Slack plain_text 75-char cap
// used for radio/checkbox option labels.
func truncateForSlackText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	if limit <= 3 {
		return s[:limit]
	}
	return s[:limit-1] + "\u2026"
}

// buildAskUserQuestionBlocks renders the CLI-style Q&A interface as Slack
// blocks. Each question becomes a header + question text + a radio-buttons
// (single-select) or checkboxes (multi-select) accessory. A final Submit
// button resolves the underlying permission.
//
// Recommended options (labels ending with "(Recommended)") are preselected so
// the user can submit with one click for the default answer.
func buildAskUserQuestionBlocks(
	questions []askUserQuestion,
	progressKey string,
) []slack.Block {
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", ":bulb: *Question*", false, false),
			nil, nil,
		),
	}

	for i, q := range questions {
		// Header + question text.
		body := ""
		if q.Header != "" {
			body = fmt.Sprintf("*%s*", q.Header)
		}
		if q.Question != "" {
			if body != "" {
				body += "\n"
			}
			body += q.Question
		}
		if body == "" {
			body = fmt.Sprintf("Question %d", i+1)
		}
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", body, false, false),
			nil, nil,
		))

		// Options. Option values are option indices as strings so we don't
		// hit Slack's 75-char plain_text limit via the label.
		options := make([]*slack.OptionBlockObject, 0, len(q.Options))
		var recommended []*slack.OptionBlockObject
		for j, opt := range q.Options {
			label := truncateForSlackText(opt.Label, 75)
			text := slack.NewTextBlockObject("plain_text", label, false, false)
			var desc *slack.TextBlockObject
			if opt.Description != "" {
				desc = slack.NewTextBlockObject(
					"plain_text",
					truncateForSlackText(opt.Description, 75),
					false, false,
				)
			}
			obj := slack.NewOptionBlockObject(strconv.Itoa(j), text, desc)
			options = append(options, obj)
			if strings.Contains(strings.ToLower(opt.Label), "(recommended)") {
				recommended = append(recommended, obj)
			}
		}

		// block_id encodes the question index so the action handler can
		// route the selection back to the right question.
		blockID := fmt.Sprintf("askq_q%d", i)

		var picker slack.BlockElement
		if q.MultiSelect {
			cb := slack.NewCheckboxGroupsBlockElement("askq_checkbox", options...)
			if len(recommended) > 0 {
				cb.InitialOptions = recommended
			}
			picker = cb
		} else {
			rb := slack.NewRadioButtonsBlockElement("askq_radio", options...)
			if len(recommended) > 0 {
				rb.InitialOption = recommended[0]
			}
			picker = rb
		}

		actions := slack.NewActionBlock(blockID, picker)
		blocks = append(blocks, actions)
	}

	// Submit button carries the PermissionActionValue so HandleBlockAction
	// can locate the running task.
	submitValue := fmt.Sprintf(`{"k":%q,"b":"allow"}`, progressKey)
	submit := slack.NewButtonBlockElement(
		"askq_submit",
		submitValue,
		slack.NewTextBlockObject("plain_text", "Submit answers", false, false),
	)
	submit.Style = "primary"

	cancelValue := fmt.Sprintf(`{"k":%q,"b":"deny"}`, progressKey)
	cancel := slack.NewButtonBlockElement(
		"askq_cancel",
		cancelValue,
		slack.NewTextBlockObject("plain_text", "Cancel", false, false),
	)
	cancel.Style = "danger"

	blocks = append(blocks, slack.NewActionBlock("askq_submit_row", submit, cancel))
	return blocks
}

// formatAskUserQuestionAnswer produces a human-readable summary of the user's
// selections, used both in the updated prompt message and in the allow
// response's message field so Claude can see what the user chose.
func formatAskUserQuestionAnswer(state *askUserQuestionState) string {
	var sb strings.Builder
	for i, q := range state.Questions {
		header := q.Header
		if header == "" {
			header = fmt.Sprintf("Question %d", i+1)
		}
		sel := state.Selections[i]
		var labels []string
		for _, idxStr := range sel {
			idx, err := strconv.Atoi(idxStr)
			if err != nil || idx < 0 || idx >= len(q.Options) {
				continue
			}
			labels = append(labels, q.Options[idx].Label)
		}
		answer := "(no answer)"
		if len(labels) > 0 {
			answer = strings.Join(labels, ", ")
		}
		fmt.Fprintf(&sb, "• *%s*: %s\n", header, answer)
	}
	return sb.String()
}
