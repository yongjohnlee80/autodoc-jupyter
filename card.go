package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yongjohnlee80/autodoc/core/lab"
	"github.com/yongjohnlee80/autodoc/plugin"

	"github.com/yongjohnlee80/autodoc-jupyter/cells"
	"github.com/yongjohnlee80/autodoc-jupyter/ipynb"
	"github.com/yongjohnlee80/autodoc-jupyter/kernel"
	"github.com/yongjohnlee80/autodoc-jupyter/outputs"
)

// host is the AutoDoc the card speaks to: *plugin.Peer, or a test's recorder.
type host interface {
	Frame(*plugin.Frame) error
	Table(plugin.TableOpen) error
	RowsKinds(id string, rows [][]string, kinds []string, more bool) error
	Call(id string, args ...string) (bool, error)
	Notify(plugin.Notice) error
}

// pandasSetup runs silently when a Python kernel starts: pandas then sends a DataFrame's table schema
// (application/vnd.dataresource+json), which the card keeps as CSV and shows as a table.
const pandasSetup = "try:\n    import pandas as _autodoc_pd\n    _autodoc_pd.set_option('display.html.table_schema', True)\n    del _autodoc_pd\nexcept ImportError:\n    pass\n"

// tablePage is how many rows of a DataFrame go in one message.
const tablePage = 200

// labCard is the plugin: a card that runs the open lab note's cells in a Jupyter kernel, one kernel
// per note, and keeps their outputs beside the note. A run goes on a goroutine of its own, so the
// card still takes Interrupt while it runs.
type labCard struct {
	plugin.Base
	connDir string
	specs   func() []kernel.Spec                                                       // kernel.Specs; a test's
	start   func(context.Context, kernel.Spec, string, string) (*kernel.Client, error) // kernel.Start

	mu      sync.Mutex
	peer    host
	w, h    int
	theme   plugin.Theme
	doc     plugin.Document
	kernels map[string]*kernel.Client // by lab note path
	chosen  map[string]string         // a note's kernel picked with Choose kernel…, by note path
	running bool
	status  string   // what the card says it is doing, or did last
	log     []string // the last run's outputs as lines
	picking []kernel.Spec
	pick    int
	tables  map[string]*outputs.Table
	sent    map[string]int // rows of each table sent so far
	tableN  int
	runs    sync.WaitGroup
}

func newLab(connDir string) *labCard {
	return &labCard{connDir: connDir, specs: kernel.Specs, start: kernel.Start,
		kernels: map[string]*kernel.Client{}, chosen: map[string]string{},
		tables: map[string]*outputs.Table{}, sent: map[string]int{}, status: "ready"}
}

func (c *labCard) Open(p *plugin.Peer, o plugin.Open) { c.open(p, o) }

func (c *labCard) open(p host, o plugin.Open) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peer, c.w, c.h, c.theme = p, o.Width, o.Height, o.Theme
	c.drawLocked()
}

func (c *labCard) Resize(w, h int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.w, c.h = w, h
	c.drawLocked()
}

func (c *labCard) Theme(t plugin.Theme) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.theme = t
	c.drawLocked()
}

// Document is the open note, after each pause in editing, and as it stood when a command was chosen.
func (c *labCard) Document(d plugin.Document) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.doc = d
	c.drawLocked()
}

// Close stops every kernel; a run in progress returns with its kernel's exit.
func (c *labCard) Close() {
	c.mu.Lock()
	ks := c.kernels
	c.kernels = map[string]*kernel.Client{}
	c.mu.Unlock()
	for _, k := range ks {
		k.Shutdown()
	}
	done := make(chan struct{})
	go func() { c.runs.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond): // the host waits two seconds
	}
	os.RemoveAll(c.connDir)
}

func (c *labCard) Command(id string) { c.CommandArgs(id, nil) }

// CommandArgs runs one of the manifest's commands; import takes the .ipynb's path (the [open] hook).
func (c *labCard) CommandArgs(id string, args []string) {
	switch id {
	case "run":
		c.run(false)
	case "all":
		c.run(true)
	case "interrupt":
		c.interrupt()
	case "restart":
		c.restart()
	case "kernel":
		c.choose()
	case "export":
		c.export()
	case "import":
		if len(args) == 0 {
			c.say("open an .ipynb with File › Open file…: it comes here and opens as a lab note")
			return
		}
		c.importNotebook(args[0])
	}
}

// keys are the commands' letters while the card has the keys, as the manifest gives them.
var keys = map[string]string{"r": "run", "a": "all", "x": "interrupt", "s": "restart", "k": "kernel", "e": "export"}

func (c *labCard) Key(k plugin.Key) {
	c.mu.Lock()
	if c.picking != nil {
		switch {
		case k.Key == "Up":
			c.pick = max(c.pick-1, 0)
		case k.Key == "Down":
			c.pick = min(c.pick+1, len(c.picking)-1)
		case k.Key == "Enter":
			c.chosen[c.doc.Path] = c.picking[c.pick].Name
			c.status = "kernel: " + display(c.picking[c.pick]) + " (add kernel: " + c.picking[c.pick].Name + " to the note's front matter to keep it)"
			c.picking = nil
		case k.Key == "q":
			c.picking = nil
		}
		c.drawLocked()
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	if id, ok := keys[k.Key]; ok && !k.Ctrl && !k.Alt {
		c.Command(id)
	}
}

// say sets the card's status line.
func (c *labCard) say(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = s
	c.drawLocked()
}

// note is the open note when it is a lab note the card can run, and its kernel's spec; otherwise
// why not.
func (c *labCard) noteLocked() (plugin.Document, cells.Note, kernel.Spec, error) {
	d := c.doc
	switch {
	case d.Path == "":
		return d, cells.Note{}, kernel.Spec{}, errors.New("no note is open")
	case !lab.Is(d.Path):
		return d, cells.Note{}, kernel.Spec{}, fmt.Errorf("%s is not a lab note: cells run in a %s note", filepath.Base(d.Path), lab.Ext)
	case d.TooLarge:
		return d, cells.Note{}, kernel.Spec{}, errors.New("the note is too large to be sent to the plugin")
	}
	n := cells.Read(d.Text)
	spec, err := c.specLocked(d.Path, n)
	return d, n, spec, err
}

// specLocked is the note's kernel: the one picked in the card, else its front matter's, else the
// first Python kernel installed, else the first of any.
func (c *labCard) specLocked(path string, n cells.Note) (kernel.Spec, error) {
	specs := c.specs()
	name := c.chosen[path]
	if name == "" {
		name = n.Kernel
	}
	if name != "" {
		if i := slices.IndexFunc(specs, func(s kernel.Spec) bool { return s.Name == name }); i >= 0 {
			return specs[i], nil
		}
		return kernel.Spec{}, fmt.Errorf("the kernel %q is not installed (k chooses another)", name)
	}
	if i := slices.IndexFunc(specs, func(s kernel.Spec) bool { return s.Language == "python" }); i >= 0 {
		return specs[i], nil
	}
	if len(specs) > 0 {
		return specs[0], nil
	}
	return kernel.Spec{}, errors.New("no Jupyter kernel is installed: pip install ipykernel installs Python's")
}

// run runs the cell under the cursor (or the selection, its outputs shown but not kept), or every
// cell in order, stopping at the first that fails; Run all then removes the outputs no cell has.
func (c *labCard) run(all bool) {
	c.mu.Lock()
	if c.running {
		c.status = "a run is in progress: x interrupts it"
		c.drawLocked()
		c.mu.Unlock()
		return
	}
	d, n, spec, err := c.noteLocked()
	if err != nil {
		c.status = err.Error()
		c.drawLocked()
		c.mu.Unlock()
		return
	}
	type job struct {
		code string
		cell *cells.Cell // nil: a selection, whose outputs are not kept
	}
	var jobs []job
	cs := n.Cells(spec.Language)
	switch {
	case all:
		for i := range cs {
			jobs = append(jobs, job{code: cs[i].Code, cell: &cs[i]})
		}
	case selected(d) != "":
		jobs = []job{{code: selected(d)}}
	default:
		cell, ok := n.At(spec.Language, d.CursorByte)
		if !ok {
			c.status = "the cursor is in no " + spec.Language + " cell"
			c.drawLocked()
			c.mu.Unlock()
			return
		}
		jobs = []job{{code: cell.Code, cell: &cell}}
	}
	if len(jobs) == 0 {
		c.status = "the note has no " + spec.Language + " cells"
		c.drawLocked()
		c.mu.Unlock()
		return
	}
	c.running, c.log = true, nil
	c.runs.Add(1)
	c.drawLocked()
	c.mu.Unlock()

	go func() {
		defer c.runs.Done()
		defer func() {
			c.mu.Lock()
			c.running = false
			c.drawLocked()
			c.mu.Unlock()
		}()
		k, err := c.kernelFor(d.Path, spec)
		if err != nil {
			c.say(err.Error())
			return
		}
		outDir := filepath.Join(filepath.Dir(d.Path), filepath.FromSlash(lab.OutDir(d.Path)))
		for i, j := range jobs {
			c.say(fmt.Sprintf("running %d of %d…", i+1, len(jobs)))
			var r outputs.Run
			reply, err := k.Execute(context.Background(), j.code, false, func(m kernel.Message) {
				r.Add(m.Header.MsgType, m.Content)
				c.show(m)
			})
			if err != nil {
				c.say(err.Error())
				return
			}
			if j.cell != nil {
				if err := outputs.Commit(filepath.Join(outDir, j.cell.Key()), r.Files()); err != nil {
					c.say("keeping the outputs: " + err.Error())
					return
				}
			}
			if len(r.Tables) > 0 {
				c.sendTable(r.Tables[len(r.Tables)-1])
			}
			if reply.Status != "ok" {
				c.say(fmt.Sprintf("cell %d of %d: %s %s", i+1, len(jobs), reply.EName, reply.EValue))
				return
			}
		}
		if all {
			keep := make([]string, len(jobs))
			for i, j := range jobs {
				keep[i] = j.cell.Key()
			}
			if err := outputs.Prune(outDir, keep); err != nil {
				c.say("removing outputs no cell has: " + err.Error())
				return
			}
		}
		c.say(fmt.Sprintf("ran %d %s, %s", len(jobs), plural(len(jobs), "cell"), time.Now().Format("15:04:05")))
	}()
}

// selected is the document's selection, when one is not empty: run on its own.
func selected(d plugin.Document) string {
	for _, r := range d.SelectionBytes {
		if r[1] > r[0] && r[0] >= 0 && r[1] <= len(d.Text) {
			return d.Text[r[0]:r[1]]
		}
	}
	return ""
}

func plural(n int, s string) string {
	if n == 1 {
		return s
	}
	return s + "s"
}

// kernelFor is the note's running kernel of spec, started (in the note's folder) when it has none
// or one of another spec.
func (c *labCard) kernelFor(path string, spec kernel.Spec) (*kernel.Client, error) {
	c.mu.Lock()
	k := c.kernels[path]
	c.mu.Unlock()
	if k != nil && k.Spec.Name == spec.Name && k.Alive() {
		return k, nil
	}
	if k != nil {
		k.Shutdown()
	}
	c.say("starting " + display(spec) + "…")
	ctx, cancel := context.WithTimeout(context.Background(), kernel.ReadyTimeout)
	defer cancel()
	k, err := c.start(ctx, spec, c.connDir, filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if spec.Language == "python" {
		_, _ = k.Execute(ctx, pandasSetup, true, nil)
	}
	c.mu.Lock()
	c.kernels[path] = k
	c.mu.Unlock()
	return k, nil
}

func display(s kernel.Spec) string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	return s.Name
}

// show adds an output message to the card's log, as lines.
func (c *labCard) show(m kernel.Message) {
	var r outputs.Run
	r.Add(m.Header.MsgType, m.Content)
	var lines []string
	for _, f := range r.Files() {
		switch f.Ext {
		case ".txt":
			lines = append(lines, strings.Split(strings.TrimRight(string(f.Data), "\n"), "\n")...)
		case ".csv":
			t := r.Tables[0]
			lines = append(lines, fmt.Sprintf("[a table: %d rows × %d columns]", len(t.Rows), len(t.Fields)))
		default:
			lines = append(lines, "["+strings.TrimPrefix(f.Ext, ".")+" output]")
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, lines...)
	if over := len(c.log) - 500; over > 0 {
		c.log = c.log[over:]
	}
	c.drawLocked()
}

// sendTable shows a DataFrame in AutoDoc's table panel: its first page, the rest on fetch.
func (c *labCard) sendTable(t outputs.Table) {
	c.mu.Lock()
	c.tableN++
	id := fmt.Sprintf("df-%d", c.tableN)
	c.tables[id], c.sent[id] = &t, min(len(t.Rows), tablePage)
	peer := c.peer
	c.mu.Unlock()
	n := min(len(t.Rows), tablePage)
	_ = peer.Table(plugin.TableOpen{ID: id, Title: "DataFrame", Fields: t.Fields, Rows: t.Rows[:n], Kinds: t.Kinds[:n], More: n < len(t.Rows)})
}

// FetchRows serves a DataFrame's next rows.
func (c *labCard) FetchRows(f plugin.TableFetch) {
	c.mu.Lock()
	t, at, peer := c.tables[f.ID], c.sent[f.ID], c.peer
	if t == nil {
		c.mu.Unlock()
		_ = peer.RowsKinds(f.ID, nil, nil, false)
		return
	}
	end := min(len(t.Rows), at+min(max(f.N, 1), tablePage))
	c.sent[f.ID] = end
	c.mu.Unlock()
	_ = peer.RowsKinds(f.ID, t.Rows[at:end], t.Kinds[at:end], end < len(t.Rows))
}

// TableClosed forgets a table its panel no longer shows.
func (c *labCard) TableClosed(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tables, id)
	delete(c.sent, id)
}

// interrupt stops the open note's run.
func (c *labCard) interrupt() {
	c.mu.Lock()
	k := c.kernels[c.doc.Path]
	c.mu.Unlock()
	if k == nil {
		c.say("no kernel runs for this note")
		return
	}
	if err := k.Interrupt(); err != nil {
		c.say("interrupt: " + err.Error())
	}
}

// restart starts the note's kernel afresh: its variables are gone.
func (c *labCard) restart() {
	c.mu.Lock()
	k := c.kernels[c.doc.Path]
	c.mu.Unlock()
	if k == nil {
		c.say("no kernel runs for this note: r starts one")
		return
	}
	c.say("restarting " + display(k.Spec) + "…")
	c.runs.Add(1)
	go func() {
		defer c.runs.Done()
		ctx, cancel := context.WithTimeout(context.Background(), kernel.ReadyTimeout)
		defer cancel()
		if err := k.Restart(ctx); err != nil {
			c.say("restart: " + err.Error())
			return
		}
		if k.Spec.Language == "python" {
			_, _ = k.Execute(ctx, pandasSetup, true, nil)
		}
		c.say(display(k.Spec) + " restarted: its variables are gone")
	}()
}

// choose lists the installed kernels to pick the note's from.
func (c *labCard) choose() {
	specs := c.specs()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(specs) == 0 {
		c.status = "no Jupyter kernel is installed: pip install ipykernel installs Python's"
	} else {
		c.picking, c.pick = specs, 0
	}
	c.drawLocked()
}

// export writes the note as an .ipynb beside it, never over one.
func (c *labCard) export() {
	c.mu.Lock()
	d, _, spec, err := c.noteLocked()
	c.mu.Unlock()
	if err != nil {
		c.say(err.Error())
		return
	}
	out, err := ipynb.Export(d.Path, ipynb.Kernel{Name: spec.Name, DisplayName: display(spec), Language: spec.Language})
	if err != nil {
		c.say(err.Error())
		return
	}
	c.say("exported " + filepath.Base(out))
}

// importNotebook writes the lab note beside the .ipynb at p (or finds it written) and opens it.
func (c *labCard) importNotebook(p string) {
	labPath, err := ipynb.Import(p)
	if err != nil {
		c.say(err.Error())
		return
	}
	c.say("opened " + filepath.Base(labPath))
	c.mu.Lock()
	peer := c.peer
	c.mu.Unlock()
	if ok, err := peer.Call("file.open_path", labPath); !ok {
		c.say(fmt.Sprintf("opening %s: %v", filepath.Base(labPath), err))
	}
}

// drawLocked sends the card's frame: the note and its kernel, the status, the last run's outputs (or
// the kernel chooser), and the keys.
func (c *labCard) drawLocked() {
	if c.peer == nil || c.w <= 0 || c.h <= 0 {
		return
	}
	f := plugin.NewFrame(c.w, c.h)
	accent := plugin.Style{FG: c.theme.Colors["document.cursor"], Bold: true}
	muted := plugin.Style{FG: c.theme.Colors["document.lineNumber"]}
	note := "no lab note open"
	if c.doc.Path != "" {
		note = filepath.Base(c.doc.Path)
	}
	x := f.Text(0, 0, "Jupyter", accent)
	kname := ""
	if k := c.kernels[c.doc.Path]; k != nil {
		kname = display(k.Spec)
		if !k.Alive() {
			kname += " (stopped)"
		}
	}
	f.Text(x, 0, " · "+note+orEmpty(" · ", kname), muted)
	state := c.status
	if c.running {
		state = "● " + state
	}
	f.Text(0, 1, state, plugin.Style{})
	f.Fill(0, 2, c.w, 1, '─', muted)
	body := c.h - 4
	if c.picking != nil {
		f.Text(0, 3, "choose the note's kernel (↑↓, Enter; q leaves):", muted)
		for i, s := range c.picking {
			if 4+i >= c.h-1 {
				break
			}
			st := plugin.Style{}
			mark := "  "
			if i == c.pick {
				st, mark = accent, "▸ "
			}
			f.Text(0, 4+i, mark+display(s)+" ("+s.Language+")", st)
		}
	} else {
		lines := c.log
		if len(lines) > body {
			lines = lines[len(lines)-body:]
		}
		for i, l := range lines {
			f.Text(0, 3+i, l, plugin.Style{})
		}
	}
	f.Text(0, c.h-1, "r run · a all · x interrupt · s restart · k kernel · e export", muted)
	_ = c.peer.Frame(f)
}

func orEmpty(sep, s string) string {
	if s == "" {
		return ""
	}
	return sep + s
}
