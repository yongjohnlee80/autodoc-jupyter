-- :jpt for AutoDoc: the Jupyter plugin's commands from the command line. AutoDoc loads this file
-- (with package.toml) as the plugin's Lua package.
autodoc.command{
  name = "jpt",
  aliases = { "jupyter" },
  args = "run | all | clear [all] | interrupt | restart | kernel | export",
  complete = { "run", "all", "clear", "interrupt", "restart", "kernel", "export" },
  help = "Jupyter: run the cell (or selection), run all, clear the cell's outputs (all: the note's), interrupt or restart the kernel, choose it, export to .ipynb",
  run = function(ed, args)
    local ok, err = autodoc.plugin.run(args[1] or "run", select(2, unpack(args)))
    if not ok then error(err, 0) end
  end,
}
