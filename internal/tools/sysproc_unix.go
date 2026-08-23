//go:build !windows

package tools

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the command in its own process group so timeout
// handling can signal the whole tree, not just the shell.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup signals the command's process group (negative pid).
func killProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}

// flockExclusive takes an exclusive advisory lock on the open file,
// blocking until acquired.
func flockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// flockRelease releases a lock taken by flockExclusive.
func flockRelease(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// killProcessGroupNeg signals a process group by negative pid.
func killProcessGroupNeg(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}
