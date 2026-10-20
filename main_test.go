package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodoc/core/lab"
	"github.com/yongjohnlee80/autodoc/core/manifests"
	"github.com/yongjohnlee80/autodoc/plugin"

	"github.com/yongjohnlee80/autodoc-jupyter/cells"
	"github.com/yongjohnlee80/autodoc-jupyter/internal/fakekernel"
	"github.com/yongjohnlee80/autodoc-jupyter/kernel"
)

// The card's kernel is this test binary, run as the fake kernel (internal/fakekernel).
func TestMain(m *testing.M) {
	fakekernel.Dispatch()
	os.Exit(m.Run())
}

// recorder is AutoDoc as the card's tests see it: the frames, tables, rows and calls it was sent.
type recorder struct {
	mu     sync.Mutex
	frame  *plugin.Frame
	tables []plugin.TableOpen
	rows   []string // "id more n"
	calls  [][]string
}

func (r *recorder) Frame(f *plugin.Frame) error { r.mu.Lock(); r.frame = f; r.mu.Unlock(); return nil }
func (r *recorder) Table(t plugin.TableOpen) error {
	r.mu.Lock()
	r.tables = append(r.tables, t)
	r.mu.Unlock()
	return nil
}
func (r *recorder) RowsKinds(id string, rows [][]string, _ []string, more bool) error {
	r.mu.Lock()
	r.rows = append(r.rows, strings.Join([]string{id, map[bool]string{true: "more", false: "end"}[more], strings.Repeat("r", len(rows))}, " "))
	r.mu.Unlock()
	return nil
}
func (r *recorder) Call(id string, args ...string) (bool, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{id}, args...))
	r.mu.Unlock()
	return true, nil
}
func (r *recorder) Notify(plugin.Notice) error { return nil }

// text is the last frame's text.
func (r *recorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frame == nil {
		return ""
	}
	var b strings.Builder
	for _, row := range r.frame.Rows() {
		for _, run := range row {
			b.WriteString(run.Text)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// card is a card on the recorder, its kernels the fake kernel in mode (language "fake", kernel name
// "fake"), stopped when the test ends.
func card(t *testing.T, mode fakekernel.Mode) (*labCard, *recorder) {
	t.Helper()
	spec, err := fakekernel.Spec(mode, "fake")
	if err != nil {
		t.Fatal(err)
	}
	c := newLab(t.TempDir())
	c.specs = func() []kernel.Spec { return []kernel.Spec{spec} }
	r := &recorder{}
	c.open(r, plugin.Open{Protocol: plugin.Protocol, Width: 70, Height: 20})
	t.Cleanup(c.Close)
	return c, r
}

// note writes a lab note's text in a folder of its own, and hands it to the card with the cursor at
// the first byte of at.
func note(t *testing.T, c *labCard, name, text, at string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	c.Document(plugin.Document{Path: p, Text: text, CursorByte: strings.Index(text, at)})
	return p
}

// settled waits until the card's run has ended, and returns its status.
func settled(t *testing.T, c *labCard) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		c.mu.Lock()
		running, status := c.running, c.status
		c.mu.Unlock()
		if !running {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run never ended: %s", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// outputsOf are the files a cell's run left, by name, read.
func outputsOf(t *testing.T, notePath, code string) map[string]string {
	t.Helper()
	dir := filepath.Dir(notePath)
	got := map[string]string{}
	for _, p := range lab.Outputs(os.DirFS(dir), lab.OutDir(notePath), lab.CellKey("fake", code)) {
		data, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		got[filepath.Base(p)] = string(data)
	}
	return got
}

const labText = "---\nkernel: fake\n---\n\n# Lab\n\n```fake\nprint\n```\n\n```mermaid\nprint\n```\n\n```fake\npng\n```\n"

// Run runs the cell under the cursor, and its outputs are kept beside the note under the cell's key,
// where AutoDoc finds them; the card shows them. A fence of another language is never run.
func TestRunKeepsACellsOutputsBesideTheNote(t *testing.T) {
	c, r := card(t, fakekernel.Signal)
	p := note(t, c, "plan.lab.md", labText, "print\n```\n\n```mermaid")
	c.Command("run")
	if s := settled(t, c); !strings.Contains(s, "ran 1 cell") {
		t.Fatalf("status %q", s)
	}
	if got := outputsOf(t, p, "print"); got["01.txt"] != "1\n" || len(got) != 1 {
		t.Errorf("the cell's outputs %q", got)
	}
	if !strings.Contains(r.text(), "Fake") || !strings.Contains(r.text(), "plan.lab.md") {
		t.Errorf("the card:\n%s", r.text())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(p), "plan.lab", "out", lab.CellKey("mermaid", "print"))); !os.IsNotExist(err) {
		t.Error("the mermaid fence ran")
	}
}

// Run all runs every cell in order and removes the outputs of code no cell has any more.
func TestRunAllRunsEveryCellAndPrunes(t *testing.T) {
	c, _ := card(t, fakekernel.Signal)
	p := note(t, c, "plan.lab.md", labText, "# Lab")
	stale := filepath.Join(filepath.Dir(p), "plan.lab", "out", "0123456789abcdef")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	c.Command("all")
	if s := settled(t, c); !strings.Contains(s, "ran 2 cells") {
		t.Fatalf("status %q", s)
	}
	if got := outputsOf(t, p, "png"); got["01.png"] != string(fakekernel.PNG) {
		t.Errorf("the plot cell's outputs %q", got)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the stale outputs were kept")
	}
}

// A DataFrame is kept as CSV and shown in AutoDoc's table panel, its kinds with it; a fetch past its
// rows, or of a table gone, ends.
func TestADataFrameReachesTheTablePanel(t *testing.T) {
	c, r := card(t, fakekernel.Signal)
	p := note(t, c, "df.lab.md", "```fake\ndf\n```\n", "df")
	c.Command("run")
	settled(t, c)
	if got := outputsOf(t, p, "df"); got["01.csv"] != "index,a,b\n0,1,x\n1,2,\n" {
		t.Errorf("the CSV %q", got)
	}
	r.mu.Lock()
	tables := slices.Clone(r.tables)
	r.mu.Unlock()
	if len(tables) != 1 || tables[0].Title != "DataFrame" || !slices.Equal(tables[0].Fields, []string{"index", "a", "b"}) ||
		len(tables[0].Rows) != 2 || !slices.Equal(tables[0].Kinds, []string{"nnt", "nnz"}) || tables[0].More {
		t.Fatalf("the table %+v", tables)
	}
	c.FetchRows(plugin.TableFetch{ID: "df-404", N: 10})
	c.TableClosed(tables[0].ID)
	c.FetchRows(plugin.TableFetch{ID: tables[0].ID, N: 10})
	if r.mu.Lock(); !slices.Equal(r.rows, []string{"df-404 end ", tables[0].ID + " end "}) {
		t.Errorf("fetches %q", r.rows)
	}
	r.mu.Unlock()
}

// Interrupt stops a run while it runs: the card takes the command, the kernel's KeyboardInterrupt is
// kept as the cell's output, and the status says so.
func TestInterruptStopsTheRun(t *testing.T) {
	c, r := card(t, fakekernel.Signal)
	p := note(t, c, "slow.lab.md", "```fake\nsleep\n```\n", "sleep")
	c.Command("run")
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(r.text(), "sleeping") {
		if time.Now().After(deadline) {
			t.Fatalf("the run never started:\n%s", r.text())
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.Command("interrupt")
	if s := settled(t, c); !strings.Contains(s, "KeyboardInterrupt") {
		t.Errorf("status %q", s)
	}
	if got := outputsOf(t, p, "sleep"); got["01.txt"] != "sleeping\n" || !strings.Contains(got["02.txt"], "KeyboardInterrupt") {
		t.Errorf("outputs %q", got)
	}
}

// What the card cannot run it says, and runs nothing: no note, a plain note, the cursor outside every
// cell, a kernel that is not installed.
func TestTheCardSaysWhyItCannotRun(t *testing.T) {
	c, _ := card(t, fakekernel.Signal)
	status := func() string { c.mu.Lock(); defer c.mu.Unlock(); return c.status }
	c.Command("run")
	if s := status(); s != "no note is open" {
		t.Errorf("no note: %q", s)
	}
	note(t, c, "plain.md", "```fake\nprint\n```\n", "print")
	if c.Command("run"); !strings.Contains(status(), "not a lab note") {
		t.Errorf("a plain note: %q", status())
	}
	note(t, c, "x.lab.md", labText, "# Lab")
	if c.Command("run"); status() != "the cursor is in no fake cell" {
		t.Errorf("outside a cell: %q", status())
	}
	note(t, c, "y.lab.md", "---\nkernel: julia\n---\n```fake\nprint\n```\n", "print")
	if c.Command("run"); !strings.Contains(status(), `"julia" is not installed`) {
		t.Errorf("a kernel not installed: %q", status())
	}
}

// A selection runs on its own, its outputs shown and not kept: they belong to no cell.
func TestASelectionRunsWithoutKeepingOutputs(t *testing.T) {
	c, r := card(t, fakekernel.Signal)
	text := "```fake\nselected words\n```\n"
	p := filepath.Join(t.TempDir(), "sel.lab.md")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	at := strings.Index(text, "selected")
	c.Document(plugin.Document{Path: p, Text: text, CursorByte: at, SelectionBytes: [][2]int{{at, at + len("selected")}}})
	c.Command("run")
	settled(t, c)
	if !strings.Contains(r.text(), "selected") {
		t.Errorf("the card:\n%s", r.text())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(p), "sel.lab")); !os.IsNotExist(err) {
		t.Error("a selection's outputs were kept")
	}
}

// An .ipynb handed to import (the [open] hook) becomes a lab note beside it, which the card asks
// AutoDoc to open; export writes it back as a notebook beside the note.
func TestImportOpensANotebookAsALabNoteAndExportWritesOne(t *testing.T) {
	c, r := card(t, fakekernel.Signal)
	dir := t.TempDir()
	nb := filepath.Join(dir, "nb.ipynb")
	src := `{"cells": [{"cell_type": "code", "metadata": {}, "outputs": [], "source": "print"}], "metadata": {"kernelspec": {"name": "fake", "language": "fake"}}, "nbformat": 4, "nbformat_minor": 5}`
	if err := os.WriteFile(nb, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	c.CommandArgs("import", []string{nb})
	labPath := filepath.Join(dir, "nb.lab.md")
	if r.mu.Lock(); len(r.calls) != 1 || !slices.Equal(r.calls[0], []string{"file.open_path", labPath}) {
		t.Fatalf("calls %q", r.calls)
	}
	r.mu.Unlock()
	data, err := os.ReadFile(labPath)
	if err != nil || len(cells.Read(string(data)).Cells("fake")) != 1 {
		t.Fatalf("the lab note: %q %v", data, err)
	}
	if err := os.Remove(nb); err != nil {
		t.Fatal(err)
	}
	c.Document(plugin.Document{Path: labPath, Text: string(data)})
	c.Command("export")
	if _, err := os.Stat(nb); err != nil {
		c.mu.Lock()
		t.Errorf("export: %v (%s)", err, c.status)
		c.mu.Unlock()
	}
}

// The manifest is one AutoDoc accepts at protocol 9: it opens .ipynb files through import, and every
// command it declares is one the card takes, by the letter the card's own keys give it.
func TestTheManifestIsOneAutoDocAccepts(t *testing.T) {
	m, err := manifests.ReadPlugin(os.DirFS("."))
	if err != nil {
		t.Fatal(err)
	}
	if m.Protocol != 9 || !slices.Equal(m.Open.Extensions, []string{".ipynb"}) || m.Open.Command != "import" ||
		!slices.Contains(m.Declares(), "opens .ipynb files") || m.Modal() {
		t.Errorf("manifest %+v; declares %q", m, m.Declares())
	}
	for _, cmd := range m.Commands {
		if cmd.Key != "" && keys[cmd.Key] != cmd.ID {
			t.Errorf("%s's key %q is the card's %q", cmd.ID, cmd.Key, keys[cmd.Key])
		}
	}
	for k, id := range keys {
		if !slices.ContainsFunc(m.Commands, func(c manifests.Command) bool { return c.ID == id && c.Key == k }) {
			t.Errorf("the card's key %q for %s is not the manifest's", k, id)
		}
	}
}

// Against a real ipykernel with pandas and matplotlib where the machine has them (VM43's gate image):
// a plot is kept as a PNG (ADR §5.6) and a DataFrame as CSV and a table (§5.8), pandas' table schema
// switched on when the kernel starts.
func TestARealPythonCellKeepsAPlotAndADataFrame(t *testing.T) {
	var spec *kernel.Spec
	for _, s := range kernel.Specs() {
		if s.Language == "python" {
			spec = &s
			break
		}
	}
	if spec == nil {
		if os.Getenv("AUTODOC_JUPYTER_REQUIRE_KERNEL") != "" {
			t.Fatal("no Python kernelspec, and AUTODOC_JUPYTER_REQUIRE_KERNEL says one is installed (CI)")
		}
		t.Skip("no Python kernelspec on this machine (ipykernel is not installed)")
	}
	c := newLab(t.TempDir())
	c.specs = func() []kernel.Spec { return []kernel.Spec{*spec} }
	r := &recorder{}
	c.open(r, plugin.Open{Protocol: plugin.Protocol, Width: 70, Height: 20})
	t.Cleanup(c.Close)
	text := "```python\nimport matplotlib.pyplot as plt\nplt.plot([1, 2, 3])\nplt.show()\n```\n\n```python\nimport pandas as pd\npd.DataFrame({'a': [1, 2], 'b': ['x', None]})\n```\n"
	p := filepath.Join(t.TempDir(), "real.lab.md")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	c.Document(plugin.Document{Path: p, Text: text})
	c.Command("all")
	if s := settled(t, c); !strings.Contains(s, "ran 2 cells") {
		t.Fatalf("status %q\n%s", s, r.text())
	}
	n := cells.Read(text).Cells("python")
	dir := filepath.Dir(p)
	plot := lab.Outputs(os.DirFS(dir), lab.OutDir(p), n[0].Key())
	if len(plot) != 1 || !strings.HasSuffix(plot[0], ".png") {
		t.Errorf("the plot cell's outputs %q", plot)
	}
	frame := lab.Outputs(os.DirFS(dir), lab.OutDir(p), n[1].Key())
	if len(frame) != 1 || !strings.HasSuffix(frame[0], ".csv") {
		t.Fatalf("the DataFrame cell's outputs %q", frame)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, frame[0])); string(data) != "index,a,b\n0,1,x\n1,2,\n" {
		t.Errorf("the CSV %q", data)
	}
	if r.mu.Lock(); len(r.tables) != 1 || !slices.Equal(r.tables[0].Fields, []string{"index", "a", "b"}) {
		t.Errorf("tables %+v", r.tables)
	}
	r.mu.Unlock()
}
