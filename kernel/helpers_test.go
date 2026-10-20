package kernel_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/yongjohnlee80/autodoc-jupyter/internal/fakekernel"
	"github.com/yongjohnlee80/autodoc-jupyter/kernel"
)

// The cells' kernel is this test binary, run as the fake kernel (internal/fakekernel).
func TestMain(m *testing.M) {
	fakekernel.Dispatch()
	os.Exit(m.Run())
}

// fakeSpec is the fake kernel's spec in mode.
func fakeSpec(t *testing.T, mode fakekernel.Mode) kernel.Spec {
	t.Helper()
	s, err := fakekernel.Spec(mode, "fake")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// startFake starts the fake kernel, shut down when the test ends: interrupted by interrupt_request
// when interruptMode is "message", else by SIGINT.
func startFake(t *testing.T, interruptMode string) *kernel.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mode := fakekernel.Signal
	if interruptMode == "message" {
		mode = fakekernel.Message
	}
	c, err := kernel.Start(ctx, fakeSpec(t, mode), t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Shutdown)
	return c
}

// run executes code on c, and returns its outputs with the reply.
func run(t *testing.T, c *kernel.Client, code string) ([]kernel.Message, kernel.Reply) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out []kernel.Message
	r, err := c.Execute(ctx, code, false, func(m kernel.Message) { out = append(out, m) })
	if err != nil {
		t.Fatalf("%s: %v", code, err)
	}
	return out, r
}
