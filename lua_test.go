package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"

	"github.com/yongjohnlee80/autodoc/core/manifests"
)

// ran is one autodoc.plugin.run the package made: the command and its arguments.
type ran struct {
	id   string
	args []string
}

// jptCommand loads lua/init.lua with an autodoc table that stands in for AutoDoc's: autodoc.command
// keeps the table it is given, and autodoc.plugin.run answers as AutoDoc's runOwnPlugin does for a
// manifest's commands ("unknown: <id>" for none of them), recording each run. It returns that
// table and the runs so far.
func jptCommand(t *testing.T, commands []string) (*lua.LState, *lua.LTable, *[]ran) {
	t.Helper()
	L := lua.NewState()
	t.Cleanup(L.Close)
	var cmd *lua.LTable
	var runs []ran
	autodoc, pl := L.NewTable(), L.NewTable()
	L.SetField(autodoc, "command", L.NewFunction(func(L *lua.LState) int { cmd = L.CheckTable(1); return 0 }))
	L.SetField(pl, "run", L.NewFunction(func(L *lua.LState) int {
		id := L.CheckString(1)
		var args []string
		for i := 2; i <= L.GetTop(); i++ {
			args = append(args, L.CheckString(i))
		}
		if !slices.Contains(commands, id) {
			L.Push(lua.LFalse)
			L.Push(lua.LString("unknown: " + id))
			return 2
		}
		runs = append(runs, ran{id, args})
		L.Push(lua.LTrue)
		L.Push(lua.LNil)
		return 2
	}))
	L.SetField(autodoc, "plugin", pl)
	L.SetGlobal("autodoc", autodoc)
	if err := L.DoFile(filepath.Join("lua", "init.lua")); err != nil {
		t.Fatal(err)
	}
	if cmd == nil {
		t.Fatal("lua/init.lua registered no command")
	}
	return L, cmd, &runs
}

// strings of a Lua list.
func listOf(v lua.LValue) []string {
	var out []string
	if t, ok := v.(*lua.LTable); ok {
		t.ForEach(func(_, v lua.LValue) { out = append(out, v.String()) })
	}
	return out
}

// The Lua package registers :jpt (and :jupyter), completing exactly the manifest's commands a user
// runs (import is the [open] hook's). Each subcommand reaches the plugin's command of that name,
// the rest its arguments, and :jpt alone runs the cell; one the plugin doesn't declare is the
// command's error, as autodoc.plugin.run says it. lua/package.toml names the package the plugin.
func TestTheLuaPackageRunsThePluginsCommands(t *testing.T) {
	m, err := manifests.ReadPlugin(os.DirFS("."))
	if err != nil {
		t.Fatal(err)
	}
	var commands, runnable []string
	for _, c := range m.Commands {
		commands = append(commands, c.ID)
		if c.ID != m.Open.Command {
			runnable = append(runnable, c.ID)
		}
	}
	L, cmd, runs := jptCommand(t, commands)
	if name := cmd.RawGetString("name"); name.String() != "jpt" || !slices.Equal(listOf(cmd.RawGetString("aliases")), []string{"jupyter"}) {
		t.Errorf("registered %v, aliases %q; want jpt, jupyter", name, listOf(cmd.RawGetString("aliases")))
	}
	complete := listOf(cmd.RawGetString("complete"))
	slices.Sort(complete)
	slices.Sort(runnable)
	if !slices.Equal(complete, runnable) {
		t.Errorf(":jpt completes %q; the manifest's commands are %q", complete, runnable)
	}
	run, ok := cmd.RawGetString("run").(*lua.LFunction)
	if !ok {
		t.Fatal(":jpt has no run function")
	}
	jpt := func(words ...string) error {
		args := L.NewTable()
		for _, w := range words {
			args.Append(lua.LString(w))
		}
		return L.CallByParam(lua.P{Fn: run, Protect: true}, lua.LNil, args)
	}
	var want []ran
	for _, id := range runnable {
		if err := jpt(id); err != nil {
			t.Errorf(":jpt %s: %v", id, err)
		}
		want = append(want, ran{id, nil})
	}
	for _, words := range [][]string{{"clear", "all"}, {}} {
		if err := jpt(words...); err != nil {
			t.Errorf(":jpt %q: %v", words, err)
		}
	}
	want = append(want, ran{"clear", []string{"all"}}, ran{"run", nil})
	if !slices.EqualFunc(*runs, want, func(a, b ran) bool { return a.id == b.id && slices.Equal(a.args, b.args) }) {
		t.Errorf("the plugin commands run: %v; want %v", *runs, want)
	}
	n := len(*runs)
	// the error as raised, error(err, 0): its value, not the traceback ApiError.Error adds
	var raised *lua.ApiError
	if err := jpt("nope"); !errors.As(err, &raised) || raised.Object.String() != "unknown: nope" || len(*runs) != n {
		t.Errorf(":jpt nope: %v, %d runs; want the unknown error and no run", err, len(*runs)-n)
	}
	pkg, err := os.ReadFile(filepath.Join("lua", "package.toml"))
	if err != nil || !strings.Contains(string(pkg), `name = "jupyter"`) {
		t.Errorf("lua/package.toml: %v; want the plugin's name", err)
	}
}
