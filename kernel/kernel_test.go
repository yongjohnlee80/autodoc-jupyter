package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A message survives its wire frames, identities and buffers too; a part changed after signing is
// refused; an empty key signs nothing, as Jupyter's does.
func TestAMessageIsSignedAndChecked(t *testing.T) {
	s := signer{key: []byte("k")}
	m := Message{Identities: [][]byte{[]byte("id")}, Header: newHeader("s", "stream"), Parent: newHeader("s", "execute_request"),
		Metadata: map[string]any{"a": "b"}, Content: json.RawMessage(`{"text":"1\n"}`), Buffers: [][]byte{[]byte("buf")}}
	frames, err := s.encode(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.decode(frames)
	if err != nil {
		t.Fatal(err)
	}
	if got.Header != m.Header || got.Parent != m.Parent || string(got.Content) != string(m.Content) || got.Metadata["a"] != "b" ||
		!bytes.Equal(got.Identities[0], []byte("id")) || !bytes.Equal(got.Buffers[0], []byte("buf")) {
		t.Errorf("round trip: %+v", got)
	}
	tampered := slices.Clone(frames)
	tampered[len(tampered)-2] = []byte(`{"text":"2\n"}`)
	if _, err := s.decode(tampered); !errors.Is(err, ErrSignature) {
		t.Errorf("a changed content: %v, want ErrSignature", err)
	}
	if _, err := (signer{key: []byte("other")}).decode(frames); !errors.Is(err, ErrSignature) {
		t.Errorf("another key: %v, want ErrSignature", err)
	}
	unsigned, _ := signer{}.encode(m)
	if len(unsigned[2]) != 0 {
		t.Errorf("an empty key signed: %q", unsigned[2])
	}
	if _, err := s.decode([][]byte{[]byte("no delimiter")}); err == nil {
		t.Error("frames with no delimiter were read")
	}
}

// Kernelspecs are read from Jupyter's folders, the first holding a name winning; a kernel.json that
// cannot be read, or has no argv, is skipped; argv names the connection file and the spec's folder.
func TestSpecsAreReadFromJupytersFolders(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	write := func(root, name, body string) {
		dir := filepath.Join(root, "kernels", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "kernel.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(first, "py", `{"argv":["python3","-m","ipykernel_launcher","-f","{connection_file}","--dir={resource_dir}"],"display_name":"Python 3","language":"Python"}`)
	write(second, "py", `{"argv":["other"],"language":"python"}`)
	write(second, "ir", `{"argv":["R","--slave"],"display_name":"R","language":"R","interrupt_mode":"message"}`)
	write(second, "broken", `{`)
	write(second, "empty", `{"language":"x"}`)
	t.Setenv("JUPYTER_PATH", first+string(os.PathListSeparator)+second)
	t.Setenv("JUPYTER_DATA_DIR", t.TempDir())
	t.Setenv("VIRTUAL_ENV", "")
	t.Setenv("CONDA_PREFIX", "")

	var names []string
	byName := map[string]Spec{}
	for _, s := range Specs() {
		if s.Dir == "" || !strings.HasPrefix(s.Dir, first) && !strings.HasPrefix(s.Dir, second) {
			continue // the machine's own kernels, under /usr/share/jupyter
		}
		names = append(names, s.Name)
		byName[s.Name] = s
	}
	if !slices.Equal(names, []string{"py", "ir"}) {
		t.Fatalf("specs %v, want py (the first folder's) and ir", names)
	}
	py := byName["py"]
	if py.Language != "python" || py.DisplayName != "Python 3" || py.Dir != filepath.Join(first, "kernels", "py") {
		t.Errorf("py: %+v", py)
	}
	if got := py.argv("/c.json"); !slices.Equal(got, []string{"python3", "-m", "ipykernel_launcher", "-f", "/c.json", "--dir=" + py.Dir}) {
		t.Errorf("argv %q", got)
	}
	if byName["ir"].InterruptMode != "message" || byName["ir"].Language != "r" {
		t.Errorf("ir: %+v", byName["ir"])
	}
}

// A kernel started runs code: each output message reaches out in order, and the reply says how the
// run ended. Its connection file is the client's own, private to the user.
func TestAKernelRunsCode(t *testing.T) {
	c := startFake(t, "")
	out, r := run(t, c, "print")
	if len(out) != 1 || out[0].Header.MsgType != "stream" || !strings.Contains(string(out[0].Content), `"text":"1\n"`) || r.Status != "ok" {
		t.Fatalf("print: %d outputs %+v, reply %+v", len(out), out, r)
	}
	out, r = run(t, c, "error")
	if len(out) != 1 || out[0].Header.MsgType != "error" || r.Status != "error" {
		t.Errorf("error: %+v, reply %+v", out, r)
	}
	out, _ = run(t, c, "png")
	if len(out) != 1 || out[0].Header.MsgType != "display_data" {
		t.Errorf("png: %+v", out)
	}
	if info, err := os.Stat(c.cur().connFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the connection file: %v %v; want it readable by the user alone", info, err)
	}
}

// Interrupt stops a run: by SIGINT to the kernel's process group, or by interrupt_request when its
// spec asks for messages. The run ends with the kernel's KeyboardInterrupt, and the kernel lives on.
func TestInterruptStopsARun(t *testing.T) {
	for _, mode := range []string{"signal", "message"} {
		t.Run(mode, func(t *testing.T) {
			c := startFake(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			started := make(chan struct{})
			var out []Message
			done := make(chan Reply, 1)
			go func() {
				r, err := c.Execute(ctx, "sleep", false, func(m Message) {
					out = append(out, m)
					if len(out) == 1 {
						close(started)
					}
				})
				if err != nil {
					t.Error(err)
				}
				done <- r
			}()
			<-started
			if err := c.Interrupt(); err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-done:
				if r.Status != "error" || len(out) != 2 || !strings.Contains(string(out[1].Content), "KeyboardInterrupt") {
					t.Errorf("interrupted: reply %+v, outputs %d", r, len(out))
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the run was not interrupted")
			}
			if _, r := run(t, c, "print"); r.Status != "ok" {
				t.Errorf("after the interrupt: %+v", r)
			}
		})
	}
}

// Restart starts the kernel afresh: what it held is gone, and it runs code again.
func TestRestartLosesTheKernelsState(t *testing.T) {
	c := startFake(t, "")
	run(t, c, "set")
	if out, _ := run(t, c, "get"); len(out) != 1 || !strings.Contains(string(out[0].Content), `"text":"1\n"`) {
		t.Fatalf("get before the restart: %+v", out)
	}
	before := c.cur().cmd.Process.Pid
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	if c.cur().cmd.Process.Pid == before {
		t.Error("the restart kept the process")
	}
	if out, r := run(t, c, "get"); r.Status != "error" || len(out) != 1 || !strings.Contains(string(out[0].Content), "NameError") {
		t.Errorf("get after the restart: %+v %+v; want x gone", out, r)
	}
}

// Shutdown ends the process and removes the connection file; a later request is ErrClosed.
func TestShutdownStopsTheKernel(t *testing.T) {
	c := startFake(t, "")
	s := c.cur()
	c.Shutdown()
	select {
	case <-s.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the process is still running")
	}
	if c.Alive() {
		t.Error("Alive after Shutdown")
	}
	if _, err := os.Stat(s.connFile); !os.IsNotExist(err) {
		t.Errorf("the connection file is still there: %v", err)
	}
	if _, err := c.Execute(context.Background(), "print", false, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("Execute after Shutdown: %v", err)
	}
	if err := c.Restart(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("Restart after Shutdown: %v", err)
	}
}

// A kernel that exits while starting is said with its stderr's end: the user learns what is missing.
func TestAKernelThatExitsSaysWhy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := Start(ctx, fakeSpec(t, "fake-kernel-dies", ""), t.TempDir(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "No module named 'ipykernel'") {
		t.Fatalf("Start: %v; want the kernel's own reason", err)
	}
}

// A kernel that never answers is given up after ReadyTimeout, its process killed.
func TestAKernelThatNeverAnswersTimesOut(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep(1) to stand for a silent kernel")
	}
	was := ReadyTimeout
	ReadyTimeout = 500 * time.Millisecond
	t.Cleanup(func() { ReadyTimeout = was })
	start := time.Now()
	_, err = Start(context.Background(), Spec{Name: "silent", Argv: []string{sleep, "60"}}, t.TempDir(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not answering") {
		t.Fatalf("Start: %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("gave up after %v", time.Since(start))
	}
}

// Against a real ipykernel when the machine has one (VM43's gate image; CI installs it): a print,
// an interrupt of a sleep, and a restart that loses a variable.
func TestARealIPyKernel(t *testing.T) {
	var spec *Spec
	for _, s := range Specs() {
		if s.Language == "python" {
			spec = &s
			break
		}
	}
	if spec == nil {
		t.Skip("no Python kernelspec on this machine (ipykernel is not installed)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := Start(ctx, *spec, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Shutdown)
	if out, r := run(t, c, "x = 41\nprint(x + 1)"); r.Status != "ok" || len(out) != 1 || !strings.Contains(string(out[0].Content), `42\n`) {
		t.Fatalf("print: %+v %+v", out, r)
	}
	done := make(chan Reply, 1)
	go func() {
		r, _ := c.Execute(ctx, "import time\ntime.sleep(30)", false, nil)
		done <- r
	}()
	time.Sleep(time.Second)
	if err := c.Interrupt(); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.Status != "error" || r.EName != "KeyboardInterrupt" {
			t.Errorf("interrupted sleep: %+v", r)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the sleep was not interrupted")
	}
	if err := c.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	if _, r := run(t, c, "x"); r.Status != "error" || r.EName != "NameError" {
		t.Errorf("x after the restart: %+v", r)
	}
}
