package outputs

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// pandasFrame is the bundle pandas 2.2 sends for a DataFrame once display.html.table_schema is on
// (VM43's gate image, kg1's probe): the table schema, with the index first, beside its HTML and text.
const pandasFrame = `{"application/vnd.dataresource+json": {"schema": {"fields": [{"name": "index", "type": "integer"}, {"name": "a", "type": "integer"}, {"name": "b", "type": "string"}, {"name": "c", "type": "number"}], "primaryKey": ["index"], "pandas_version": "1.4.0"}, "data": [{"index": 0, "a": 1, "b": "x, y", "c": 1.5}, {"index": 1, "a": 2, "b": null, "c": null}]}, "text/html": "<table></table>", "text/plain": "   a     b    c\n0  1  x, y  1.5"}`

func bundle(t *testing.T, s string) Bundle {
	t.Helper()
	var b Bundle
	if err := json.Unmarshal([]byte(s), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// Each kind of output is kept as its row of the table: a DataFrame as CSV (a null empty, a comma
// quoted) and as a table (a null shown as null, its kinds); an image decoded; SVG and Vega-Lite as
// themselves; anything else as its text/plain, a list of lines (as .ipynb keeps it) joined.
func TestEachBundleIsKeptAsItsFile(t *testing.T) {
	f, table, ok := FromBundle(bundle(t, pandasFrame))
	if !ok || f.Ext != ".csv" || string(f.Data) != "index,a,b,c\n0,1,\"x, y\",1.5\n1,2,,\n" {
		t.Fatalf("a DataFrame: %v %q %q", ok, f.Ext, f.Data)
	}
	if !slices.Equal(table.Fields, []string{"index", "a", "b", "c"}) || !slices.Equal(table.Rows[1], []string{"1", "2", "null", "null"}) ||
		!slices.Equal(table.Kinds, []string{"nntn", "nnzz"}) {
		t.Errorf("its table: %+v", table)
	}
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nfake"))
	for _, c := range []struct {
		bundle, ext, data string
	}{
		{`{"image/png": "` + png[:8] + `\n` + png[8:] + `", "text/plain": "<Figure>"}`, ".png", "\x89PNG\r\n\x1a\nfake"},
		{`{"image/jpeg": "` + base64.StdEncoding.EncodeToString([]byte("jpg")) + `"}`, ".jpg", "jpg"},
		{`{"image/svg+xml": ["<svg>\n", "</svg>"], "text/plain": "x"}`, ".svg", "<svg>\n</svg>"},
		{`{"application/vnd.vegalite.v4+json": {"mark": "bar"}, "text/plain": "x"}`, ".chart.json", "{\n  \"mark\": \"bar\"\n}\n"},
		{`{"text/html": "<b>x</b>", "text/plain": ["a\n", "b"]}`, ".txt", "a\nb"},
	} {
		f, table, ok := FromBundle(bundle(t, c.bundle))
		if !ok || f.Ext != c.ext || string(f.Data) != c.data || table != nil {
			t.Errorf("%s: %v %q %q", c.bundle, ok, f.Ext, f.Data)
		}
	}
	if _, _, ok := FromBundle(bundle(t, `{"text/html": "<b>x</b>"}`)); ok {
		t.Error("a bundle with no type kept was kept")
	}
}

// A run's outputs are its messages in order: consecutive streams of one name joined, another name
// or another output starting a new file; an error as its traceback without colours; clear_output
// dropping what came before; status and execute_input are not outputs.
func TestARunKeepsItsOutputsInOrder(t *testing.T) {
	var r Run
	for _, m := range []struct{ typ, content string }{
		{"stream", `{"name": "stdout", "text": "a\n"}`},
		{"stream", `{"name": "stdout", "text": "b\n"}`},
		{"stream", `{"name": "stderr", "text": "warn\n"}`},
		{"status", `{"execution_state": "idle"}`},
		{"execute_result", `{"data": {"text/plain": "42"}}`},
		{"stream", `{"name": "stdout", "text": "c\n"}`},
		{"error", `{"ename": "ValueError", "evalue": "bad", "traceback": ["\u001b[0;31mValueError\u001b[0m: bad"]}`},
	} {
		r.Add(m.typ, json.RawMessage(m.content))
	}
	var got []string
	for _, f := range r.Files() {
		got = append(got, f.Ext+":"+string(f.Data))
	}
	want := []string{".txt:a\nb\n", ".txt:warn\n", ".txt:42", ".txt:c\n", ".txt:ValueError: bad\n"}
	if !slices.Equal(got, want) {
		t.Errorf("files %q, want %q", got, want)
	}
	r.Add("display_data", json.RawMessage(`{"data": `+pandasFrame+`}`))
	if len(r.Tables) != 1 {
		t.Errorf("tables %d", len(r.Tables))
	}
	r.Add("clear_output", json.RawMessage(`{"wait": false}`))
	r.Add("stream", json.RawMessage(`{"name": "stdout", "text": "d\n"}`))
	if files := r.Files(); len(files) != 1 || string(files[0].Data) != "d\n" || len(r.Tables) != 0 {
		t.Errorf("after clear_output: %+v, %d tables", files, len(r.Tables))
	}
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// Commit makes the files the folder's whole content, NN.<ext> in order: a rerun with fewer outputs
// leaves none of the older run's, no hidden folder is left behind, and a run with none removes the
// folder. A hundred outputs are named so that name order is output order.
func TestCommitReplacesTheCellsFolderWhole(t *testing.T) {
	out := filepath.Join(t.TempDir(), "plan.lab", "out")
	dir := filepath.Join(out, "k1")
	if err := Commit(dir, []File{{".txt", []byte("1")}, {".png", []byte("p")}, {".txt", []byte("3")}}); err != nil {
		t.Fatal(err)
	}
	if got := names(t, dir); !slices.Equal(got, []string{"01.txt", "02.png", "03.txt"}) {
		t.Fatalf("first run: %v", got)
	}
	if err := Commit(dir, []File{{".csv", []byte("a\n1\n")}}); err != nil {
		t.Fatal(err)
	}
	if got := names(t, dir); !slices.Equal(got, []string{"01.csv"}) {
		t.Errorf("rerun: %v; want the older run's files gone", got)
	}
	if got := names(t, out); !slices.Equal(got, []string{"k1"}) {
		t.Errorf("out/ holds %v; want no hidden folder left", got)
	}
	if err := Commit(dir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) || len(names(t, out)) != 0 {
		t.Errorf("a run with no outputs left %v", names(t, out))
	}
	many := make([]File, 100)
	for i := range many {
		many[i] = File{".txt", []byte{byte('0' + i%10)}}
	}
	if err := Commit(dir, many); err != nil {
		t.Fatal(err)
	}
	got := names(t, dir)
	if len(got) != 100 || got[0] != "001.txt" || got[9] != "010.txt" || got[99] != "100.txt" {
		t.Errorf("a hundred outputs: %v…", got[:3])
	}
}

// Prune removes the folders of keys no cell has, and leaves a run being written.
func TestPruneRemovesOutputsNoCellHas(t *testing.T) {
	out := t.TempDir()
	for _, d := range []string{"keep", "gone", ".k.run-1"} {
		if err := os.MkdirAll(filepath.Join(out, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := Prune(out, []string{"keep"}); err != nil {
		t.Fatal(err)
	}
	if got := names(t, out); !slices.Equal(got, []string{".k.run-1", "keep"}) {
		t.Errorf("after Prune: %v", got)
	}
	if err := Prune(filepath.Join(out, "none"), nil); err != nil {
		t.Errorf("a note with no outputs yet: %v", err)
	}
}

// An error with no traceback is its name and value.
func TestAnErrorWithoutATraceback(t *testing.T) {
	if f := Error("NameError", "x", nil); f.Ext != ".txt" || !strings.HasPrefix(string(f.Data), "NameError: x") {
		t.Errorf("%q", f.Data)
	}
}
