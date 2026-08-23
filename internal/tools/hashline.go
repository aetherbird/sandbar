package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// hashPrefixLen is the number of hex characters used as a line hash.
// 8 chars (4 bytes) gives 2^32 possible values — enough for
// typical files without collision risk in the edit context.
const hashPrefixLen = 8

// lineHash returns a short hex hash of the line content (without
// trailing newline). The same content always produces the same hash.
func lineHash(line string) string {
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:])[:hashPrefixLen]
}

// hashlineRef is a parsed reference from hashline-formatted text:
// the hash prefix and the expected content after it.
type hashlineRef struct {
	Hash    string
	Content string
}

// parseHashline tries to parse text as hashline-formatted lines.
// Each line must start with an 8-character hex hash followed by a
// space. Returns nil (not an error) when the text is not in
// hashline format — the caller should fall back to plain string
// matching.
func parseHashline(text string) []hashlineRef {
	lines := strings.Split(text, "\n")
	refs := make([]hashlineRef, 0, len(lines))
	for _, l := range lines {
		if l == "" {
			continue
		}
		if len(l) < hashPrefixLen+1 {
			return nil // too short to be hashline
		}
		prefix := l[:hashPrefixLen]
		rest := l[hashPrefixLen:]
		if rest[0] != ' ' {
			return nil // not hashline format
		}
		// Validate the prefix is hex.
		for _, c := range prefix {
			if !isHexChar(c) {
				return nil
			}
		}
		refs = append(refs, hashlineRef{Hash: prefix, Content: rest[1:]})
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
}

func isHexChar(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// validateHashlines checks that every hash reference in refs still
// matches the current file content. Returns an error describing the
// stale anchors (with the current hashes for those lines) so the
// model can re-read and retry. All stale anchors are reported, not
// just the first, because a re-read fixes them all at once.
func validateHashlines(content []byte, refs []hashlineRef) error {
	lines := splitLines(string(content))
	// Build a map of hash → line index for fast lookup.
	hashIndex := map[string]int{}
	for i, l := range lines {
		hashIndex[lineHash(l)] = i
	}
	var stale []string
	for _, ref := range refs {
		idx, ok := hashIndex[ref.Hash]
		if !ok {
			stale = append(stale, fmt.Sprintf("line %q (%s): hash not found in the current file", ref.Content, ref.Hash))
			continue
		}
		if lines[idx] != ref.Content {
			stale = append(stale, fmt.Sprintf("line %d %q (%s): content differs (current hash %q)",
				idx+1, ref.Content, ref.Hash, lineHash(lines[idx])))
		}
	}
	if len(stale) > 0 {
		return fmt.Errorf("stale hashline anchor(s): %s; the file changed since it was read — re-read it for fresh anchors",
			strings.Join(stale, "; "))
	}
	return nil
}

// applyHashlineEdit replaces the lines referenced by refs with
// newContent. The refs must not overlap. newContent is split on
// newlines and replaces the span from the first ref's line to the
// last ref's line (inclusive).
func applyHashlineEdit(content []byte, refs []hashlineRef, newContent string) ([]byte, error) {
	lines := splitLines(string(content))
	// Build hash → index map.
	hashIndex := map[string]int{}
	for i, l := range lines {
		hashIndex[lineHash(l)] = i
	}

	// Collect the line indices of all refs, in order of appearance.
	indices := make([]int, 0, len(refs))
	for _, ref := range refs {
		idx, ok := hashIndex[ref.Hash]
		if !ok {
			return nil, fmt.Errorf("hashline anchor %q not found", ref.Hash)
		}
		indices = append(indices, idx)
	}

	// Find the span: from the first referenced line to the last.
	first := indices[0]
	last := indices[0]
	for _, idx := range indices[1:] {
		if idx < first {
			first = idx
		}
		if idx > last {
			last = idx
		}
	}

	// Replace the span with newContent lines.
	newLines := splitLines(newContent)
	var sb strings.Builder
	for _, l := range lines[:first] {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	for _, nl := range newLines {
		sb.WriteString(nl)
		sb.WriteByte('\n')
	}
	for _, l := range lines[last+1:] {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	return []byte(sb.String()), nil
}

// formatHashlineOutput prefixes each line of content with its content hash,
// producing hashline-formatted output. Trailing-newline semantics follow
// splitLines: "a\nb\n" yields two stamped lines, not a phantom third.
func formatHashlineOutput(content string) string {
	var b strings.Builder
	for _, l := range splitLines(content) {
		b.WriteString(lineHash(l))
		b.WriteByte(' ')
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}
