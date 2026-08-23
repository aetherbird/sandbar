package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// ExpectedFileAbsent is the expected_sha256 sentinel used when a mutation is
// intended to create a new file. It deliberately cannot be confused with a
// SHA-256 digest.
const ExpectedFileAbsent = "absent"

// ErrFileConflict identifies an optimistic-concurrency conflict. Callers can
// use errors.Is without having to inspect an error message.
var ErrFileConflict = errors.New("file changed since it was read")

// FileConflictError reports a failed SHA-256 precondition without exposing the
// file's contents. CurrentSHA256 is ExpectedFileAbsent when the path does not
// currently exist.
type FileConflictError struct {
	Path           string
	ExpectedSHA256 string
	CurrentSHA256  string
}

func (e *FileConflictError) Error() string {
	return fmt.Sprintf(
		"file edit conflict for %q: expected_sha256 is %q, current sha256 is %q; re-read the file and retry with the latest sha256",
		e.Path,
		e.ExpectedSHA256,
		e.CurrentSHA256,
	)
}

// Unwrap allows errors.Is(err, ErrFileConflict).
func (e *FileConflictError) Unwrap() error { return ErrFileConflict }

type fileSnapshot struct {
	content []byte
	mode    os.FileMode
	exists  bool
	hash    string
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func readFileSnapshot(path string) (fileSnapshot, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{hash: ExpectedFileAbsent}, nil
	}
	if err != nil {
		return fileSnapshot{}, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fileSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return fileSnapshot{}, fmt.Errorf("path is not a regular file")
	}

	content, err := io.ReadAll(f)
	if err != nil {
		return fileSnapshot{}, err
	}
	return fileSnapshot{
		content: content,
		mode:    info.Mode(),
		exists:  true,
		hash:    sha256Hex(content),
	}, nil
}

func expectedSHA256(args map[string]interface{}) (string, error) {
	raw, ok := args["expected_sha256"].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("expected_sha256 is required; use %q only when creating a new file", ExpectedFileAbsent)
	}

	expected := strings.ToLower(strings.TrimSpace(raw))
	if expected == ExpectedFileAbsent {
		return expected, nil
	}
	if len(expected) != sha256.Size*2 {
		return "", fmt.Errorf("expected_sha256 must be a 64-character SHA-256 digest or %q", ExpectedFileAbsent)
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return "", fmt.Errorf("expected_sha256 must be a hexadecimal SHA-256 digest or %q", ExpectedFileAbsent)
	}
	return expected, nil
}

func checkFilePrecondition(displayPath, expected string, current fileSnapshot) error {
	if expected == current.hash {
		return nil
	}
	return &FileConflictError{
		Path:           displayPath,
		ExpectedSHA256: expected,
		CurrentSHA256:  current.hash,
	}
}

// canonicalMutationPath resolves symlinks in the existing portion of path.
// This both gives aliases the same in-process lock and retains the historical
// behavior of writes through a symlink targeting the symlink's destination.
func canonicalMutationPath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve mutation path: %w", err)
	}
	absPath = filepath.Clean(absPath)

	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		return resolved, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("resolve mutation path: %w", err)
	}

	// Resolve the nearest existing ancestor so two spellings through a symlink
	// still serialize when the leaf (or one of its parent directories) is new.
	ancestor := filepath.Dir(absPath)
	suffix := []string{filepath.Base(absPath)}
	for {
		resolved, resolveErr := filepath.EvalSymlinks(ancestor)
		if resolveErr == nil {
			parts := append([]string{resolved}, reverseStrings(suffix)...)
			return filepath.Join(parts...), nil
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return "", fmt.Errorf("resolve mutation path: %w", resolveErr)
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return absPath, nil
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = parent
	}
}

func reverseStrings(values []string) []string {
	reversed := make([]string, len(values))
	for i := range values {
		reversed[len(values)-1-i] = values[i]
	}
	return reversed
}

type mutationLockEntry struct {
	mu   sync.Mutex
	refs int
}

var mutationPathLocks struct {
	sync.Mutex
	entries map[string]*mutationLockEntry
}

func acquireMutationPathLock(path string) (func(), error) {
	mutationPathLocks.Lock()
	if mutationPathLocks.entries == nil {
		mutationPathLocks.entries = make(map[string]*mutationLockEntry)
	}
	entry := mutationPathLocks.entries[path]
	if entry == nil {
		entry = &mutationLockEntry{}
		mutationPathLocks.entries[path] = entry
	}
	entry.refs++
	mutationPathLocks.Unlock()

	entry.mu.Lock()
	releaseLocal := func() {
		entry.mu.Unlock()
		mutationPathLocks.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(mutationPathLocks.entries, path)
		}
		mutationPathLocks.Unlock()
	}

	// Coordinate Sandbar processes as well as goroutines. The lock file is
	// keyed by the canonical target path and contains no user data. External
	// editors do not participate in this advisory lock, so the live SHA is still
	// revalidated immediately before commit below.
	lockDir := filepath.Join(os.TempDir(), fmt.Sprintf("sandbar-file-locks-%d", os.Getuid()))
	if err := os.MkdirAll(lockDir, 0700); err != nil {
		releaseLocal()
		return nil, fmt.Errorf("create mutation lock directory: %w", err)
	}
	pathHash := sha256.Sum256([]byte(path))
	lockPath := filepath.Join(lockDir, hex.EncodeToString(pathHash[:])+".lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		releaseLocal()
		return nil, fmt.Errorf("open mutation lock: %w", err)
	}
	if err := flockExclusive(lockFile); err != nil {
		_ = lockFile.Close()
		releaseLocal()
		return nil, fmt.Errorf("lock mutation path: %w", err)
	}
	return func() {
		_ = flockRelease(lockFile)
		_ = lockFile.Close()
		releaseLocal()
	}, nil
}

// mutateFile performs a content-addressed read-modify-write. transform runs
// only after the caller's precondition succeeds. The precondition is checked
// again immediately before the atomic rename so stale writes cannot silently
// overwrite another mutation made through FileTools.
func mutateFile(
	ctx context.Context,
	resolvedPath string,
	displayPath string,
	expected string,
	allowCreate bool,
	transform func(fileSnapshot) ([]byte, error),
) (before fileSnapshot, afterHash string, err error) {
	canonicalPath, err := canonicalMutationPath(resolvedPath)
	if err != nil {
		return fileSnapshot{}, "", err
	}
	unlock, err := acquireMutationPathLock(canonicalPath)
	if err != nil {
		return fileSnapshot{}, "", err
	}
	defer unlock()

	if err := ctx.Err(); err != nil {
		return fileSnapshot{}, "", err
	}

	before, err = readFileSnapshot(canonicalPath)
	if err != nil {
		return fileSnapshot{}, "", fmt.Errorf("read file: %w", err)
	}
	if err := checkFilePrecondition(displayPath, expected, before); err != nil {
		return fileSnapshot{}, "", err
	}
	if !before.exists && !allowCreate {
		return fileSnapshot{}, "", fmt.Errorf("read file: %w", os.ErrNotExist)
	}

	newContent, err := transform(before)
	if err != nil {
		return fileSnapshot{}, "", err
	}
	if err := ctx.Err(); err != nil {
		return fileSnapshot{}, "", err
	}

	if allowCreate {
		if err := os.MkdirAll(filepath.Dir(canonicalPath), 0755); err != nil {
			return fileSnapshot{}, "", fmt.Errorf("create directory: %w", err)
		}
	}

	mode := os.FileMode(0644)
	respectUmask := true
	if before.exists {
		mode = before.mode
		respectUmask = false
	}
	latest := before
	commitErr := atomicReplaceFile(canonicalPath, newContent, mode, respectUmask, func() (os.FileMode, error) {
		// The potentially slow temp-file write and fsync have already completed;
		// revalidate the target now, immediately before the atomic rename.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		current, readErr := readFileSnapshot(canonicalPath)
		if readErr != nil {
			return 0, fmt.Errorf("revalidate file: %w", readErr)
		}
		if checkErr := checkFilePrecondition(displayPath, expected, current); checkErr != nil {
			return 0, checkErr
		}
		if !current.exists && !allowCreate {
			return 0, fmt.Errorf("revalidate file: %w", os.ErrNotExist)
		}
		latest = current
		if current.exists {
			return current.mode, nil
		}
		return 0, nil // retain the temp file's umask-filtered creation mode
	})
	if commitErr != nil {
		if errors.Is(commitErr, ErrFileConflict) {
			return fileSnapshot{}, "", commitErr
		}
		return fileSnapshot{}, "", fmt.Errorf("write file: %w", commitErr)
	}
	return latest, sha256Hex(newContent), nil
}

func atomicReplaceFile(
	path string,
	content []byte,
	mode os.FileMode,
	respectUmask bool,
	revalidate func() (os.FileMode, error),
) error {
	dir := filepath.Dir(path)
	tmpPath := filepath.Join(dir, ".sandbar-edit-"+uuid.NewString())
	createMode := os.FileMode(0600)
	if respectUmask {
		createMode = mode
	}
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, createMode)
	if err != nil {
		return err
	}
	keepTemp := true
	defer func() {
		if keepTemp {
			_ = os.Remove(tmpPath)
		}
	}()

	// Keep all permission and special mode bits that chmod can preserve.
	mode = mode.Perm() | mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	// Chmod after writing because some kernels clear setuid/setgid bits when a
	// file's contents change.
	if !respectUmask {
		if err := tmp.Chmod(mode); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}

	latestMode, err := revalidate()
	if err != nil {
		_ = tmp.Close()
		return err
	}
	latestMode = latestMode.Perm() | latestMode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
	if latestMode != 0 && latestMode != mode {
		if err := tmp.Chmod(latestMode); err != nil {
			_ = tmp.Close()
			return err
		}
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	keepTemp = false

	// Directory fsync is best-effort: some filesystems do not support syncing a
	// directory, and the rename has already committed by this point.
	if dirHandle, openErr := os.Open(dir); openErr == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}

func mutationResult(action, path, oldContent, newContent, newHash string) string {
	return fileDiffSummary(action, path, oldContent, newContent) + "\nsha256: " + newHash
}
