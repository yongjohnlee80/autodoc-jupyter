package kernel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Spec is a kernelspec: a kernel's kernel.json, and the folder it was read from.
type Spec struct {
	// Name is the kernel's name: its folder's ("python3").
	Name string
	// Dir is the kernelspec's folder; "{resource_dir}" in Argv names it.
	Dir string
	// Argv starts the kernel. "{connection_file}" is replaced with the connection file's path.
	Argv        []string `json:"argv"`
	DisplayName string   `json:"display_name"`
	// Language is the kernel's language ("python"): a lab note's cells are the fences in it.
	Language string `json:"language"`
	// InterruptMode is "signal" (SIGINT to the kernel's process group; the default) or "message"
	// (an interrupt_request on the control channel).
	InterruptMode string            `json:"interrupt_mode"`
	Env           map[string]string `json:"env"`
}

// SpecDirs are the folders kernelspecs are read from, in the order a name is first found: each of
// $JUPYTER_PATH, the user's data folder ($JUPYTER_DATA_DIR, else ~/.local/share/jupyter, or
// ~/Library/Jupyter on macOS), the active virtual environment's or conda environment's
// share/jupyter, then /usr/local/share/jupyter and /usr/share/jupyter. Each holds kernels/<name>/.
func SpecDirs() []string {
	var dirs []string
	for _, d := range filepath.SplitList(os.Getenv("JUPYTER_PATH")) {
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	switch {
	case os.Getenv("JUPYTER_DATA_DIR") != "":
		dirs = append(dirs, os.Getenv("JUPYTER_DATA_DIR"))
	case runtime.GOOS == "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			dirs = append(dirs, filepath.Join(home, "Library", "Jupyter"))
		}
	default:
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" {
			if home, err := os.UserHomeDir(); err == nil {
				data = filepath.Join(home, ".local", "share")
			}
		}
		if data != "" {
			dirs = append(dirs, filepath.Join(data, "jupyter"))
		}
	}
	for _, env := range []string{"VIRTUAL_ENV", "CONDA_PREFIX"} {
		if p := os.Getenv(env); p != "" {
			dirs = append(dirs, filepath.Join(p, "share", "jupyter"))
		}
	}
	return append(dirs, "/usr/local/share/jupyter", "/usr/share/jupyter")
}

// Specs are the kernelspecs found under SpecDirs, by name; the first folder holding a name wins.
// A kernel.json that cannot be read, or that has no argv, is skipped.
func Specs() []Spec {
	var out []Spec
	for _, d := range SpecDirs() {
		entries, err := os.ReadDir(filepath.Join(d, "kernels"))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || slices.ContainsFunc(out, func(s Spec) bool { return s.Name == e.Name() }) {
				continue
			}
			s, err := ReadSpec(filepath.Join(d, "kernels", e.Name()))
			if err != nil {
				continue
			}
			out = append(out, s)
		}
	}
	return out
}

// ReadSpec reads the kernelspec in dir: its kernel.json.
func ReadSpec(dir string) (Spec, error) {
	data, err := os.ReadFile(filepath.Join(dir, "kernel.json"))
	if err != nil {
		return Spec{}, err
	}
	var s Spec
	if err := json.Unmarshal(data, &s); err != nil {
		return Spec{}, fmt.Errorf("kernel: %s: %w", filepath.Join(dir, "kernel.json"), err)
	}
	if len(s.Argv) == 0 {
		return Spec{}, fmt.Errorf("kernel: %s has no argv", filepath.Join(dir, "kernel.json"))
	}
	s.Name, s.Dir = filepath.Base(dir), dir
	s.Language = strings.ToLower(s.Language)
	return s, nil
}

// argv is the spec's command for the connection file at conn.
func (s Spec) argv(conn string) []string {
	out := make([]string, len(s.Argv))
	for i, a := range s.Argv {
		a = strings.ReplaceAll(a, "{connection_file}", conn)
		out[i] = strings.ReplaceAll(a, "{resource_dir}", s.Dir)
	}
	return out
}
