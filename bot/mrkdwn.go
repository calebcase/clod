package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/ast"
	"github.com/gomarkdown/markdown/parser"
)

// ConvertMarkdownToMrkdwn converts GitHub-flavored markdown to Slack's mrkdwn format.
// Uses an AST parser for robust handling of nested structures.
func ConvertMarkdownToMrkdwn(md string) string {
	// Peel off leading/trailing spaces and tabs before parsing. CommonMark
	// strips leading whitespace from paragraph text, so a streaming chunk
	// that begins with a single space (like " loading." arriving after a
	// prior "Models") would lose the word boundary on round-trip. Newlines
	// at the edges are still noise worth dropping via the final Trim —
	// callers expect a compact message without leading/trailing blank
	// lines.
	var leading, trailing string
	body := md
	for len(body) > 0 && (body[0] == ' ' || body[0] == '\t') {
		leading += string(body[0])
		body = body[1:]
	}
	for len(body) > 0 && (body[len(body)-1] == ' ' || body[len(body)-1] == '\t') {
		trailing = string(body[len(body)-1]) + trailing
		body = body[:len(body)-1]
	}

	extensions := parser.CommonExtensions | parser.AutoHeadingIDs | parser.Strikethrough
	p := parser.NewWithExtensions(extensions)

	data := markdown.NormalizeNewlines([]byte(body))
	node := p.Parse(data)

	renderer := &mrkdwnRenderer{}
	result := markdown.Render(node, renderer)

	return leading + strings.Trim(string(result), "\n\r") + trailing
}

// mrkdwnRenderer renders markdown AST to Slack's mrkdwn format.
type mrkdwnRenderer struct{}

func (r *mrkdwnRenderer) RenderNode(w io.Writer, node ast.Node, entering bool) ast.WalkStatus {
	switch n := node.(type) {
	case *ast.Document:
		return ast.GoToNext

	case *ast.Paragraph:
		if !entering {
			// Don't add newline if parent is a ListItem (it handles its own newlines).
			if _, isListItem := n.Parent.(*ast.ListItem); !isListItem {
				_, _ = fmt.Fprint(w, "\n")
			}
		}
		return ast.GoToNext

	case *ast.Text:
		if entering {
			_, _ = fmt.Fprint(w, string(n.Literal))
		}
		return ast.GoToNext

	case *ast.Strong:
		if entering {
			_, _ = fmt.Fprint(w, "*")
		} else {
			_, _ = fmt.Fprint(w, "*")
		}
		return ast.GoToNext

	case *ast.Emph:
		if entering {
			_, _ = fmt.Fprint(w, "_")
		} else {
			_, _ = fmt.Fprint(w, "_")
		}
		return ast.GoToNext

	case *ast.Del:
		if entering {
			_, _ = fmt.Fprint(w, "~")
		} else {
			_, _ = fmt.Fprint(w, "~")
		}
		return ast.GoToNext

	case *ast.Heading:
		if entering {
			_, _ = fmt.Fprint(w, "\n*")
		} else {
			_, _ = fmt.Fprint(w, "*\n")
		}
		return ast.GoToNext

	case *ast.Link:
		if entering {
			// Render children to get the link text.
			var textBuilder strings.Builder
			for _, child := range n.Children {
				childData := markdown.Render(child, r)
				textBuilder.Write(childData)
			}
			linkText := strings.TrimSpace(textBuilder.String())
			_, _ = fmt.Fprintf(w, "<%s|%s>", string(n.Destination), linkText)
			return ast.SkipChildren
		}
		return ast.GoToNext

	case *ast.Code:
		if entering {
			_, _ = fmt.Fprintf(w, "`%s`", string(n.Literal))
		}
		return ast.GoToNext

	case *ast.CodeBlock:
		if entering {
			code := strings.TrimSuffix(string(n.Literal), "\n")
			// Skip empty code blocks
			if code != "" {
				// Trailing blank line ensures two consecutive code blocks don't
				// end up with their closing and opening fences on adjacent
				// lines. Slack's mrkdwn parser renders "```\n```" as six
				// literal backticks instead of as a fence boundary, which
				// leaks the rest of the document out of code formatting.
				_, _ = fmt.Fprintf(w, "```\n%s\n```\n\n", code)
			}
		}
		return ast.GoToNext

	case *ast.List:
		if !entering {
			_, _ = fmt.Fprint(w, "\n")
		}
		return ast.GoToNext

	case *ast.ListItem:
		if entering {
			// Determine the bullet style.
			parent := n.Parent
			if list, ok := parent.(*ast.List); ok {
				if list.ListFlags&ast.ListTypeOrdered != 0 {
					// Find item index for ordered lists.
					idx := 1
					for i, sibling := range list.Children {
						if sibling == node {
							idx = i + 1
							break
						}
					}
					start := list.Start
					if start == 0 {
						start = 1
					}
					_, _ = fmt.Fprintf(w, "%d. ", idx+start-1)
				} else {
					_, _ = fmt.Fprint(w, "• ")
				}
			}
		} else {
			_, _ = fmt.Fprint(w, "\n")
		}
		return ast.GoToNext

	case *ast.BlockQuote:
		if entering {
			// Render children and prefix each line with >.
			var contentBuilder strings.Builder
			for _, child := range n.Children {
				childData := markdown.Render(child, r)
				contentBuilder.Write(childData)
			}
			content := strings.TrimSpace(contentBuilder.String())
			lines := strings.Split(content, "\n")
			for _, line := range lines {
				_, _ = fmt.Fprintf(w, "> %s\n", line)
			}
			return ast.SkipChildren
		}
		return ast.GoToNext

	case *ast.HorizontalRule:
		if entering {
			_, _ = fmt.Fprint(w, "\n---\n")
		}
		return ast.GoToNext

	case *ast.Table:
		// GFM pipe tables don't have a Slack-mrkdwn equivalent — Slack
		// has no native table syntax. The cleanest fallback is to
		// render as a fenced code block with space-padded columns:
		// monospace alignment carries the structure, the column gaps
		// are visually obvious, and the agent's `**bold**` markers
		// degrade to literal asterisks (less informative but readable
		// and unambiguous).
		//
		// Without this case the default walker descends through
		// Table → {Header,Body,Footer} → Row → Cell with no separator
		// emission, smushing every cell's text together. That's the
		// bug this case fixes — a table like
		//   | Config             | Time  | Speedup |
		//   | BF16+compile       | 33.11 | 1.00×   |
		//   | BF16+compile+FA4   | 19.16 | 1.73×   |
		// would otherwise render as
		//   `Config Time SpeedupBF16+compile33.111.00×...`
		// in Slack.
		if !entering {
			return ast.GoToNext
		}
		rows := collectTableRows(n, r)
		if len(rows) == 0 {
			return ast.SkipChildren
		}
		_, _ = fmt.Fprint(w, formatTableAsCodeBlock(rows))
		return ast.SkipChildren

	case *ast.Softbreak:
		if entering {
			_, _ = fmt.Fprint(w, "\n")
		}
		return ast.GoToNext

	case *ast.Hardbreak:
		if entering {
			_, _ = fmt.Fprint(w, "\n")
		}
		return ast.GoToNext

	case *ast.HTMLSpan:
		// Pass through HTML spans as-is.
		if entering {
			_, _ = fmt.Fprint(w, string(n.Literal))
		}
		return ast.GoToNext

	case *ast.HTMLBlock:
		// Pass through HTML blocks as-is.
		if entering {
			_, _ = fmt.Fprint(w, string(n.Literal))
		}
		return ast.GoToNext

	default:
		// For unknown nodes, try to render children.
		return ast.GoToNext
	}
}

func (r *mrkdwnRenderer) RenderHeader(w io.Writer, node ast.Node) {}

func (r *mrkdwnRenderer) RenderFooter(w io.Writer, node ast.Node) {}

// collectTableRows walks an *ast.Table subtree and returns its cells
// as a [][]string, header row first (when present), then body rows,
// then footer rows. Cell content is rendered through the same
// mrkdwn renderer (so `**bold**` becomes `*bold*`, links keep their
// `<url|text>` form, etc.) and then trimmed of surrounding
// whitespace and newlines so it fits a single table row.
func collectTableRows(table *ast.Table, r *mrkdwnRenderer) [][]string {
	var rows [][]string
	for _, section := range table.GetChildren() {
		// section is *ast.TableHeader, *ast.TableBody, or *ast.TableFooter
		for _, rowNode := range section.GetChildren() {
			row, ok := rowNode.(*ast.TableRow)
			if !ok {
				continue
			}
			var cells []string
			for _, cellNode := range row.GetChildren() {
				cell, ok := cellNode.(*ast.TableCell)
				if !ok {
					continue
				}
				var buf strings.Builder
				for _, child := range cell.GetChildren() {
					buf.Write(markdown.Render(child, r))
				}
				cells = append(cells, strings.TrimSpace(buf.String()))
			}
			rows = append(rows, cells)
		}
	}
	return rows
}

// formatTableAsCodeBlock renders rows into a fenced ``` block with
// space-padded columns. Header row (if any) is followed by a dashed
// separator so the eye finds the data section quickly.
func formatTableAsCodeBlock(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	// Column widths: max len of any cell in that column.
	cols := 0
	for _, row := range rows {
		if len(row) > cols {
			cols = len(row)
		}
	}
	widths := make([]int, cols)
	for _, row := range rows {
		for i, cell := range row {
			if l := len(cell); l > widths[i] {
				widths[i] = l
			}
		}
	}

	var out strings.Builder
	out.WriteString("```\n")
	const colGap = "  "
	writeRow := func(row []string) {
		for i := 0; i < cols; i++ {
			if i > 0 {
				out.WriteString(colGap)
			}
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			out.WriteString(cell)
			for p := len(cell); p < widths[i]; p++ {
				out.WriteByte(' ')
			}
		}
		out.WriteByte('\n')
	}
	writeRow(rows[0])
	if len(rows) > 1 {
		// dashed separator under the header
		for i := 0; i < cols; i++ {
			if i > 0 {
				out.WriteString(colGap)
			}
			for p := 0; p < widths[i]; p++ {
				out.WriteByte('-')
			}
		}
		out.WriteByte('\n')
	}
	for _, row := range rows[1:] {
		writeRow(row)
	}
	out.WriteString("```\n")
	return out.String()
}
