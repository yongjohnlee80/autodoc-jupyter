//go:build !unix

package kernel

import (
	"errors"
	"os/exec"
)

func ownGroup(*exec.Cmd) {}

// interrupt is a signal-mode interrupt, which only a Unix process group takes.
func interrupt(*exec.Cmd) error {
	return errors.New("kernel: this kernel interrupts by signal, which needs a Unix system")
}

func kill(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
