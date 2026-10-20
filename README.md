# autodoc-jupyter

Jupyter for [AutoDoc](https://github.com/yongjohnlee80/autodoc), as a plugin (AutoDoc's ADR 1792457255).

A **lab note** is a Markdown note named `*.lab.md`. Its fences in the kernel's language are cells,
which run in a Jupyter kernel, and every other fence (a Mermaid diagram, a CSV table, a Vega-Lite
chart) stays content. A cell's outputs are kept beside the note, never in it:

```text
plan.lab.md
plan.lab/out/<cell key>/01.txt          text, an error's traceback
plan.lab/out/<cell key>/02.png          a plot
plan.lab/out/<cell key>/03.csv          a DataFrame (also shown in AutoDoc's table panel)
plan.lab/out/<cell key>/04.chart.json   a Vega-Lite chart
```

AutoDoc draws them under their cell, in its HTML export and preview and in `--gui`'s Rendered view.
The cell key is a hash of the fence's language and code, so an edited cell shows nothing until it
runs again, and `git diff` of the note never fills with output.

## Use

It needs AutoDoc v0.2.1 or later: plugin protocol 9 (lab notes and the `.ipynb` open hook), a document's workspace root (to find the note on disk), and `view.redraw` (to draw a run's outputs at once). An older one refuses the plugin: it does not speak protocol 9.

- **Add it:** Plugins › Add from a git URL…, with this repository's URL. AutoDoc builds it with Go.
- **Kernels:** any Jupyter kernel installed where Jupyter keeps them. Python's comes with ipykernel
  (`pip install ipykernel`, or your system's `python3-ipykernel`). A note picks one in its front
  matter: `kernel: python3`. Without one, the card uses the first Python kernel.
- **The card**, docked on the right, while a lab note is open:

  | key | |
  |---|---|
  | `r` | run the cell under the cursor (or the selection, its outputs shown, not kept) |
  | `a` | run every cell in order, stopping at the first error, then remove outputs no cell has |
  | `x` | interrupt the run |
  | `s` | restart the kernel: its variables are gone |
  | `k` | choose the note's kernel |
  | `e` | export the note as an `.ipynb` beside it (never over one) |

- **`.ipynb` files:** opening one in AutoDoc hands it to this plugin, which writes `nb.lab.md`
  beside `nb.ipynb` (never over an existing one) with its outputs, and opens that.

Each note has its own kernel, started in the note's folder, and kernels live on while the card is
hidden. Esc hides the card; turning the plugin off stops them.

## Build

```sh
go build -trimpath -o bin/autodoc-jupyter .
go test -race ./...
```

The kernel speaks Jupyter's messaging protocol in pure Go, on
[go-zeromq/zmq4](https://github.com/go-zeromq/zmq4): no libzmq, and no Python beside the kernel.

## Licence

Apache-2.0, as AutoDoc.
