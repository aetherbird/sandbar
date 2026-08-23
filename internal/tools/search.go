package tools

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// SearchFiles searches file contents or finds files by name using ripgrep.
func SearchFiles(ctx context.Context, args map[string]interface{}) (string, error) {
	return searchFiles(ctx, args, "", "")
}

// searchFilesInWorkspace returns a search handler whose relative paths default
// to workspace. The request-scoped workspace injected by the agent takes
// precedence when present.
func searchFilesInWorkspace(workspace string) func(context.Context, map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		return searchFiles(ctx, args, workspace, "")
	}
}

func searchFiles(ctx context.Context, args map[string]interface{}, defaultWorkspace, rgExecutable string) (string, error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}

	workspace, _ := args["workspace"].(string)
	if workspace == "" {
		workspace = defaultWorkspace
	}
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	if workspace != "" && !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}

	// ripgrep is a soft dependency: when it is not on PATH the pure-Go
	// walker below produces the same result format. An explicitly injected
	// executable that cannot be found is treated the same way (this is also
	// how tests force the fallback path).
	if rgExecutable != "" {
		if _, err := exec.LookPath(rgExecutable); err != nil {
			rgExecutable = ""
		}
	} else if found, err := exec.LookPath("rg"); err == nil {
		rgExecutable = found
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("search path %q: %w", path, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("search path %q is not a directory", path)
	}

	target, _ := args["target"].(string)
	if target == "" {
		target = "content"
	}
	switch target {
	case "files", "content":
	default:
		return "", fmt.Errorf("target must be 'content' or 'files', got: %s", target)
	}

	fileGlob, _ := args["file_glob"].(string)
	limit := 50
	if v, ok := args["limit"]; ok {
		switch n := v.(type) {
		case float64:
			limit = int(n)
		case int:
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}

	searchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var lines []string
	var truncated bool
	if rgExecutable == "" {
		lines, truncated, err = goSearch(searchCtx, path, pattern, target, fileGlob, limit)
	} else {
		lines, truncated, err = rgSearch(searchCtx, cancel, path, pattern, target, fileGlob, limit, rgExecutable)
	}
	if err != nil {
		return "", err
	}

	if len(lines) == 0 {
		return "(no matches)", nil
	}

	var sb strings.Builder
	if truncated {
		sb.WriteString(fmt.Sprintf("Search results (%d shown; more available):\n\n", len(lines)))
	} else {
		sb.WriteString(fmt.Sprintf("Search results (%d of %d shown):\n\n", len(lines), len(lines)))
	}
	for _, line := range lines {
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

// rgSearch runs ripgrep and streams its output, stopping once one extra match
// proves that the requested result limit was reached. This bounds memory and
// avoids scanning the rest of a large tree solely to discard its output.
func rgSearch(ctx context.Context, cancel context.CancelFunc, path, pattern, target, fileGlob string, limit int, rgExecutable string) ([]string, bool, error) {
	var cmd *exec.Cmd
	include := func(string) bool { return true }
	switch target {
	case "files":
		argsList := []string{"--files"}
		if fileGlob != "" {
			argsList = append(argsList, "-g", fileGlob)
		}
		cmd = exec.CommandContext(ctx, rgExecutable, argsList...)
		needle := strings.ToLower(pattern)
		include = func(line string) bool {
			return strings.Contains(strings.ToLower(filepath.Base(line)), needle)
		}
	case "content":
		argsList := []string{"-n", "--no-heading", "--color=never"}
		if fileGlob != "" {
			argsList = append(argsList, "-g", fileGlob)
		}
		argsList = append(argsList, "--", pattern)
		cmd = exec.CommandContext(ctx, rgExecutable, argsList...)
	}
	cmd.Dir = path

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, fmt.Errorf("search stdout: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, false, fmt.Errorf("start search: %w", err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	lines := make([]string, 0, limit)
	truncated := false
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if !include(line) {
			continue
		}
		if len(lines) == limit {
			truncated = true
			cancel()
			break
		}
		lines = append(lines, line)
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		// Stop ripgrep before waiting: an overlong line can leave it blocked on
		// the stdout pipe after Scanner stops consuming output.
		cancel()
	}
	waitErr := cmd.Wait()
	if scanErr != nil {
		return nil, false, fmt.Errorf("read search output: %w", scanErr)
	}
	if waitErr != nil && !truncated {
		if exitErr, ok := waitErr.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			// rg exits 1 when no matches — not an error.
			return nil, false, nil
		}
		if ctx.Err() != nil {
			return nil, false, fmt.Errorf("search failed: %w", ctx.Err())
		}
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, false, fmt.Errorf("search failed: %w: %s", waitErr, detail)
		}
		return nil, false, fmt.Errorf("search failed: %w", waitErr)
	}
	return lines, truncated, nil
}

// ─── Pure-Go fallback walker ────────────────────────────────────────────────
//
// goSearch reproduces the ripgrep invocation's observable behavior when rg is
// not installed: same match semantics (regex content search, case-insensitive
// literal filename search), same skipping (hidden entries — rg runs without
// --hidden — plus .git and the usual build/vendor junk), same result format
// ("relative/path:line:text" for content, bare relative paths for files), and
// the same one-past-the-limit truncation marker.

const (
	goSearchMaxFileBytes = 1 << 20 // skip files larger than 1MB
	goSearchSniffBytes   = 8 << 10 // binary sniff window
)

var goSearchSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "target": true,
}

func goSearch(ctx context.Context, root, pattern, target, fileGlob string, limit int) ([]string, bool, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, false, fmt.Errorf("search failed: %w", err)
	}

	lines := make([]string, 0, limit)
	truncated := false
	needle := strings.ToLower(pattern)
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if truncated || ctx.Err() != nil {
			return fs.SkipAll
		}
		if err != nil {
			// An unreadable subtree is skipped, not fatal — rg also continues
			// past directories it cannot enter.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || goSearchSkipDirs[name]) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		switch target {
		case "files":
			if fileGlob != "" && !globMatch(fileGlob, rel, name) {
				return nil
			}
			if !strings.Contains(strings.ToLower(name), needle) {
				return nil
			}
		case "content":
			if fileGlob != "" && !globMatch(fileGlob, rel, name) {
				return nil
			}
			more, matchErr := goSearchFile(ctx, p, rel, re, limit, &lines)
			if matchErr != nil {
				return nil // unreadable file: skip, like rg
			}
			if more {
				truncated = true // limit reached and one more match found
				return fs.SkipAll
			}
			return nil
		}

		if len(lines) == limit {
			truncated = true
			return fs.SkipAll
		}
		lines = append(lines, rel)
		return nil
	})
	if walkErr != nil {
		return nil, false, fmt.Errorf("search failed: %w", walkErr)
	}
	if ctx.Err() != nil && !truncated && len(lines) < limit {
		return nil, false, fmt.Errorf("search failed: %w", ctx.Err())
	}
	return lines, truncated, nil
}

// goSearchFile scans one file for regex matches, appending "rel:line:text"
// lines in file order. Returns more=true when the limit was already reached
// and at least one further match exists — the one-past-the-limit proof
// ripgrep's streaming consumer provides.
func goSearchFile(ctx context.Context, path, rel string, re *regexp.Regexp, limit int, lines *[]string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > goSearchMaxFileBytes {
		return false, err
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	// Binary sniff on the first 8KB: rg suppresses files containing NUL
	// bytes, and a file reading zero bytes is simply empty, not binary.
	sniff := make([]byte, goSearchSniffBytes)
	n, _ := io.ReadFull(f, sniff)
	if n == 0 {
		return false, nil
	}
	if bytes.IndexByte(sniff[:n], 0) >= 0 {
		return false, nil
	}

	// Lines already consumed by the sniff live in the buffer; read the rest.
	rest, err := io.ReadAll(io.LimitReader(f, goSearchMaxFileBytes))
	if err != nil {
		return false, err
	}
	full := append(sniff[:n], rest...)

	scanner := bufio.NewScanner(bytes.NewReader(full))
	scanner.Buffer(make([]byte, 64*1024), goSearchMaxFileBytes+1)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if ctx.Err() != nil {
			return false, nil
		}
		if !re.MatchString(scanner.Text()) {
			continue
		}
		if len(*lines) == limit {
			return true, nil
		}
		*lines = append(*lines, fmt.Sprintf("%s:%d:%s", rel, lineNo, scanner.Text()))
	}
	return false, scanner.Err()
}

// globMatch mirrors rg's -g semantics closely enough for tool arguments: a
// glob without a slash matches the file's base name; one with a slash matches
// the whole relative path.
func globMatch(glob, rel, name string) bool {
	subject := name
	if strings.Contains(glob, "/") {
		subject = rel
	}
	matched, err := filepath.Match(glob, subject)
	return err == nil && matched
}
