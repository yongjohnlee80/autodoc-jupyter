// Package ipynb turns a Jupyter notebook (nbformat 4) into a lab note and back (ADR 1792457255
// §4.6.3). A markdown cell is its source; a code cell is a fence in the kernel's language, its
// outputs kept as the cell's files (package outputs); a raw cell is a "raw" fence, which never runs.
package ipynb

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/yongjohnlee80/autodoc/core/lab"

	"github.com/yongjohnlee80/autodoc-jupyter/cells"
	"github.com/yongjohnlee80/autodoc-jupyter/outputs"
)

// notebook is the part of nbformat 4 read and written.
type notebook struct {
	Cells    []cell   `json:"cells"`
	Metadata metadata `json:"metadata"`
	Format   int      `json:"nbformat"`
	Minor    int      `json:"nbformat_minor"`
}

type metadata struct {
	Kernelspec struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Language    string `json:"language"`
	} `json:"kernelspec"`
	LanguageInfo struct {
		Name string `json:"name"`
	} `json:"language_info"`
}

// cell is a notebook cell. A code cell always has outputs and an execution count (null when it has
// not run), and no other cell has either: both are raw JSON, left out when unset.
type cell struct {
	Type           string          `json:"cell_type"`
	ID             string          `json:"id,omitempty"`
	Metadata       map[string]any  `json:"metadata"`
	Source         json.RawMessage `json:"source"`
	Outputs        json.RawMessage `json:"outputs,omitempty"`
	ExecutionCount json.RawMessage `json:"execution_count,omitempty"`
}

// RawLang is the fence a raw cell is kept in.
const RawLang = "raw"

// LabPath is the lab note beside the notebook at p: nb.ipynb's nb.lab.md.
func LabPath(p string) string { return strings.TrimSuffix(p, filepath.Ext(p)) + lab.Ext }

// NotebookPath is the notebook beside the lab note at p: nb.lab.md's nb.ipynb.
func NotebookPath(p string) string { return p[:len(p)-len(lab.Ext)] + ".ipynb" }

// Import writes the lab note beside the notebook at p (LabPath), its front matter naming the
// notebook's kernel, and each code cell's outputs under the note's lab.OutDir; a later cell with the
// same language and code as an earlier one shares its folder, and keeps the earlier one's outputs.
// When the lab note exists, nothing is written: it is the note to open, as it is.
func Import(p string) (labPath string, err error) {
	labPath = LabPath(p)
	if _, err := os.Stat(labPath); err == nil {
		return labPath, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	var nb notebook
	if err := json.Unmarshal(data, &nb); err != nil {
		return "", fmt.Errorf("ipynb: %s is not a notebook: %w", filepath.Base(p), err)
	}
	if nb.Format != 4 {
		return "", fmt.Errorf("ipynb: %s is nbformat %d; this reads 4", filepath.Base(p), nb.Format)
	}
	// the fence's language, read as every reader of the note reads a fence's (its first word,
	// lower-cased), so the key written here is the key the note's own parse gives: "Wolfram Language"
	// is a wolfram fence
	lang := lab.FenceLang(nb.Metadata.Kernelspec.Language)
	if lang == "" {
		lang = lab.FenceLang(nb.Metadata.LanguageInfo.Name)
	}
	if lang == "" {
		lang = "python"
	}
	var b strings.Builder
	if k := nb.Metadata.Kernelspec.Name; k != "" {
		b.WriteString("---\nkernel: " + yamlString(k) + "\n---\n\n")
	}
	outDir := filepath.Join(filepath.Dir(labPath), filepath.FromSlash(lab.OutDir(labPath)))
	written := map[string]bool{}
	for _, c := range nb.Cells {
		src := source(c.Source)
		switch c.Type {
		case "markdown":
			b.WriteString(strings.TrimRight(src, "\n") + "\n\n")
		case "raw":
			b.WriteString(fence(RawLang, src))
		case "code":
			b.WriteString(fence(lang, src))
			key := lab.CellKey(lang, src)
			if written[key] {
				continue
			}
			written[key] = true
			var outs []json.RawMessage
			_ = json.Unmarshal(c.Outputs, &outs)
			var run outputs.Run
			for _, o := range outs {
				typ, content := output(o)
				run.Add(typ, content)
			}
			if files := run.Files(); len(files) > 0 {
				if err := outputs.Commit(filepath.Join(outDir, key), files); err != nil {
					return "", err
				}
			}
		}
	}
	f, err := os.OpenFile(labPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return labPath, nil // written meanwhile: that one is the note
		}
		return "", err
	}
	if _, err := f.WriteString(strings.TrimRight(b.String(), "\n") + "\n"); err != nil {
		f.Close()
		return "", err
	}
	return labPath, f.Close()
}

// source is a cell's source: a string, or its lines.
func source(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var lines []string
	_ = json.Unmarshal(raw, &lines)
	return strings.Join(lines, "")
}

// output is an .ipynb output as the message type and content a run's iopub message has: a stream's
// text joined from its lines; a bundle's values are read as they are (outputs.FromBundle takes lines).
func output(raw json.RawMessage) (string, json.RawMessage) {
	var o struct {
		Type string          `json:"output_type"`
		Name string          `json:"name"`
		Text json.RawMessage `json:"text"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return "", nil
	}
	if o.Type == "stream" {
		content, _ := json.Marshal(map[string]string{"name": o.Name, "text": source(o.Text)})
		return o.Type, content
	}
	return o.Type, raw
}

var backticks = regexp.MustCompile("`{3,}")

// fence is src in a fence of lang: its backticks more than any run src holds, so the fence holds it.
func fence(lang, src string) string {
	n := 3
	for _, m := range backticks.FindAllString(src, -1) {
		n = max(n, len(m)+1)
	}
	f := strings.Repeat("`", n)
	body := strings.TrimRight(src, "\n")
	if body != "" {
		body += "\n"
	}
	return f + lang + "\n" + body + f + "\n\n"
}

// yamlString is s as a YAML scalar: plain when it is a simple name, else double-quoted (JSON's
// quoting is YAML's).
func yamlString(s string) string {
	if regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`).MatchString(s) {
		return s
	}
	q, _ := json.Marshal(s)
	return string(q)
}

// Kernel is what Export writes into a notebook's metadata: the kernelspec the note runs in.
type Kernel struct {
	Name, DisplayName, Language string
}

// Export writes the notebook beside the lab note at p (NotebookPath), never over an existing file:
// the note's text between cells as markdown cells, its fences in k.Language as code cells with the
// outputs their files hold, and its raw fences as raw cells. Its other fences (a diagram, a chart,
// a table) stay in the markdown, as the note shows them.
func Export(p string, k Kernel) (string, error) {
	if !lab.Is(p) {
		return "", fmt.Errorf("ipynb: %s is not a lab note (%s)", filepath.Base(p), lab.Ext)
	}
	out := NotebookPath(p)
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	text := string(data)
	note := cells.Read(text)
	runs := append(note.Cells(k.Language), note.Cells(RawLang)...)
	slices.SortFunc(runs, func(a, b cells.Cell) int { return a.Start - b.Start })

	nb := notebook{Format: 4, Minor: 5, Cells: []cell{}}
	nb.Metadata.Kernelspec.Name, nb.Metadata.Kernelspec.DisplayName, nb.Metadata.Kernelspec.Language = k.Name, k.DisplayName, k.Language
	nb.Metadata.LanguageInfo.Name = k.Language
	n := 0
	id := func() string { n++; return fmt.Sprintf("cell-%d", n) }
	markdown := func(s string) {
		if s = strings.Trim(s, "\n"); strings.TrimSpace(s) != "" {
			nb.Cells = append(nb.Cells, cell{Type: "markdown", ID: id(), Metadata: map[string]any{}, Source: lines(s)})
		}
	}
	dir := filepath.Dir(p)
	at := note.Body
	for _, c := range runs {
		markdown(text[at:c.Start])
		at = c.End
		code := strings.TrimRight(c.Code, "\n")
		if c.Lang == RawLang {
			nb.Cells = append(nb.Cells, cell{Type: "raw", ID: id(), Metadata: map[string]any{}, Source: lines(code)})
			continue
		}
		outs := []json.RawMessage{}
		for _, f := range lab.Outputs(os.DirFS(dir), lab.OutDir(p), c.Key()) {
			if o, ok := fileOutput(f, dir); ok {
				outs = append(outs, o)
			}
		}
		list, err := json.Marshal(outs)
		if err != nil {
			return "", err
		}
		nb.Cells = append(nb.Cells, cell{Type: "code", ID: id(), Metadata: map[string]any{}, Source: lines(code), Outputs: list, ExecutionCount: json.RawMessage("null")})
	}
	markdown(text[min(at, len(text)):])

	body, err := json.MarshalIndent(nb, "", " ")
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("ipynb: %s exists: export never writes over it", filepath.Base(out))
		}
		return "", err
	}
	if _, err := f.Write(append(body, '\n')); err != nil {
		f.Close()
		return "", err
	}
	return out, f.Close()
}

// lines is s as nbformat keeps text: its lines, each but the last with its newline.
func lines(s string) json.RawMessage {
	parts := strings.SplitAfter(s, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if parts == nil {
		parts = []string{}
	}
	raw, _ := json.Marshal(parts)
	return raw
}

// fileOutput is an output file (a slash path in the note's folder dir) as an .ipynb output: text as
// stdout, an image or SVG as display_data, a chart as Vega-Lite, a CSV as its text.
func fileOutput(p, dir string) (json.RawMessage, bool) {
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
	if err != nil {
		return nil, false
	}
	display := func(mime string, value any) (json.RawMessage, bool) {
		raw, err := json.Marshal(map[string]any{"output_type": "display_data", "data": map[string]any{mime: value}, "metadata": map[string]any{}})
		return raw, err == nil
	}
	name := strings.ToLower(path.Base(p))
	switch {
	case strings.HasSuffix(name, ".chart.json"):
		var spec any
		if json.Unmarshal(data, &spec) != nil {
			return nil, false
		}
		return display(outputs.MIMEVegaLite5, spec)
	case strings.HasSuffix(name, ".png"):
		return display(outputs.MIMEPNG, base64.StdEncoding.EncodeToString(data))
	case strings.HasSuffix(name, ".jpg"), strings.HasSuffix(name, ".jpeg"):
		return display(outputs.MIMEJPEG, base64.StdEncoding.EncodeToString(data))
	case strings.HasSuffix(name, ".svg"):
		return display(outputs.MIMESVG, json.RawMessage(lines(string(data))))
	case strings.HasSuffix(name, ".csv"):
		return display(outputs.MIMEText, json.RawMessage(lines(string(data))))
	case strings.HasSuffix(name, ".txt"):
		raw, err := json.Marshal(map[string]any{"output_type": "stream", "name": "stdout", "text": json.RawMessage(lines(string(data)))})
		return raw, err == nil
	}
	return nil, false
}
