package tools

import (
	"fmt"
	"strings"
	"time"
)

// SSHRuntimeConfig controls remote shell_exec execution. When the model sets
// the `host` argument, the harness owns the ssh transport and POSIX quoting so
// the model passes a plain command and never composes nested ssh/python/shell
// quoting.
type SSHRuntimeConfig struct {
	ConnectTimeout time.Duration
	BatchMode      bool
	AllowedHosts   []string // empty = any host
}

// DefaultSSHRuntimeConfig is applied when no config is supplied.
func DefaultSSHRuntimeConfig() SSHRuntimeConfig {
	return SSHRuntimeConfig{
		ConnectTimeout: 5 * time.Second,
		BatchMode:      true,
	}
}

// validateRemoteHost rejects hosts that cannot be a legitimate ssh target:
// empty, leading "-" (ssh would parse it as an option — argument-injection
// guard), or whitespace. When allowedHosts is non-empty the host must match
// an entry exactly.
func validateRemoteHost(host string, allowedHosts []string) error {
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("remote host is required")
	}
	if strings.HasPrefix(host, "-") {
		return fmt.Errorf("invalid remote host %q: an ssh destination must not begin with \"-\"", host)
	}
	for _, r := range host {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return fmt.Errorf("invalid remote host %q: whitespace is not allowed", host)
		}
	}
	if len(allowedHosts) > 0 {
		for _, allowed := range allowedHosts {
			if host == allowed {
				return nil
			}
		}
		return fmt.Errorf("remote host %q is not in allowed_hosts", host)
	}
	return nil
}

// quotePosixPath single-quotes a value for a POSIX shell, escaping embedded
// single quotes.
func quotePosixPath(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// wrapInPosixShell wraps a command so it runs under bash on the remote host
// regardless of the remote login shell.
func wrapInPosixShell(command string) string {
	return "bash -c " + quotePosixPath(command)
}

// buildRemoteArgv constructs the ssh argv for remote execution. The command
// is a single argv element handed to ssh (which passes it to the remote login
// shell as its -c argument); no local "bash -c" re-parses it.
func buildRemoteArgv(host, command string, cfg SSHRuntimeConfig) []string {
	timeoutSec := int(cfg.ConnectTimeout.Seconds())
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	argv := []string{
		"ssh",
		"-o", fmt.Sprintf("ConnectTimeout=%d", timeoutSec),
		"-o", "BatchMode=yes",
		host,
		wrapInPosixShell(command),
	}
	if !cfg.BatchMode {
		// Keep the default yes unless explicitly disabled; BatchMode is the
		// only interactive prompt switch the agent loop can tolerate.
		argv = []string{
			"ssh",
			"-o", fmt.Sprintf("ConnectTimeout=%d", timeoutSec),
			host,
			wrapInPosixShell(command),
		}
	}
	return argv
}
