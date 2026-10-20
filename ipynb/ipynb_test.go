package ipynb

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodoc/core/lab"

	"github.com/yongjohnlee80/autodoc-jupyter/cells"
)

var png = base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nplot"))

// nb is a notebook as Jupyter writes one: markdown, code with a stream and a plot, a raw cell, a
// code cell holding a fence of its own, and a second cell with the first's code.
var nb = `{
 "cells": [
  {"cell_type": "markdown", "id": "a", "metadata": {}, "source": ["# Sales\n", "\n", "A *plot*."]},
  {"cell_type": "code", "execution_count": 1, "id": "b", "metadata": {}, "outputs": [
    {"output_type": "stream", "name": "stdout", "text": ["1\n", "2\n"]},
    {"output_type": "display_data", "data": {"image/png": "` + png + `", "text/plain": ["<Figure>"]}, "metadata": {}}
   ], "source": ["x = [1, 2]\n", "for v in x:\n", "    print(v)"]},
  {"cell_type": "raw", "id": "c", "metadata": {}, "source": "\\LaTeX"},
  {"cell_type": "code", "execution_count": 2, "id": "d", "metadata": {}, "outputs": [], "source": "s = '''\n` + "```" + `\n'''"},
  {"cell_type": "code", "execution_count": 3, "id": "e", "metadata": {}, "outputs": [{"output_type": "stream", "name": "stdout", "text": "again\n"}], "source": ["x = [1, 2]\n", "for v in x:\n", "    print(v)"]}
 ],
 "metadata": {"kernelspec": {"display_name": "Python 3", "language": "python", "name": "python3"}, "language_info": {"name": "python"}},
 "nbformat": 4,
 "nbformat_minor": 5
}`

func writeNotebook(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sales.ipynb")
	if err := os.WriteFile(p, []byte(nb), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Import writes the lab note beside the notebook: its kernel in the front matter, markdown as itself,
// code as fences in the kernel's language (a longer fence around code holding one), raw as a raw
// fence; and each code cell's outputs under the cell's key, which is the key the note's own parse
// gives (AutoDoc finds them). A repeated cell keeps the first's outputs.
func TestImportWritesTheLabNoteAndItsOutputs(t *testing.T) {
	p := writeNotebook(t)
	labPath, err := Import(p)
	if err != nil {
		t.Fatal(err)
	}
	if labPath != strings.TrimSuffix(p, ".ipynb")+".lab.md" {
		t.Fatalf("lab note %s", labPath)
	}
	data, _ := os.ReadFile(labPath)
	text := string(data)
	for _, want := range []string{"---\nkernel: python3\n---\n\n# Sales\n\nA *plot*.\n\n```python\nx = [1, 2]\n", "```raw\n\\LaTeX\n```", "````python\ns = '''\n```\n'''\n````"} {
		if !strings.Contains(text, want) {
			t.Errorf("the note lacks %q:\n%s", want, text)
		}
	}
	note := cells.Read(text)
	cs := note.Cells("python")
	if note.Kernel != "python3" || len(cs) != 3 {
		t.Fatalf("kernel %q, %d cells", note.Kernel, len(cs))
	}
	dir := filepath.Dir(labPath)
	got := lab.Outputs(os.DirFS(dir), lab.OutDir(labPath), cs[0].Key())
	if len(got) != 2 || !strings.HasSuffix(got[0], "/01.txt") || !strings.HasSuffix(got[1], "/02.png") {
		t.Fatalf("the first cell's outputs: %v", got)
	}
	if txt, _ := os.ReadFile(filepath.Join(dir, got[0])); string(txt) != "1\n2\n" {
		t.Errorf("its stream %q", txt)
	}
	if img, _ := os.ReadFile(filepath.Join(dir, got[1])); string(img) != "\x89PNG\r\n\x1a\nplot" {
		t.Errorf("its plot %q", img)
	}
	if cs[2].Key() != cs[0].Key() {
		t.Fatal("the repeated cell has another key")
	}
	if got := lab.Outputs(os.DirFS(dir), lab.OutDir(labPath), cs[1].Key()); got != nil {
		t.Errorf("a cell with no outputs has %v", got)
	}
}

// An existing lab note is the note to open: Import writes nothing, and never over it.
func TestImportNeverWritesOverALabNote(t *testing.T) {
	p := writeNotebook(t)
	labPath := LabPath(p)
	if err := os.WriteFile(labPath, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Import(p)
	if err != nil || got != labPath {
		t.Fatalf("Import: %s %v", got, err)
	}
	if data, _ := os.ReadFile(labPath); string(data) != "mine\n" {
		t.Errorf("the note was overwritten: %q", data)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(p), "sales.lab")); !os.IsNotExist(err) {
		t.Errorf("outputs were written: %v", err)
	}
}

// A notebook round trip (ADR §5.7): import, then export, gives back the same cells, in order, and
// the same outputs: the stream's text and the plot's bytes. Export never writes over a notebook.
func TestANotebookRoundTrips(t *testing.T) {
	p := writeNotebook(t)
	labPath, err := Import(p)
	if err != nil {
		t.Fatal(err)
	}
	k := Kernel{Name: "python3", DisplayName: "Python 3", Language: "python"}
	if _, err := Export(labPath, k); err == nil || !strings.Contains(err.Error(), "never writes over") {
		t.Fatalf("Export over the notebook: %v", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	out, err := Export(labPath, k)
	if err != nil || out != p {
		t.Fatalf("Export: %s %v", out, err)
	}
	var before, after notebook
	if err := json.Unmarshal([]byte(nb), &before); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if after.Format != 4 || after.Metadata.Kernelspec.Name != "python3" || after.Metadata.Kernelspec.Language != "python" {
		t.Errorf("metadata %+v", after.Metadata)
	}
	summary := func(n notebook) []string {
		var out []string
		for _, c := range n.Cells {
			s := c.Type + ":" + strings.TrimRight(source(c.Source), "\n")
			var outs []json.RawMessage
			_ = json.Unmarshal(c.Outputs, &outs)
			for _, o := range outs {
				var v struct {
					Type string          `json:"output_type"`
					Text json.RawMessage `json:"text"`
					Data map[string]json.RawMessage
				}
				_ = json.Unmarshal(o, &v)
				switch {
				case v.Type == "stream":
					s += " | stream " + source(v.Text)
				case v.Data["image/png"] != nil:
					s += " | png " + source(v.Data["image/png"])
				}
			}
			out = append(out, s)
		}
		return out
	}
	// the repeated cell keeps the first's outputs (the accepted key model, ADR §R8): its own "again"
	// is not kept
	want := summary(before)
	want[4] = strings.Replace(want[4], " | stream again\n", " | stream 1\n2\n | png "+png, 1)
	if got := summary(after); !slices.Equal(got, want) {
		t.Errorf("round trip:\n got %q\nwant %q", got, want)
	}
	for _, c := range after.Cells {
		raw, _ := json.Marshal(c)
		hasCount, hasOutputs := strings.Contains(string(raw), `"execution_count":null`), strings.Contains(string(raw), `"outputs":`)
		if (c.Type == "code") != hasCount || (c.Type == "code") != hasOutputs {
			t.Errorf("a %s cell: %s", c.Type, raw)
		}
	}
}

// Export reads only a lab note.
func TestExportReadsALabNoteOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "plain.md")
	if err := os.WriteFile(p, []byte("# x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(p, Kernel{Language: "python"}); err == nil {
		t.Error("a plain note was exported")
	}
}

// A kernelspec's language of more than one word is a fence of its first word, and the outputs are
// keyed as the written note's own parse keys that fence: AutoDoc finds them, and an export, under the
// same kernelspec, gives them back. (Lector's review of #1, r2.)
func TestAMultiWordLanguageRoundTrips(t *testing.T) {
	p := filepath.Join(t.TempDir(), "wl.ipynb")
	src := `{"cells": [{"cell_type": "code", "execution_count": 1, "metadata": {}, "outputs": [{"output_type": "stream", "name": "stdout", "text": "2\n"}], "source": "1 + 1"}], "metadata": {"kernelspec": {"name": "wolframlanguage14", "display_name": "Wolfram Language 14", "language": "Wolfram Language"}}, "nbformat": 4, "nbformat_minor": 5}`
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	labPath, err := Import(p)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(labPath)
	if !strings.Contains(string(data), "```wolfram\n1 + 1\n```") {
		t.Fatalf("the fence:\n%s", data)
	}
	cs := cells.Read(string(data)).Cells("Wolfram Language")
	if len(cs) != 1 {
		t.Fatalf("cells %+v", cs)
	}
	dir := filepath.Dir(labPath)
	got := lab.Outputs(os.DirFS(dir), lab.OutDir(labPath), cs[0].Key())
	if len(got) != 1 {
		t.Fatalf("the cell's outputs under its parsed key: %v", got)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(labPath, Kernel{Name: "wolframlanguage14", DisplayName: "Wolfram Language 14", Language: "Wolfram Language"}); err != nil {
		t.Fatal(err)
	}
	var nb notebook
	out, _ := os.ReadFile(p)
	if err := json.Unmarshal(out, &nb); err != nil || len(nb.Cells) != 1 || nb.Cells[0].Type != "code" ||
		!strings.Contains(string(nb.Cells[0].Outputs), `"2\n"`) {
		t.Errorf("the export: %s (%v)", out, err)
	}
}
