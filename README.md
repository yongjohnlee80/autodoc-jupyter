# autodoc-jupyter

A Jupyter plugin for [AutoDoc](https://github.com/yongjohnlee80/autodoc): run the code cells of a
**lab file** (`*.lab.md`) in a Jupyter kernel, keep their outputs beside the note, and open an
`.ipynb` as a lab file.

- A lab file is an ordinary Markdown note. Its fenced code cells in the kernel's language are
  runnable, and it stays searchable, linkable and rendered like any note.
- Outputs (text, tracebacks, images, Vega-Lite charts, CSV) are files in the note's companion
  folder, `plan.lab/out/<cell key>/`. The note itself is never edited. AutoDoc's Rendered view and
  HTML export draw them under each cell.
- A DataFrame also opens in AutoDoc's results table.
- Opening an `.ipynb` writes `nb.lab.md` beside it and opens that. Export writes an `.ipynb` back.
- The kernel is any installed Jupyter kernel (`ipykernel`, IRkernel, …), spoken to directly over
  ZeroMQ (pure Go, [`go-zeromq/zmq4`](https://github.com/go-zeromq/zmq4)): one binary, no Python
  helper.

Design: ADR 1792457255, *AutoDoc lab files and the autodoc-jupyter plugin*.

Status: in development. It needs an AutoDoc that speaks plugin protocol 9.

## Licence

Apache-2.0, as AutoDoc.
