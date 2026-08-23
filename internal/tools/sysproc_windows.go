//go:build windows

package tools

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup is a no-op on Windows: there is no POSIX process group,
// so timeout handling falls back to killing the direct child.
func setProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup terminates the direct child process on Windows.
func killProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	_ = sig
	return cmd.Process.Kill()
}

// flockExclusive is a no-op on Windows: the cross-process advisory mutation
// lock degrades to the in-process mutex only. Single-user CLI use makes this
// acceptable; the SHA-256 precondition still guards against stale writes.
func flockExclusive(f *os.File) error { return nil }

// flockRelease releases the advisory lock held by flockExclusive.
func flockRelease(f *os.File) error { return nil }

// killProcessGroupNeg terminates the direct child by pid on Windows.
func killProcessGroupNeg(pid int, sig syscall.Signal) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	_ = sig
	return proc.Kill()
}
