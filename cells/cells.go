// Package cells reads a lab note (ADR 1792457255): its kernel, named in its front matter, and its
// cells, the fences in the kernel's language. The note is parsed as AutoDoc's export and Rendered
// view parse it (kind.ParseMarkdown), and a cell's key is lab.CellKey of what they read, so the
// outputs the plugin writes are the ones they find.
package cells

import (
	"strings"

	"github.com/yongjohnlee80/autodoc/core/kind"
	"github.com/yongjohnlee80/autodoc/core/lab"
	"github.com/yongjohnlee80/golib/parse/markdown"
	pyaml "github.com/yongjohnlee80/golib/parse/yaml"
	"github.com/yongjohnlee80/golib/yaml"
)

// Cell is one fence of a lab note in the kernel's language.
type Cell struct {
	Lang string // lab.FenceLang of its info string
	Code string // what is between its fences, as the export reads it
	// Start and End are the fence's bytes in the note, its fence lines included.
	Start, End int
}

// Key is the cell's key: its outputs' folder under the note's lab.OutDir.
func (c Cell) Key() string { return lab.CellKey(c.Lang, c.Code) }

// Note is a lab note read: its kernel's name ("" when its front matter names none), and every
// fenced code block, each with its language.
type Note struct {
	Kernel string
	fences []Cell
}

// Read reads a lab note's text.
func Read(text string) Note {
	doc := kind.ParseMarkdown([]byte(text))
	var n Note
	var walk func(*markdown.Node)
	walk = func(node *markdown.Node) {
		for c := node.FirstChild; c != nil; c = c.Next {
			switch {
			case c.Kind == markdown.KindFrontmatter:
				n.Kernel = kernelOf(c.Literal)
			case c.Kind == markdown.KindCodeBlock && c.Fence != 0:
				n.fences = append(n.fences, Cell{Lang: lab.FenceLang(string(c.Info)), Code: string(c.Literal), Start: c.Span.Start, End: c.Span.End})
			default:
				walk(c) // a fence in a list item or a quote is a cell too, as the export finds it
			}
		}
	}
	walk(doc.Root)
	return n
}

// kernelOf is the front matter's kernel: a string; "" for none, or front matter that is not YAML.
func kernelOf(front []byte) string {
	st, err := pyaml.Parse(front)
	if err != nil || len(st.Docs) == 0 {
		return ""
	}
	v, err := yaml.Evaluate(st.Docs[0], yaml.Core)
	if err != nil {
		return ""
	}
	m, ok := v.(yaml.Map)
	if !ok {
		return ""
	}
	k, _ := m.Get("kernel")
	s, _ := k.(string)
	return strings.TrimSpace(s)
}

// Cells are the note's fences in lang (a kernelspec's language), in order: its cells. Any other
// fence (mermaid, csv, vega-lite, text) is content, and never runs.
func (n Note) Cells(lang string) []Cell {
	lang = strings.ToLower(lang)
	var out []Cell
	for _, c := range n.fences {
		if c.Lang == lang && lang != "" {
			out = append(out, c)
		}
	}
	return out
}

// At is the cell in lang holding the byte offset at, its fence lines included; false when at is in
// none.
func (n Note) At(lang string, at int) (Cell, bool) {
	for _, c := range n.Cells(lang) {
		if at >= c.Start && at <= c.End {
			return c, true
		}
	}
	return Cell{}, false
}
