package tools

import (
	"context"
	"fmt"
	"strings"
)

// FilePatch atomically applies an unambiguous replacement when expected_sha256
// still matches the current file.
func (f *FileTools) FilePatch(ctx context.Context, args map[string]interface{}) (string, error) {
	path, ok := args["path"].(string)
	if !ok || path == "" {
		return "", fmt.Errorf("path is required")
	}
	search, ok := args["old_str"].(string)
	if !ok {
		if alias, present := args["search_string"]; present {
			search, ok = alias.(string)
		}
	}
	if !ok || search == "" {
		return "", fmt.Errorf("old_str must be a non-empty string")
	}
	replace, ok := args["new_str"].(string)
	if !ok {
		if alias, present := args["replace_string"]; present {
			replace, ok = alias.(string)
		}
	}
	if !ok {
		return "", fmt.Errorf("new_str must be a string")
	}
	expected, err := expectedSHA256(args)
	if err != nil {
		return "", err
	}
	if expected == ExpectedFileAbsent {
		return "", fmt.Errorf("file_patch cannot use expected_sha256 %q; read the existing file and provide its SHA-256", ExpectedFileAbsent)
	}

	resolved, err := f.validatePath(ctx, path)
	if err != nil {
		return "", err
	}

	var result string
	before, newHash, err := mutateFile(ctx, resolved, path, expected, false, func(snapshot fileSnapshot) ([]byte, error) {
		content := string(snapshot.content)
		matches := strings.Count(content, search)
		if matches == 0 {
			// A hashline-formatted old_str (each line "8-hex-hash content", as
			// file_read emits) selects its lines by content anchor instead of
			// literal matching. Anchors are validated first; stale anchors are
			// rejected below with the offending line numbers and current hashes
			// — the whole-file SHA-256 precondition is the coarse guard, these
			// are the fine one.
			if refs := parseHashline(search); refs != nil {
				if err := validateHashlines(snapshot.content, refs); err != nil {
					return nil, err
				}
				patched, err := applyHashlineEdit(snapshot.content, refs, replace)
				if err != nil {
					return nil, err
				}
				result = string(patched)
				return patched, nil
			}
			closest, err := findClosestLine(ctx, content, search)
			if err != nil {
				return nil, err
			}
			if closest == 0 {
				return nil, fmt.Errorf("search_string not found in %s (closest-match scan skipped: file exceeds %d bytes)", path, fuzzyMatchMaxBytes)
			}
			return nil, fmt.Errorf("search_string not found in %s (closest match around line %d)", path, closest)
		}
		if matches > 1 {
			return nil, fmt.Errorf("search_string matches %d locations in %s — require more context for an unambiguous match", matches, path)
		}

		result = strings.Replace(content, search, replace, 1)
		return []byte(result), nil
	})
	if err != nil {
		return "", err
	}
	return mutationResult("patched", path, string(before.content), result, newHash), nil
}

// fuzzyMatchMaxBytes caps the closest-match scan: the Levenshtein pass runs at
// every offset of every line, so on a multi-MB file a stale old_str would burn
// the tool batch for tens of seconds. Above this size the scan is skipped and
// the not-found error is returned without a closest-match hint.
const fuzzyMatchMaxBytes = 1 << 20 // 1 MiB

// fuzzyMatchCheckInterval is how often (in lines) the scan polls ctx so an
// interrupted turn aborts the fuzzy pass.
const fuzzyMatchCheckInterval = 1024

// findClosestLine returns the 1-based line number of the closest substring
// match. It returns 0 when the file exceeds fuzzyMatchMaxBytes (scan skipped),
// and an error when ctx is cancelled mid-scan.
func findClosestLine(ctx context.Context, content, search string) (int, error) {
	if len(search) == 0 {
		return 1, nil
	}
	if len(content) > fuzzyMatchMaxBytes {
		return 0, nil
	}
	bestLine := 1
	bestDist := len(content) + len(search)
	lines := strings.Split(content, "\n")
	start := 0
	for i, line := range lines {
		if i%fuzzyMatchCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		for j := 0; j <= len(line)-len(search); j++ {
			end := j + len(search)
			if end > len(line) {
				end = len(line)
			}
			d := levenshtein(line[j:end], search)
			if d < bestDist {
				bestDist = d
				bestLine = i + 1
			}
		}
		start += len(line) + 1
	}
	return bestLine, nil
}

func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			curr[j] = min(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}
