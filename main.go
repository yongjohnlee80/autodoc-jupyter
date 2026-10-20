// Command autodoc-jupyter is the Jupyter plugin for AutoDoc (ADR 1792457255): a card that runs a lab
// note's cells (*.lab.md, its fences in the kernel's language) in a Jupyter kernel, keeps their
// outputs beside the note where AutoDoc draws them, and opens an .ipynb as a lab note. AutoDoc runs
// it from its Plugins menu; run by hand, it says so and stops.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/yongjohnlee80/autodoc/plugin"
)

func main() {
	dir, err := os.MkdirTemp("", "autodoc-jupyter-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "autodoc-jupyter:", err)
		os.Exit(1)
	}
	if err := plugin.Serve(context.Background(), newLab(dir)); err != nil {
		os.RemoveAll(dir)
		fmt.Fprintln(os.Stderr, "autodoc-jupyter:", err)
		os.Exit(1)
	}
}
