package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestMutationPathLockHelper(t *testing.T) {
	if os.Getenv("SANDBAR_MUTATION_LOCK_HELPER") != "1" {
		return
	}
	path := os.Getenv("SANDBAR_MUTATION_LOCK_PATH")
	unlock, err := acquireMutationPathLock(path)
	if err != nil {
		t.Fatalf("acquire helper lock: %v", err)
	}
	defer unlock()
	if signal := os.Getenv("SANDBAR_MUTATION_LOCK_SIGNAL"); signal != "" {
		if err := os.WriteFile(signal, []byte("locked"), 0600); err != nil {
			t.Fatalf("signal acquired lock: %v", err)
		}
	}
	if hold := os.Getenv("SANDBAR_MUTATION_LOCK_HOLD"); hold != "" {
		duration, err := time.ParseDuration(hold)
		if err != nil {
			t.Fatalf("parse lock hold: %v", err)
		}
		time.Sleep(duration)
	}
}

func TestMutationPathLockCoordinatesProcesses(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "shared.txt")
	signal := filepath.Join(dir, "locked")
	helperEnv := []string{
		"SANDBAR_MUTATION_LOCK_HELPER=1",
		"SANDBAR_MUTATION_LOCK_PATH=" + target,
	}
	first := exec.Command(os.Args[0], "-test.run=^TestMutationPathLockHelper$")
	first.Env = append(os.Environ(), append(helperEnv,
		"SANDBAR_MUTATION_LOCK_SIGNAL="+signal,
		"SANDBAR_MUTATION_LOCK_HOLD=500ms",
	)...)
	if err := first.Start(); err != nil {
		t.Fatalf("start first lock helper: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(signal); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = first.Process.Kill()
			t.Fatal("first helper did not acquire the mutation lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	second := exec.Command(os.Args[0], "-test.run=^TestMutationPathLockHelper$")
	second.Env = append(os.Environ(), helperEnv...)
	started := time.Now()
	if output, err := second.CombinedOutput(); err != nil {
		t.Fatalf("second lock helper: %v\n%s", err, output)
	}
	if elapsed := time.Since(started); elapsed < 350*time.Millisecond {
		t.Fatalf("cross-process mutation lock waited only %v", elapsed)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("first lock helper: %v", err)
	}
}

func TestFileReadSuccess(t *testing.T) {
	ft := NewFileTools("../../tests/fixtures/_workspace")
	result, err := ft.FileRead(context.Background(), map[string]interface{}{
		"path": "main.go",
	})
	if err != nil {
		t.Fatalf("file_read: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty file content")
	}
	if !contains(result, "Hello, Sandbar!") {
		t.Errorf("content missing expected string: %s", result)
	}
}

func TestFileWriteCreationHonorsProcessUmask(t *testing.T) {
	workspace := t.TempDir()
	oldUmask := syscall.Umask(0077)
	defer syscall.Umask(oldUmask)

	_, err := NewFileTools(workspace).FileWrite(context.Background(), map[string]interface{}{
		"path":            "private.txt",
		"content":         "private",
		"expected_sha256": ExpectedFileAbsent,
	})
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	info, err := os.Stat(filepath.Join(workspace, "private.txt"))
	if err != nil {
		t.Fatalf("stat created file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("created mode = %04o, want umask-filtered 0600", got)
	}
}

func TestFileReadTraversalRejected(t *testing.T) {
	ft := NewFileTools("../../tests/fixtures/_workspace")
	_, err := ft.FileRead(context.Background(), map[string]interface{}{
		"path": "../config.valid.yaml",
	})
	if err == nil {
		t.Fatal("expected path traversal to be rejected")
	}
}

func TestFileReadAbsoluteAllowed(t *testing.T) {
	ft := NewFileTools("../../tests/fixtures/_workspace")
	_, err := ft.FileRead(context.Background(), map[string]interface{}{
		"path": "/etc/passwd",
	})
	if err != nil {
		t.Fatalf("expected absolute path to be allowed: %v", err)
	}
}

func TestFileWriteAndAppend(t *testing.T) {
	tmpDir := t.TempDir()
	ft := NewFileTools(tmpDir)

	// Write
	result, err := ft.FileWrite(context.Background(), map[string]interface{}{
		"path":            "test.txt",
		"content":         "hello",
		"expected_sha256": ExpectedFileAbsent,
	})
	if err != nil {
		t.Fatalf("file_write: %v", err)
	}
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "test.txt"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("content: got %q, want hello", string(data))
	}

	// Append
	result, err = ft.FileAppend(context.Background(), map[string]interface{}{
		"path":            "test.txt",
		"content":         " world",
		"expected_sha256": sha256Hex([]byte("hello")),
	})
	if err != nil {
		t.Fatalf("file_append: %v", err)
	}

	data, err = os.ReadFile(filepath.Join(tmpDir, "test.txt"))
	if err != nil {
		t.Fatalf("read back 2: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("content: got %q, want 'hello world'", string(data))
	}
	if !strings.Contains(result, "sha256: "+sha256Hex([]byte("hello world"))) {
		t.Errorf("mutation result missing new full SHA-256: %s", result)
	}
}

func TestFileReadTruncation(t *testing.T) {
	tmpDir := t.TempDir()
	ft := NewFileTools(tmpDir)

	// Write a large file.
	largeContent := make([]byte, defaultMaxBytes+100)
	for i := range largeContent {
		largeContent[i] = 'a'
	}
	os.WriteFile(filepath.Join(tmpDir, "large.txt"), largeContent, 0644)

	result, err := ft.FileRead(context.Background(), map[string]interface{}{
		"path": "large.txt",
	})
	if err != nil {
		t.Fatalf("file_read: %v", err)
	}
	if len(result) <= defaultMaxBytes {
		t.Errorf("expected truncated content > %d, got %d", defaultMaxBytes, len(result))
	}
	if !contains(result, "truncated") {
		t.Errorf("expected truncation marker in content")
	}
	if !strings.Contains(result, "[sha256: "+sha256Hex(largeContent)+"]") {
		preview := result
		if len(preview) > 120 {
			preview = preview[:120]
		}
		t.Errorf("truncated read did not expose full-content SHA-256: %s", preview)
	}
}

func TestFileReadMaxBytes(t *testing.T) {
	tmpDir := t.TempDir()
	ft := NewFileTools(tmpDir)
	os.WriteFile(filepath.Join(tmpDir, "small.txt"), []byte("hello world"), 0644)

	result, err := ft.FileRead(context.Background(), map[string]interface{}{
		"path":      "small.txt",
		"max_bytes": 5.0,
	})
	if err != nil {
		t.Fatalf("file_read: %v", err)
	}
	if !contains(result, "truncated") {
		t.Errorf("expected truncation with max_bytes: %s", result)
	}
}

func TestFileReadBinary(t *testing.T) {
	tmpDir := t.TempDir()
	ft := NewFileTools(tmpDir)
	binaryData := []byte{0x00, 0x01, 0xFF, 0xFE}
	os.WriteFile(filepath.Join(tmpDir, "bin.dat"), binaryData, 0644)

	result, err := ft.FileRead(context.Background(), map[string]interface{}{
		"path": "bin.dat",
	})
	if err != nil {
		t.Fatalf("file_read: %v", err)
	}
	if !contains(result, "binary output suppressed") {
		t.Errorf("expected binary suppression: %s", result)
	}
	if !strings.Contains(result, "[sha256: "+sha256Hex(binaryData)+"]") {
		t.Errorf("binary read did not expose full-content SHA-256: %s", result)
	}
}

func TestFileReadMissingPath(t *testing.T) {
	ft := NewFileTools(t.TempDir())
	_, err := ft.FileRead(context.Background(), map[string]interface{}{})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestFileWriteMissingPath(t *testing.T) {
	ft := NewFileTools(t.TempDir())
	_, err := ft.FileWrite(context.Background(), map[string]interface{}{"content": "x"})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestFileAppendMissingPath(t *testing.T) {
	ft := NewFileTools(t.TempDir())
	_, err := ft.FileAppend(context.Background(), map[string]interface{}{"content": "x"})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestFileReadMissingFile(t *testing.T) {
	ft := NewFileTools(t.TempDir())
	_, err := ft.FileRead(context.Background(), map[string]interface{}{"path": "nope.txt"})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestFileWriteRequiresExpectedSHA256(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "current.txt"), []byte("current"), 0644); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(workspace)
	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "write",
			call: func() error {
				_, err := ft.FileWrite(context.Background(), map[string]interface{}{
					"path": "new.txt", "content": "content",
				})
				return err
			},
		},
		{
			name: "append",
			call: func() error {
				_, err := ft.FileAppend(context.Background(), map[string]interface{}{
					"path": "current.txt", "content": "content",
				})
				return err
			},
		},
		{
			name: "patch",
			call: func() error {
				_, err := ft.FilePatch(context.Background(), map[string]interface{}{
					"path": "current.txt", "old_str": "current", "new_str": "changed",
				})
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if err == nil || !strings.Contains(err.Error(), "expected_sha256 is required") {
				t.Fatalf("expected required precondition error, got %v", err)
			}
		})
	}
}

func TestFileWriteCreateAndUpdateCAS(t *testing.T) {
	workspace := t.TempDir()
	ft := NewFileTools(workspace)
	path := filepath.Join(workspace, "nested", "value.txt")

	createResult, err := ft.FileWrite(context.Background(), map[string]interface{}{
		"path":            "nested/value.txt",
		"content":         "one",
		"expected_sha256": ExpectedFileAbsent,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	oneHash := sha256Hex([]byte("one"))
	if !strings.Contains(createResult, "sha256: "+oneHash) {
		t.Fatalf("create result missing hash %s: %s", oneHash, createResult)
	}

	updateResult, err := ft.FileWrite(context.Background(), map[string]interface{}{
		"path":            "nested/value.txt",
		"content":         "two",
		"expected_sha256": oneHash,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	twoHash := sha256Hex([]byte("two"))
	if !strings.Contains(updateResult, "sha256: "+twoHash) {
		t.Fatalf("update result missing hash %s: %s", twoHash, updateResult)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "two" {
		t.Fatalf("updated file = %q, %v; want two", data, err)
	}
}

func TestFileWriteMismatchReturnsTypedConflict(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "value.txt")
	if err := os.WriteFile(path, []byte("current secret body"), 0644); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(workspace)
	staleHash := sha256Hex([]byte("stale"))
	currentHash := sha256Hex([]byte("current secret body"))

	_, err := ft.FileWrite(context.Background(), map[string]interface{}{
		"path":            "value.txt",
		"content":         "replacement",
		"expected_sha256": staleHash,
	})
	if !errors.Is(err, ErrFileConflict) {
		t.Fatalf("expected ErrFileConflict, got %v", err)
	}
	var conflict *FileConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *FileConflictError, got %T", err)
	}
	if conflict.ExpectedSHA256 != staleHash || conflict.CurrentSHA256 != currentHash {
		t.Fatalf("conflict hashes = expected %q current %q", conflict.ExpectedSHA256, conflict.CurrentSHA256)
	}
	if strings.Contains(err.Error(), "current secret body") {
		t.Fatalf("conflict leaked file content: %v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || string(data) != "current secret body" {
		t.Fatalf("conflicting write modified file: %q, %v", data, readErr)
	}
}

func TestFileWriteExpectedAbsentConflictsWithExistingFile(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "value.txt"), []byte("present"), 0644); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(workspace)
	_, err := ft.FileWrite(context.Background(), map[string]interface{}{
		"path":            "value.txt",
		"content":         "replacement",
		"expected_sha256": ExpectedFileAbsent,
	})
	var conflict *FileConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if conflict.CurrentSHA256 != sha256Hex([]byte("present")) {
		t.Fatalf("current hash = %q", conflict.CurrentSHA256)
	}
}

func TestFileWritePreservesMode(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "script.sh")
	if err := os.WriteFile(path, []byte("old"), 0751); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0751); err != nil {
		t.Fatal(err)
	}

	ft := NewFileTools(workspace)
	_, err := ft.FileWrite(context.Background(), map[string]interface{}{
		"path":            "script.sh",
		"content":         "new",
		"expected_sha256": sha256Hex([]byte("old")),
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0751 {
		t.Fatalf("mode = %04o, want 0751", got)
	}
}

func TestConcurrentFileWritesWithSamePreconditionOnlyOneCommits(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "shared.txt")
	initial := []byte("initial")
	if err := os.WriteFile(path, initial, 0644); err != nil {
		t.Fatal(err)
	}
	expected := sha256Hex(initial)

	const writers = 32
	start := make(chan struct{})
	var successes atomic.Int32
	var conflicts atomic.Int32
	unexpected := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// Separate FileTools instances verify that serialization is global,
			// rather than accidentally scoped to one registry instance.
			ft := NewFileTools(workspace)
			_, err := ft.FileWrite(context.Background(), map[string]interface{}{
				"path":            "shared.txt",
				"content":         fmt.Sprintf("writer-%d", i),
				"expected_sha256": expected,
			})
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrFileConflict):
				conflicts.Add(1)
			default:
				unexpected <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(unexpected)
	if errValue, ok := <-unexpected; ok {
		t.Fatalf("unexpected mutation error: %v", errValue)
	}
	if got := successes.Load(); got != 1 {
		t.Fatalf("successful writes = %d, want 1", got)
	}
	if got := conflicts.Load(); got != writers-1 {
		t.Fatalf("conflicts = %d, want %d", got, writers-1)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(data), "writer-") {
		t.Fatalf("final content = %q, %v", data, err)
	}
}

func TestConcurrentFileAppendsAreCompareAndSwap(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "shared.txt")
	if err := os.WriteFile(path, []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	expected := sha256Hex([]byte("base"))
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, suffix := range []string{"-one", "-two"} {
		go func(suffix string) {
			<-start
			_, err := NewFileTools(workspace).FileAppend(context.Background(), map[string]interface{}{
				"path":            "shared.txt",
				"content":         suffix,
				"expected_sha256": expected,
			})
			results <- err
		}(suffix)
	}
	close(start)
	var successes, conflicts int
	for range 2 {
		err := <-results
		if err == nil {
			successes++
		} else if errors.Is(err, ErrFileConflict) {
			conflicts++
		} else {
			t.Fatalf("append: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d; want 1 and 1", successes, conflicts)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "base-one" && got != "base-two" {
		t.Fatalf("append was not an atomic read-modify-write: %q", got)
	}
}

func TestFileWriteAbsolutePathStillAllowed(t *testing.T) {
	workspace := t.TempDir()
	targetDir := t.TempDir()
	target := filepath.Join(targetDir, "absolute.txt")
	ft := NewFileTools(workspace)
	_, err := ft.FileWrite(context.Background(), map[string]interface{}{
		"path":            target,
		"content":         "absolute",
		"expected_sha256": ExpectedFileAbsent,
	})
	if err != nil {
		t.Fatalf("absolute write: %v", err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "absolute" {
		t.Fatalf("absolute content = %q, %v", data, err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsInternal(s, substr))
}

func containsInternal(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestFileToolsHonorContextWorkspace(t *testing.T) {
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	ft := NewFileTools(workspaceA)
	ctx := WithWorkspace(context.Background(), workspaceB)

	// Write a relative path: it must land in B, not the construction workspace A.
	if _, err := ft.FileWrite(ctx, map[string]interface{}{
		"path":            "ctx.txt",
		"content":         "in-b",
		"expected_sha256": ExpectedFileAbsent,
	}); err != nil {
		t.Fatalf("file_write with ctx workspace: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workspaceB, "ctx.txt"))
	if err != nil {
		t.Fatalf("read back from B: %v", err)
	}
	if string(data) != "in-b" {
		t.Errorf("content: got %q, want in-b", string(data))
	}
	if _, err := os.Stat(filepath.Join(workspaceA, "ctx.txt")); !os.IsNotExist(err) {
		t.Errorf("write leaked into construction workspace A: %v", err)
	}

	// Read the same relative path back through the ctx workspace.
	out, err := ft.FileRead(ctx, map[string]interface{}{"path": "ctx.txt"})
	if err != nil {
		t.Fatalf("file_read with ctx workspace: %v", err)
	}
	if !strings.Contains(out, "in-b") {
		t.Errorf("file_read output missing B content: %s", out)
	}

	// Patch also resolves through the ctx workspace.
	if _, err := ft.FilePatch(ctx, map[string]interface{}{
		"path":            "ctx.txt",
		"old_str":         "in-b",
		"new_str":         "patched",
		"expected_sha256": sha256Hex([]byte("in-b")),
	}); err != nil {
		t.Fatalf("file_patch with ctx workspace: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(workspaceB, "ctx.txt"))
	if err != nil {
		t.Fatalf("read patched file: %v", err)
	}
	if string(data) != "patched" {
		t.Errorf("patched content: got %q, want patched", string(data))
	}
}

func TestFileToolsContextWorkspaceStaysJailed(t *testing.T) {
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	ft := NewFileTools(workspaceA)
	ctx := WithWorkspace(context.Background(), workspaceB)

	for _, tc := range []string{"../escape.txt", "sub/../../escape.txt"} {
		if _, err := ft.FileWrite(ctx, map[string]interface{}{
			"path":            tc,
			"content":         "escape",
			"expected_sha256": ExpectedFileAbsent,
		}); err == nil {
			t.Errorf("file_write(%q) escaped the ctx workspace jail", tc)
		}
	}
	if _, err := ft.FileRead(ctx, map[string]interface{}{"path": "../escape.txt"}); err == nil {
		t.Error("file_read escaped the ctx workspace jail")
	}
}

func TestFileToolsFallBackToConstructionWorkspace(t *testing.T) {
	workspaceA := t.TempDir()
	ft := NewFileTools(workspaceA)

	if _, err := ft.FileWrite(context.Background(), map[string]interface{}{
		"path":            "plain.txt",
		"content":         "in-a",
		"expected_sha256": ExpectedFileAbsent,
	}); err != nil {
		t.Fatalf("file_write without ctx workspace: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workspaceA, "plain.txt"))
	if err != nil {
		t.Fatalf("read back from A: %v", err)
	}
	if string(data) != "in-a" {
		t.Errorf("content: got %q, want in-a", string(data))
	}
}
