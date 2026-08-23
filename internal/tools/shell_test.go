package tools

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestShellExecEcho(t *testing.T) {
	workspace := t.TempDir()
	se := NewShellExec(workspace, nil)
	out, err := se.Execute(context.Background(), map[string]interface{}{
		"command": "echo hello",
	})
	if err != nil {
		t.Fatalf("shell exec: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("output missing hello: %s", out)
	}
}

func TestShellExecCapturesFastOutput(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	workspace := t.TempDir()
	se := NewShellExec(workspace, nil)
	const marker = "fast-output-marker"
	for i := 0; i < 1000; i++ {
		out, err := se.Execute(context.Background(), map[string]interface{}{
			"command": "printf " + marker,
		})
		if err != nil {
			t.Fatalf("iteration %d: shell exec: %v", i, err)
		}
		if !strings.Contains(out, marker) {
			t.Fatalf("iteration %d: output missing %q: %s", i, marker, out)
		}
	}
}

func TestShellExecBlocked(t *testing.T) {
	workspace := t.TempDir()
	se := NewShellExec(workspace, nil)
	_, err := se.Execute(context.Background(), map[string]interface{}{
		"command": "rm -rf /",
	})
	if err == nil {
		t.Fatal("expected error for blocked command")
	}
}

// TestCheckBlockedCommandWord proves the blocklist matches the command word of
// a segment, not any substring of the command line. "forbidden" stands in for
// any blockable command word — sudo is never a valid block target (see
// TestCheckBlockedSudoNeverBlocked).
func TestCheckBlockedCommandWord(t *testing.T) {
	se := NewShellExec(t.TempDir(), []string{"forbidden"})

	blocked := []string{
		"forbidden ls",
		"echo hi && forbidden ls",
		"forbidden rm -rf /",
	}
	for _, cmd := range blocked {
		if err := se.checkBlocked(cmd); err == nil {
			t.Errorf("expected %q to be blocked (forbidden is the command word)", cmd)
		}
	}

	allowed := []string{
		"which forbidden",        // forbidden is an argument
		"echo forbidden",         // forbidden is an argument
		"grep forbidden /etc/os", // forbidden is an argument
		"cat /docs/forbidden.md", // "forbidden" is inside a path
		"ls -la forbiddeners",    // "forbidden" is a substring of an argument
	}
	for _, cmd := range allowed {
		if err := se.checkBlocked(cmd); err != nil {
			t.Errorf("expected %q NOT to be blocked, got: %v", cmd, err)
		}
	}
}

// TestCheckBlockedSudoNeverBlocked enforces the product rule that sudo can
// never be blocked: even if "sudo" is present in the configured blocklist
// (drift, template regen, agent edits), sudo commands stay runnable.
func TestCheckBlockedSudoNeverBlocked(t *testing.T) {
	se := NewShellExec(t.TempDir(), []string{"sudo"})
	for _, cmd := range []string{"sudo ls", "sudo cp a b", "sudo systemctl restart x"} {
		if err := se.checkBlocked(cmd); err != nil {
			t.Errorf("sudo must never be blocked, but %q was: %v", cmd, err)
		}
	}
}

// TestCheckBlockedMultiWordEntry proves multi-word entries match exact leading
// tokens, so "rm -rf /" no longer false-positives on "rm -rf /tmp/x".
func TestCheckBlockedMultiWordEntry(t *testing.T) {
	se := NewShellExec(t.TempDir(), []string{"rm -rf /", "chmod 777"})

	blocked := []string{
		"rm -rf /",
		"rm -rf / ; echo done",
		"chmod 777 /tmp/x",
	}
	for _, cmd := range blocked {
		if err := se.checkBlocked(cmd); err == nil {
			t.Errorf("expected %q to be blocked", cmd)
		}
	}

	allowed := []string{
		"rm -rf /tmp/x",   // different target — the old substring false positive
		"rm -rf /home",    // not bare root
		"rm -rf",          // no target
		"chmod 755 x",     // not the 777 form
		"echo 'rm -rf /'", // quoted argument to echo, not the rm command
		"chmod",           // bare command, no args
	}
	for _, cmd := range allowed {
		if err := se.checkBlocked(cmd); err != nil {
			t.Errorf("expected %q NOT to be blocked, got: %v", cmd, err)
		}
	}
}

// TestCheckBlockedFallback covers the empty-config defaults and confirms sudo
// is no longer blocked by the fallback.
func TestCheckBlockedFallback(t *testing.T) {
	se := NewShellExec(t.TempDir(), nil) // nil → fallback defaults
	if err := se.checkBlocked("rm -rf /"); err == nil {
		t.Error("fallback should block rm -rf /")
	}
	if err := se.checkBlocked("rm -rf /var/log/foo"); err != nil {
		t.Errorf("fallback should NOT block rm -rf /var/log/foo, got: %v", err)
	}
	if err := se.checkBlocked("sudo ls"); err != nil {
		t.Errorf("fallback must not block sudo (sudo is allowed), got: %v", err)
	}
}

func TestShellExecTimeout(t *testing.T) {
	workspace := t.TempDir()
	se := NewShellExec(workspace, nil)
	_, err := se.Execute(context.Background(), map[string]interface{}{
		"command":         "sleep 10",
		"timeout_seconds": 1,
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestShellExecExitCode(t *testing.T) {
	workspace := t.TempDir()
	se := NewShellExec(workspace, nil)
	out, err := se.Execute(context.Background(), map[string]interface{}{
		"command": "exit 42",
	})
	if err != nil {
		t.Fatalf("shell exec: %v", err)
	}
	if !strings.Contains(out, "Exit code: 42") {
		t.Errorf("output missing exit code: %s", out)
	}
}

func TestShellExecStderr(t *testing.T) {
	workspace := t.TempDir()
	se := NewShellExec(workspace, nil)
	out, err := se.Execute(context.Background(), map[string]interface{}{
		"command": "echo err >&2; exit 1",
	})
	if err != nil {
		t.Fatalf("shell exec: %v", err)
	}
	if !strings.Contains(out, "Stderr:") {
		t.Errorf("output missing stderr section: %s", out)
	}
	if !strings.Contains(out, "err") {
		t.Errorf("output missing stderr content: %s", out)
	}
}

func TestShellExecCancel(t *testing.T) {
	workspace := t.TempDir()
	se := NewShellExec(workspace, nil)

	// Start a long-running command in a goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	var execErr error
	go func() {
		_, execErr = se.Execute(ctx, map[string]interface{}{
			"command":         "sleep 100",
			"timeout_seconds": 2,
		})
		close(done)
	}()

	// Give cmd.Start() time to run, then cancel.
	time.Sleep(200 * time.Millisecond)
	se.Cancel()

	<-done
	if execErr == nil {
		t.Fatal("expected error after cancellation")
	}
}
