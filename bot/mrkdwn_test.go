package main

import (
	"strings"
	"testing"
)

func TestConvertMarkdownToMrkdwn(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "bold double asterisk",
			input:    "This is **bold** text",
			expected: "This is *bold* text",
		},
		{
			name:     "italic underscore",
			input:    "This is _italic_ text",
			expected: "This is _italic_ text",
		},
		{
			name:     "italic asterisk",
			input:    "This is *italic* text",
			expected: "This is _italic_ text",
		},
		{
			name:     "h2 header",
			input:    "## Summary",
			expected: "*Summary*",
		},
		{
			name:     "h1 header",
			input:    "# Title",
			expected: "*Title*",
		},
		{
			name:     "link",
			input:    "[Click here](https://example.com)",
			expected: "<https://example.com|Click here>",
		},
		{
			name:     "strikethrough",
			input:    "This is ~~deleted~~ text",
			expected: "This is ~deleted~ text",
		},
		{
			name:     "code block language stripped",
			input:    "```bash\necho hello\n```",
			expected: "```\necho hello\n```",
		},
		{
			name:     "code block without language",
			input:    "```\necho hello\n```",
			expected: "```\necho hello\n```",
		},
		{
			name:     "inline code unchanged",
			input:    "Run `npm install` to install",
			expected: "Run `npm install` to install",
		},
		{
			name:     "unordered list",
			input:    "* Item one\n* Item two\n* Item three",
			expected: "• Item one\n• Item two\n• Item three",
		},
		{
			name:     "ordered list",
			input:    "1. First\n2. Second\n3. Third",
			expected: "1. First\n2. Second\n3. Third",
		},
		{
			name:     "blockquote",
			input:    "> This is a quote",
			expected: "> This is a quote",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ConvertMarkdownToMrkdwn(tt.input)
			// Normalize whitespace for comparison.
			result = strings.TrimSpace(result)
			expected := strings.TrimSpace(tt.expected)
			if result != expected {
				t.Errorf("ConvertMarkdownToMrkdwn(%q)\ngot:  %q\nwant: %q", tt.input, result, expected)
			}
		})
	}
}

func TestConvertMarkdownToMrkdwn_Complex(t *testing.T) {
	input := `## Summary

**Chosen Library:** pyfiglet

Here's how to use it:

1. Install the package
2. Import it
3. Call the function

` + "```python" + `
import pyfiglet
print(pyfiglet.figlet_format("Hello"))
` + "```" + `

For more info, see [the docs](https://example.com).
`

	result := ConvertMarkdownToMrkdwn(input)

	// Check key conversions are present.
	if !strings.Contains(result, "*Summary*") {
		t.Error("Header not converted to bold")
	}
	if !strings.Contains(result, "*Chosen Library:*") {
		t.Error("Bold not converted")
	}
	if !strings.Contains(result, "1. Install") {
		t.Error("Ordered list not preserved")
	}
	if !strings.Contains(result, "```\nimport pyfiglet") {
		t.Error("Code block language not stripped")
	}
	if !strings.Contains(result, "<https://example.com|the docs>") {
		t.Error("Link not converted")
	}
}

// TestGFMTableRendersAsCodeBlock guards the regression the bot saw
// in 0.31.x: a GFM pipe-table from claude/agent output had no
// `*ast.Table` case in mrkdwnRenderer, so cells fell through and
// concatenated with no separator. Bold markers around adjacent
// cells (`**14.40**` and `**2.30×**`) ended up touching, which
// rendered as the gibberish `*14.40**2.30×*` in Slack.
//
// Expected behaviour now: tables render as a fenced code block
// with space-padded columns. We don't pin the exact spacing —
// just check the cells appear as separate tokens, the bold-
// adjacency artefact is gone, and the result is wrapped in a
// fence so Slack treats it as monospace.
func TestGFMTableRendersAsCodeBlock(t *testing.T) {
	input := `| Config             | Time  | Speedup |
| ------------------ | ----- | ------- |
| BF16+compile       | 33.11 | 1.00×   |
| BF16+compile+FA4   | 19.16 | 1.73×   |
| TE+compile+FA4+fqkv | **14.40** | **2.30×** |
`
	result := ConvertMarkdownToMrkdwn(input)

	// Wrapped in a fenced code block.
	if !strings.HasPrefix(strings.TrimLeft(result, "\n"), "```") {
		t.Errorf("expected table to start with ```; got:\n%s", result)
	}
	// Separator row under the header.
	if !strings.Contains(result, "---") {
		t.Errorf("expected dashed header separator; got:\n%s", result)
	}
	// Cells must not be smushed together. Pick a transition that
	// the bug specifically manifested on:
	if strings.Contains(result, "33.111.00") || strings.Contains(result, "compile33.11") {
		t.Errorf("table cells appear concatenated without whitespace; got:\n%s", result)
	}
	// And the bold-adjacency artefact ("*14.40**2.30×*") must be gone.
	if strings.Contains(result, "*14.40**2.30") {
		t.Errorf("adjacent-bold collapse regression; got:\n%s", result)
	}
	// Cells should still be there.
	for _, cell := range []string{"BF16+compile", "33.11", "1.73×", "TE+compile+FA4+fqkv"} {
		if !strings.Contains(result, cell) {
			t.Errorf("missing cell %q in output:\n%s", cell, result)
		}
	}
}
