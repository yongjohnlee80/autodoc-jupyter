package cells

import (
	"slices"
	"strings"
	"testing"

	"github.com/yongjohnlee80/autodoc/core/lab"
)

const note = "---\ntitle: Plan\nkernel: python3\n---\n\n# Plan\n\n```python\nx = 1\n```\n\n```mermaid\nx = 1\n```\n\n- a step\n\n  ```Python {.x}\n  print(x)\n  ```\n\n```r\nx <- 1\n```\n"

// A lab note's kernel is its front matter's; its cells are its fences in the kernel's language, a
// fence in a list item included, its language read case-blind with the info string's attributes
// dropped; every other fence is content.
func TestANotesCellsAreItsFencesInTheKernelsLanguage(t *testing.T) {
	n := Read(note)
	if n.Kernel != "python3" {
		t.Errorf("kernel %q", n.Kernel)
	}
	var codes []string
	for _, c := range n.Cells("Python") {
		codes = append(codes, c.Code)
	}
	if !slices.Equal(codes, []string{"x = 1\n", "print(x)\n"}) {
		t.Fatalf("python cells %q", codes)
	}
	if r := n.Cells("r"); len(r) != 1 || r[0].Code != "x <- 1\n" {
		t.Errorf("r cells %+v", r)
	}
	if n.Cells("") != nil {
		t.Error("a kernel with no language has cells")
	}
}

// A cell's key is lab.CellKey of its language and code, as AutoDoc's export computes it from the same
// parse; a mermaid fence holding the same body has another (the negative control), and a CRLF note's
// cell has its LF twin's key.
func TestACellsKeyIsTheExportsKey(t *testing.T) {
	n := Read(note)
	cells := n.Cells("python")
	if cells[0].Key() != lab.CellKey("python", "x = 1") || cells[1].Key() != lab.CellKey("python", "print(x)") {
		t.Errorf("keys %s %s", cells[0].Key(), cells[1].Key())
	}
	if mermaid := lab.CellKey("mermaid", "x = 1"); mermaid == cells[0].Key() {
		t.Error("the mermaid fence's key is the cell's")
	}
	crlf := Read(strings.ReplaceAll(note, "\n", "\r\n")).Cells("python")
	if len(crlf) != 2 || crlf[0].Key() != cells[0].Key() || crlf[1].Key() != cells[1].Key() {
		t.Errorf("a CRLF note's keys differ: %+v", crlf)
	}
}

// At finds the cell holding a byte, on its fence lines too, and none between cells or in a fence that
// is not a cell.
func TestAtFindsTheCellUnderTheCursor(t *testing.T) {
	n := Read(note)
	open := strings.Index(note, "```python")
	inside := strings.Index(note, "x = 1")
	closing := strings.Index(note[inside:], "```") + inside
	for _, at := range []int{open, inside, closing + 2} {
		if c, ok := n.At("python", at); !ok || c.Code != "x = 1\n" {
			t.Errorf("at %d: %+v %v", at, c, ok)
		}
	}
	for _, at := range []int{strings.Index(note, "# Plan"), strings.Index(note, "```mermaid") + 12, 0} {
		if c, ok := n.At("python", at); ok {
			t.Errorf("at %d found %+v", at, c)
		}
	}
	listed := strings.Index(note, "print(x)")
	if c, ok := n.At("python", listed); !ok || c.Code != "print(x)\n" {
		t.Errorf("the listed cell: %+v %v", c, ok)
	}
}

// A note with no front matter, or one that is not YAML, names no kernel.
func TestANoteWithoutAKernel(t *testing.T) {
	for _, text := range []string{"# x\n\n```python\n1\n```\n", "---\n: [\n---\n# x\n", "---\nkernel: 3\n---\n", "---\ntitle: x\n---\n"} {
		if k := Read(text).Kernel; k != "" {
			t.Errorf("%q: kernel %q", text, k)
		}
	}
}
