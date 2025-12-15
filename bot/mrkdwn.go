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
	extensions := parser.CommonExtensions | parser.AutoHeadingIDs | parser.Strikethrough
	p := parser.NewWithExtensions(extensions)

	data := markdown.NormalizeNewlines([]byte(md))
	node := p.Parse(data)

	renderer := &mrkdwnRenderer{}
	result := markdown.Render(node, renderer)

	return strings.TrimSpace(string(result))
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
			_, _ = fmt.Fprintf(w, "```\n%s\n```\n", code)
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
