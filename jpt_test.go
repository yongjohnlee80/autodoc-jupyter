package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodoc/core/manifests"
	"github.com/yongjohnlee80/autodoc/plugin"

	"github.com/yongjohnlee80/autodoc-jupyter/internal/fakekernel"
)

// clear removes the outputs of the cell under the cursor and no other's; clear all removes every
// cell's and the outputs folder. A cursor in no cell, and a run in progress, clear nothing.
func TestClearRemovesTheCellsOutputsOrTheNotes(t *testing.T) {
	c, _ := card(t, fakekernel.Signal)
	p := note(t, c, "plan.lab.md", labText, "# Lab")
	c.Command("all")
	if s := settled(t, c); !strings.Contains(s, "ran 2 cells") {
		t.Fatalf("status %q", s)
	}
	out := filepath.Join(filepath.Dir(p), "plan.lab", "out")

	c.mu.Lock()
	c.running = true
	c.mu.Unlock()
	c.CommandArgs("clear", []string{"all"})
	c.mu.Lock()
	c.running = false
	status := c.status
	c.mu.Unlock()
	if !strings.Contains(status, "a run is in progress") || len(outputsOf(t, p, "print")) == 0 {
		t.Fatalf("clear during a run: %q, the outputs %v", status, outputsOf(t, p, "print"))
	}

	c.Document(plugin.Document{Path: p, Text: labText, CursorByte: strings.Index(labText, "print\n```\n\n```mermaid")})
	c.Command("clear")
	if got := outputsOf(t, p, "print"); len(got) != 0 {
		t.Errorf("the cell's outputs after clear: %q", got)
	}
	if got := outputsOf(t, p, "png"); got["01.png"] != string(fakekernel.PNG) {
		t.Errorf("clear took another cell's outputs: %q", got)
	}
	c.mu.Lock()
	status = c.status
	c.mu.Unlock()
	if status != "cleared the cell's outputs" {
		t.Errorf("status %q", status)
	}

	c.Document(plugin.Document{Path: p, Text: labText, CursorByte: strings.Index(labText, "# Lab")})
	c.Command("clear")
	if got := outputsOf(t, p, "png"); got["01.png"] != string(fakekernel.PNG) {
		t.Errorf("a clear with the cursor in no cell took outputs: %q", got)
	}
	c.CommandArgs("clear", []string{"all"})
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("clear all left the outputs folder: %v", err)
	}
}

// The Lua package registers :jpt (and :jupyter) and sends each subcommand to the plugin's command
// of that name, the rest its arguments (autodoc.plugin.run), :jpt alone running the cell. Its
// completion lists exactly the manifest's commands a user runs (import is the [open] hook's). Read
// as text: running it needs AutoDoc's script engine, whose Lua is no dependency of this module.
func TestTheLuaPackageRunsThePluginsCommands(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("lua", "init.lua"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`name = "jpt"`, `aliases = { "jupyter" }`, `autodoc.plugin.run(args[1] or "run", select(2, unpack(args)))`, `if not ok then error(err, 0) end`} {
		if !strings.Contains(string(src), want) {
			t.Errorf("lua/init.lua lacks %q", want)
		}
	}
	m, err := manifests.ReadPlugin(os.DirFS("."))
	if err != nil {
		t.Fatal(err)
	}
	var runnable []string
	for _, c := range m.Commands {
		if c.ID != m.Open.Command {
			runnable = append(runnable, c.ID)
		}
	}
	list := regexp.MustCompile(`complete = \{([^}]*)\}`).FindSubmatch(src)
	if list == nil {
		t.Fatal("lua/init.lua has no complete list")
	}
	var complete []string
	for _, w := range regexp.MustCompile(`"([a-z]+)"`).FindAllSubmatch(list[1], -1) {
		complete = append(complete, string(w[1]))
	}
	slices.Sort(runnable)
	slices.Sort(complete)
	if !slices.Equal(complete, runnable) {
		t.Errorf(":jpt completes %q; the manifest's commands are %q", complete, runnable)
	}
	pkg, err := os.ReadFile(filepath.Join("lua", "package.toml"))
	if err != nil || !strings.Contains(string(pkg), `name = "jupyter"`) {
		t.Errorf("lua/package.toml: %v; want the plugin's name", err)
	}
}
