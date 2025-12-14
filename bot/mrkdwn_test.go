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
