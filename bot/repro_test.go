package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestBashFenceAdjacency guards against Slack rendering bugs where:
//   - two inline Bash fences collapse into `"``````"` on Slack
//   - a closing fence gets glued to streamed assistant text so the fence
//     never terminates and subsequent content leaks out of code formatting
//
// Uses the exact inline format runner.go emits for short Bash results.
func TestBashFenceAdjacency(t *testing.T) {
	bashFmt := "\n```\n%s\n```\n"

	bash1 := "/tmp/clod-ssh-agent/agent.sock\nls: cannot access '/ssh*'\n/tmp/clod-ssh-agent/agent.sock"
	bash2 := "256 SHA256:mBN9cOIoJzy3axGkS caleb+agent@prodia.com (ED25519)\nHi caleb-agent!"
	assistant := "SSH works. Let me clone the inference repo."
	bash3 := "Cloning into 'inference'...\nDockerfile\nMakefile\nREADME.md"

	buffer := ""
	buffer += fmt.Sprintf(bashFmt, bash1)
	buffer += fmt.Sprintf(bashFmt, bash2)
	buffer += assistant
	buffer += fmt.Sprintf(bashFmt, bash3)

	out := ConvertMarkdownToMrkdwn(buffer)

	if strings.Contains(out, "```\n```") {
		t.Errorf("output contains adjacent fences (```\\n```):\n%s", out)
	}
	if strings.Contains(out, "```SSH") {
		t.Errorf("closing fence glued to assistant text (```SSH...):\n%s", out)
	}
	if !strings.Contains(out, assistant) {
		t.Errorf("assistant text missing from output:\n%s", out)
	}
	for i, b := range []string{bash1, bash2, bash3} {
		lastLine := b
		if idx := strings.LastIndex(b, "\n"); idx != -1 {
			lastLine = b[idx+1:]
		}
		needle := lastLine + "\n```"
		if !strings.Contains(out, needle) {
			t.Errorf("bash%d not properly terminated by closing fence in output:\n%s", i+1, out)
		}
	}
}

// TestBashFenceWithDeltaStreaming mimics the real-world sequence where
// content_block_delta chunks (streaming assistant text) arrive interleaved
// with bash tool results, and the final flushed buffer must not have any
// closing ``` glued to a following chunk.
func TestBashFenceWithDeltaStreaming(t *testing.T) {
	bashFmt := "\n```\n%s\n```\n"

	bashResult := "/usr/include/python3.13/Python.h\nii  libpython3-dev:amd64             3.13.5-1                        amd64       header files\nii  python3-dev                      3.13.5-1                        amd64       header files\n/usr/bin/python3\nPython 3.13.5"

	// Assistant text usually streams in many small deltas. Each just gets
	// concatenated into the buffer.
	deltas := []string{
		"Conf", "irmed. ", "`python3-dev`", " is installed, ", "`Python.h`", " present at ",
		"`/usr/include/python3.13/Python.h`", ". Resuming the worker bootstrap.",
	}

	buffer := ""
	buffer += fmt.Sprintf(bashFmt, bashResult)
	for _, d := range deltas {
		buffer += d
	}

	out := ConvertMarkdownToMrkdwn(buffer)

	// The bash content must end with the closing fence on its own line —
	// nothing glued to it.
	if !strings.Contains(out, "Python 3.13.5\n```\n") {
		t.Errorf("closing fence not properly terminated after Python 3.13.5 line:\n%s", out)
	}
	// Confirmed text must appear OUTSIDE any surrounding code fence.
	if strings.Contains(out, "```Confirmed") {
		t.Errorf("closing fence glued to 'Confirmed' text:\n%s", out)
	}
	// Entire phrase should survive.
	if !strings.Contains(out, "Confirmed. `python3-dev` is installed") {
		t.Errorf("assistant confirmation text missing:\n%s", out)
	}
}

// joinConsolidated applies the same separator logic as handlers.go flushBuffer.
// Kept in sync manually — if the logic there changes, update this too.
func joinConsolidated(prev, next string) string {
	if prev == "" {
		return next
	}
	if next == "" {
		return prev
	}
	prevEndsWithNewline := strings.HasSuffix(prev, "\n")
	newStartsWithNewline := strings.HasPrefix(next, "\n")
	prevEndsWithFence := strings.HasSuffix(prev, "```")
	newStartsWithFence := strings.HasPrefix(next, "```")
	var separator string
	switch {
	case prevEndsWithFence || newStartsWithFence:
		separator = "\n\n"
	case !prevEndsWithNewline && !newStartsWithNewline:
		separator = ""
	}
	return prev + separator + next
}

// TestConsolidationAcrossFlushes reproduces the actual bug path: each flush
// goes through TrimSpace + ConvertMarkdownToMrkdwn and then the handler
// concatenates onto the prior consolidated Slack message. That path stripped
// the blank-line padding between fenced blocks and glued ``` onto adjacent
// fences or text.
func TestConsolidationAcrossFlushes(t *testing.T) {
	bashFmt := "\n```\n%s\n```\n"

	// Simulate three independent flushes hitting the same consolidated message.
	flush1 := strings.TrimSpace(fmt.Sprintf(bashFmt, "/usr/include/python3.13/Python.h\nii  libpython3-dev\nPython 3.13.5"))
	flush1 = ConvertMarkdownToMrkdwn(flush1)

	flush2 := strings.TrimSpace("Confirmed. `python3-dev` is installed. Resuming the worker bootstrap.")
	flush2 = ConvertMarkdownToMrkdwn(flush2)

	flush3 := strings.TrimSpace(fmt.Sprintf(bashFmt, "(Bash completed with no output)"))
	flush3 = ConvertMarkdownToMrkdwn(flush3)

	flush4 := strings.TrimSpace(fmt.Sprintf(bashFmt, "/home/ubuntu/src/github.com/prodialabs/agents/processor/inference"))
	flush4 = ConvertMarkdownToMrkdwn(flush4)

	flush5 := strings.TrimSpace(fmt.Sprintf(bashFmt, "(Bash completed with no output)"))
	flush5 = ConvertMarkdownToMrkdwn(flush5)

	consolidated := flush1
	for _, f := range []string{flush2, flush3, flush4, flush5} {
		consolidated = joinConsolidated(consolidated, f)
	}

	if strings.Contains(consolidated, "``````") {
		t.Errorf("consolidated message contains six literal backticks:\n%s", consolidated)
	}
	if strings.Contains(consolidated, "```Confirmed") {
		t.Errorf("closing fence glued to 'Confirmed' text in consolidated:\n%s", consolidated)
	}
	if strings.Contains(consolidated, "```\n```") {
		t.Errorf("adjacent fences in consolidated output:\n%s", consolidated)
	}
	fmt.Println("TestConsolidationAcrossFlushes output:")
	fmt.Println(consolidated)
}

// TestSplitStreamingTextPreservesWordBoundary guards against the
// "Modelsloading." bug: claude streams sentence fragments that often begin
// or end with a single space ("Models" then " loading."), and
// flushBuffer's edge trim must not eat that space or consolidation will
// join the two chunks into one long word.
func TestSplitStreamingTextPreservesWordBoundary(t *testing.T) {
	// Two deltas from claude, each flushed in its own tick.
	chunk1 := "Models"
	chunk2 := " loading."

	// flushBuffer now trims only \n\r\t (see handlers.go). Simulate the
	// same trim + convert + consolidation join.
	trim := func(s string) string { return strings.Trim(s, "\n\r\t") }

	a := ConvertMarkdownToMrkdwn(trim(chunk1))
	b := ConvertMarkdownToMrkdwn(trim(chunk2))

	joined := joinConsolidated(a, b)
	if !strings.Contains(joined, "Models loading.") {
		t.Errorf("consolidation ate the word boundary space:\ngot:  %q\nwant substring: %q", joined, "Models loading.")
	}
}

// TestTwoShortBashResultsInOneFlush reproduces the "(Bash completed with no
// output)" scenario where two adjacent bash calls return trivial content.
func TestTwoShortBashResultsInOneFlush(t *testing.T) {
	bashFmt := "\n```\n%s\n```\n"

	buffer := ""
	buffer += fmt.Sprintf(bashFmt, "(Bash completed with no output)")
	buffer += fmt.Sprintf(bashFmt, "/home/ubuntu/src/github.com/prodialabs/agents/processor/inference")
	buffer += fmt.Sprintf(bashFmt, "(Bash completed with no output)")

	out := ConvertMarkdownToMrkdwn(buffer)

	// No adjacent fences.
	if strings.Contains(out, "```\n```") {
		t.Errorf("adjacent fences in output:\n%s", out)
	}
	// Literal "``````" (six backticks) on a line is the visible Slack
	// manifestation.
	if strings.Contains(out, "``````") {
		t.Errorf("six literal backticks in output:\n%s", out)
	}
	fmt.Println("TestTwoShortBashResultsInOneFlush output:")
	fmt.Println(out)
}
