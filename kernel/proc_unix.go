//go:build unix

package kernel

import (
	"os/exec"
	"syscall"
)

// ownGroup starts the kernel in a process group of its own, so SIGINT reaches it (and what it
// started) and nothing of the plugin's.
func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// interrupt sends SIGINT to the kernel's process group.
func interrupt(cmd *exec.Cmd) error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT) }

// kill ends the kernel's process group.
func kill(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
