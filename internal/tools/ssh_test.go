package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuotePosixPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''"},
		{"echo hi", "'echo hi'"},
		{"echo it's fine", `'echo it'\''s fine'`},
		{"a'b'c", `'a'\''b'\''c'`},
		{"line1\nline2", "'line1\nline2'"},
	}
	for _, c := range cases {
		if got := quotePosixPath(c.in); got != c.want {
			t.Errorf("quotePosixPath(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestWrapInPosixShell(t *testing.T) {
	if got, want := wrapInPosixShell("echo hi"), "bash -c 'echo hi'"; got != want {
		t.Errorf("wrapInPosixShell: got %q want %q", got, want)
	}
}

func TestBuildRemoteArgv(t *testing.T) {
	cfg := DefaultSSHRuntimeConfig()
	argv := buildRemoteArgv("some-host", "sudo ls", cfg)
	want := []string{
		"ssh",
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		"some-host",
		"bash -c 'sudo ls'",
	}
	if len(argv) != len(want) {
		t.Fatalf("argv: got %v want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv: got %v want %v", argv, want)
		}
	}

	// Sub-second connect timeouts floor at 1s.
	cfg.ConnectTimeout = 200 * time.Millisecond
	if argv := buildRemoteArgv("h", "true", cfg); argv[2] != "ConnectTimeout=1" {
		t.Errorf("sub-second timeout: got %q", argv[2])
	}

	// Batch mode disabled drops the -o BatchMode pair.
	cfg = DefaultSSHRuntimeConfig()
	cfg.BatchMode = false
	argv = buildRemoteArgv("h", "true", cfg)
	if strings.Contains(strings.Join(argv, " "), "BatchMode") {
		t.Errorf("batch mode disabled but BatchMode present: %v", argv)
	}
	if argv[len(argv)-1] != "bash -c 'true'" {
		t.Errorf("command element: got %q", argv[len(argv)-1])
	}
}

func TestValidateRemoteHost(t *testing.T) {
	valid := []string{"some-host", "user@host", "192.0.2.5", "host.example.com"}
	for _, h := range valid {
		if err := validateRemoteHost(h, nil); err != nil {
			t.Errorf("valid host %q rejected: %v", h, err)
		}
	}
	invalid := []string{"", "   ", "-oProxyCommand=x", "has space", "tab\there"}
	for _, h := range invalid {
		if err := validateRemoteHost(h, nil); err == nil {
			t.Errorf("invalid host %q accepted", h)
		}
	}
	// Allowlist: exact match only.
	if err := validateRemoteHost("other", []string{"some-host"}); err == nil {
		t.Error("host outside allowlist accepted")
	}
	if err := validateRemoteHost("some-host", []string{"some-host"}); err != nil {
		t.Errorf("allowlisted host rejected: %v", err)
	}
}

// installFakeSSH puts a fake ssh executable first on PATH that records its
// argv (one element per line) into the file named by SSH_ARGV_LOG, prints a
// fixed stdout, and exits with SSH_EXIT (default 0). Returns a function
// reading the recorded argv.
func installFakeSSH(t *testing.T) func() []string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "argv.log")
	script := `#!/bin/sh
printf '%s\n' "$@" >> "$SSH_ARGV_LOG"
echo "remote-stdout"
exit "${SSH_EXIT:-0}"
`
	sshPath := filepath.Join(dir, "ssh")
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SSH_ARGV_LOG", logPath)
	return func() []string {
		raw, err := os.ReadFile(logPath)
		if err != nil {
			return nil
		}
		if len(raw) == 0 {
			return nil
		}
		return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	}
}

func TestRemoteShellExecRunsSshWithDirectArgv(t *testing.T) {
	readArgv := installFakeSSH(t)
	se := NewShellExec(t.TempDir(), nil)

	out, err := se.Execute(context.Background(), map[string]interface{}{
		"command": "echo hello $HOME",
		"host":    "some-host",
	})
	if err != nil {
		t.Fatalf("remote exec: %v", err)
	}
	if !strings.Contains(out, "Exit code: 0") || !strings.Contains(out, "remote-stdout") {
		t.Fatalf("unexpected output: %q", out)
	}
	argv := readArgv()
	// The shim records "$@" — the script's arguments, starting after argv[0]
	// ("ssh" itself, resolved from PATH).
	want := []string{
		"-o", "ConnectTimeout=5",
		"-o", "BatchMode=yes",
		"some-host",
		"bash -c 'echo hello $HOME'",
	}
	if len(argv) != len(want) {
		t.Fatalf("argv: got %v want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv: got %v want %v", argv, want)
		}
	}
	// The command travels as ONE argv element with $HOME unexpanded: no local
	// shell layer parsed it.
	if argv[len(argv)-1] != "bash -c 'echo hello $HOME'" {
		t.Fatalf("command element was locally parsed: %q", argv[len(argv)-1])
	}
}

func TestRemoteShellExecQuotesEmbeddedSingleQuotes(t *testing.T) {
	readArgv := installFakeSSH(t)
	se := NewShellExec(t.TempDir(), nil)
	_, err := se.Execute(context.Background(), map[string]interface{}{
		"command": "echo it's fine",
		"host":    "some-host",
	})
	if err != nil {
		t.Fatalf("remote exec: %v", err)
	}
	argv := readArgv()
	if want := "bash -c 'echo it'\\''s fine'"; len(argv) == 0 || argv[len(argv)-1] != want {
		t.Fatalf("quoted command element: got %q want %q", argv[len(argv)-1], want)
	}
}

func TestRemoteShellExecExitCodePropagates(t *testing.T) {
	installFakeSSH(t)
	t.Setenv("SSH_EXIT", "42")
	se := NewShellExec(t.TempDir(), nil)
	out, err := se.Execute(context.Background(), map[string]interface{}{
		"command": "false",
		"host":    "some-host",
	})
	if err != nil {
		t.Fatalf("remote exec: %v", err)
	}
	if !strings.Contains(out, "Exit code: 42") {
		t.Fatalf("exit code not propagated: %q", out)
	}
}

func TestRemoteShellExecBlockedCommandRejectedBeforeSsh(t *testing.T) {
	readArgv := installFakeSSH(t)
	se := NewShellExec(t.TempDir(), []string{"rm -rf /"})
	_, err := se.Execute(context.Background(), map[string]interface{}{
		"command": "rm -rf /",
		"host":    "some-host",
	})
	if err == nil || !strings.Contains(err.Error(), "blocked by safety policy") {
		t.Fatalf("expected blocklist rejection, got: %v", err)
	}
	if argv := readArgv(); argv != nil {
		t.Fatalf("ssh must not run for a blocked command, argv=%v", argv)
	}
}

func TestRemoteShellExecSudoNeverBlocked(t *testing.T) {
	readArgv := installFakeSSH(t)
	se := NewShellExec(t.TempDir(), []string{"sudo"}) // drift: sudo in blocklist
	_, err := se.Execute(context.Background(), map[string]interface{}{
		"command": "sudo ls",
		"host":    "some-host",
	})
	if err != nil {
		t.Fatalf("sudo must never be blocked, even remotely: %v", err)
	}
	argv := readArgv()
	if len(argv) == 0 || argv[len(argv)-1] != "bash -c 'sudo ls'" {
		t.Fatalf("unexpected argv: %v", argv)
	}
}

func TestRemoteShellExecAsyncRejected(t *testing.T) {
	se := NewShellExec(t.TempDir(), nil)
	_, err := se.Execute(context.Background(), map[string]interface{}{
		"command": "true",
		"host":    "some-host",
		"async":   true,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot target remote hosts") {
		t.Fatalf("expected remote async rejection, got: %v", err)
	}
}

func TestRemoteShellExecAllowedHostsEnforced(t *testing.T) {
	readArgv := installFakeSSH(t)
	se := NewShellExec(t.TempDir(), nil)
	if err := se.SetSSHConfig(SSHRuntimeConfig{
		ConnectTimeout: 5 * time.Second,
		BatchMode:      true,
		AllowedHosts:   []string{"allowed-host"},
	}); err != nil {
		t.Fatalf("set ssh config: %v", err)
	}
	_, err := se.Execute(context.Background(), map[string]interface{}{
		"command": "true",
		"host":    "other-host",
	})
	if err == nil || !strings.Contains(err.Error(), "not in allowed_hosts") {
		t.Fatalf("expected allowlist rejection, got: %v", err)
	}
	if _, err := se.Execute(context.Background(), map[string]interface{}{
		"command": "true",
		"host":    "allowed-host",
	}); err != nil {
		t.Fatalf("allowlisted host rejected: %v", err)
	}
	if argv := readArgv(); argv == nil || argv[4] != "allowed-host" {
		t.Fatalf("unexpected argv: %v", argv)
	}
}

func TestRemoteShellExecRejectsHostOptionInjection(t *testing.T) {
	se := NewShellExec(t.TempDir(), nil)
	_, err := se.Execute(context.Background(), map[string]interface{}{
		"command": "true",
		"host":    "-oProxyCommand=evil",
	})
	if err == nil || !strings.Contains(err.Error(), "must not begin with") {
		t.Fatalf("expected host injection rejection, got: %v", err)
	}
}
