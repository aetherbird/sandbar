package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLineHashDeterministicAndShort(t *testing.T) {
	h1 := lineHash("hello world")
	h2 := lineHash("hello world")
	if h1 != h2 {
		t.Fatalf("same content produced different hashes: %q vs %q", h1, h2)
	}
	if len(h1) != hashPrefixLen {
		t.Fatalf("hash length = %d, want %d", len(h1), hashPrefixLen)
	}
	if lineHash("hello world") == lineHash("hello worlD") {
		t.Fatal("different content collided")
	}
}

func TestParseHashline(t *testing.T) {
	// Well-formed hashline text parses.
	text := lineHash("line one") + " line one\n" + lineHash("line two") + " line two\n"
	refs := parseHashline(text)
	if refs == nil {
		t.Fatalf("parseHashline(%q) = nil", text)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2", len(refs))
	}
	if refs[0].Hash != lineHash("line one") || refs[0].Content != "line one" {
		t.Fatalf("ref[0] = %+v", refs[0])
	}

	// Plain text (no hashes) returns nil.
	if refs := parseHashline("just plain text\n"); refs != nil {
		t.Fatalf("plain text parsed as hashline: %+v", refs)
	}
	// Non-hex prefix returns nil.
	if refs := parseHashline("zzzzzzzz line\n"); refs != nil {
		t.Fatalf("non-hex prefix parsed: %+v", refs)
	}
	// Empty text returns nil.
	if refs := parseHashline(""); refs != nil {
		t.Fatalf("empty text parsed: %+v", refs)
	}
}

func TestValidateHashlines(t *testing.T) {
	content := []byte("alpha\nbeta\ngamma\n")

	// All anchors fresh → no error.
	refs := []hashlineRef{
		{Hash: lineHash("beta"), Content: "beta"},
	}
	if err := validateHashlines(content, refs); err != nil {
		t.Fatalf("fresh anchor refused: %v", err)
	}

	// Anchor with stale content → error naming the hash and the current one.
	stale := []hashlineRef{
		{Hash: lineHash("BETA"), Content: "BETA"},
	}
	err := validateHashlines(content, stale)
	if err == nil {
		t.Fatal("stale anchor accepted")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Fatalf("error should mention staleness: %v", err)
	}

	// Anchor not present at all → error.
	ghost := []hashlineRef{
		{Hash: lineHash("wombat"), Content: "wombat"},
	}
	if err := validateHashlines(content, ghost); err == nil {
		t.Fatal("ghost anchor accepted")
	}
}

func TestApplyHashlineEdit(t *testing.T) {
	content := []byte("alpha\nbeta\ngamma\n")
	refs := []hashlineRef{
		{Hash: lineHash("beta"), Content: "beta"},
	}
	got, err := applyHashlineEdit(content, refs, "BETA")
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if string(got) != "alpha\nBETA\ngamma\n" {
		t.Fatalf("got %q", got)
	}

	// Multi-line replacement spanning a block.
	blockRefs := []hashlineRef{
		{Hash: lineHash("alpha"), Content: "alpha"},
		{Hash: lineHash("beta"), Content: "beta"},
	}
	got, err = applyHashlineEdit(content, blockRefs, "one\ntwo")
	if err != nil {
		t.Fatalf("block apply failed: %v", err)
	}
	if string(got) != "one\ntwo\ngamma\n" {
		t.Fatalf("block got %q", got)
	}

	// Replacement at the tail keeps trailing newline semantics.
	lastRefs := []hashlineRef{
		{Hash: lineHash("gamma"), Content: "gamma"},
	}
	got, err = applyHashlineEdit(content, lastRefs, "end")
	if err != nil {
		t.Fatalf("tail apply failed: %v", err)
	}
	if string(got) != "alpha\nbeta\nend\n" {
		t.Fatalf("tail got %q", got)
	}
}

// TestHashlineOutputFormat checks the hashline output shape from file_read:
// header plus one "hash content" line per file line, with no phantom line
// for the trailing newline.
func TestHashlineOutputFormat(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "f.txt"), []byte("a\nbb\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := NewFileTools(workspace).FileRead(context.Background(), map[string]interface{}{"path": "f.txt"})
	if err != nil {
		t.Fatalf("file_read: %v", err)
	}
	lines := splitLines(out)
	if len(lines) != 3 { // header + 2 content lines
		t.Fatalf("want 3 lines, got %d: %q", len(lines), out)
	}
	if lines[1] != lineHash("a")+" a" || lines[2] != lineHash("bb")+" bb" {
		t.Fatalf("hashline output wrong: %q", out)
	}
}

// TestHashlineEndToEnd verifies the full read → patch round trip: read emits
// hashline lines, file_patch accepts those exact anchors, and a stale anchor
// is rejected without touching the file.
func TestHashlineEndToEnd(t *testing.T) {
	workspace := t.TempDir()
	original := "first line\nsecond line\nthird line\n"
	if err := os.WriteFile(filepath.Join(workspace, "f.txt"), []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(workspace)

	readOut, err := ft.FileRead(context.Background(), map[string]interface{}{"path": "f.txt"})
	if err != nil {
		t.Fatalf("file_read: %v", err)
	}
	anchor := lineHash("second line") + " second line"
	if !strings.Contains(readOut, anchor) {
		t.Fatalf("read output missing anchor %q: %q", anchor, readOut)
	}

	if _, err := ft.FilePatch(context.Background(), map[string]interface{}{
		"path": "f.txt", "old_str": anchor, "new_str": "SECOND",
		"expected_sha256": sha256Hex([]byte(original)),
	}); err != nil {
		t.Fatalf("hashline patch failed: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(workspace, "f.txt"))
	if string(data) != "first line\nSECOND\nthird line\n" {
		t.Fatalf("file = %q", data)
	}

	// Now the anchor is stale: patching again with the old anchor must refuse
	// and leave the file untouched. The anchor's hash names the stale line.
	_, err = ft.FilePatch(context.Background(), map[string]interface{}{
		"path": "f.txt", "old_str": anchor, "new_str": "AGAIN",
		"expected_sha256": sha256Hex(data),
	})
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale anchor should be refused with a staleness error, got %v", err)
	}
	if !strings.Contains(err.Error(), lineHash("second line")) || !strings.Contains(err.Error(), "re-read") {
		t.Fatalf("stale refusal should name the anchor hash and ask for a re-read: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(workspace, "f.txt")); string(after) != "first line\nSECOND\nthird line\n" {
		t.Fatalf("stale patch mutated the file: %q", after)
	}
}

// TestFilePatchHashlineSpanReplacesFirstToLastAnchor: multiple anchors select
// the whole span from the first to the last anchored line.
func TestFilePatchHashlineSpanReplacesFirstToLastAnchor(t *testing.T) {
	workspace := t.TempDir()
	content := "alpha\nbeta\ngamma\ndelta\n"
	if err := os.WriteFile(filepath.Join(workspace, "f.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	oldStr := lineHash("beta") + " beta\n" + lineHash("delta") + " delta"
	if _, err := NewFileTools(workspace).FilePatch(context.Background(), map[string]interface{}{
		"path": "f.txt", "old_str": oldStr, "new_str": "B\nG\nD",
		"expected_sha256": sha256Hex([]byte(content)),
	}); err != nil {
		t.Fatalf("span patch failed: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(workspace, "f.txt"))
	if string(data) != "alpha\nB\nG\nD\n" {
		t.Fatalf("file = %q", data)
	}
}

// TestFilePatchMixedPlainAndHashlineOldStr: an old_str that mixes hashline and
// plain lines is NOT hashline-formatted, so plain semantics apply — the
// hashline line does not literally match and the patch is refused.
func TestFilePatchMixedPlainAndHashlineOldStr(t *testing.T) {
	workspace := t.TempDir()
	content := "alpha\nbeta\ngamma\n"
	if err := os.WriteFile(filepath.Join(workspace, "f.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	mixed := lineHash("beta") + " beta\nplain gamma"
	if _, err := NewFileTools(workspace).FilePatch(context.Background(), map[string]interface{}{
		"path": "f.txt", "old_str": mixed, "new_str": "x",
		"expected_sha256": sha256Hex([]byte(content)),
	}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("mixed old_str should fall back to plain matching and fail not-found, got %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(workspace, "f.txt")); string(data) != content {
		t.Fatalf("file mutated: %q", data)
	}
}

// TestFilePatchPlainOldStrUnchangedWithHashlinesInRead: a plain (non-hashline)
// old_str still patches normally even though reads are now hash-stamped.
func TestFilePatchPlainOldStrUnchangedWithHashlinesInRead(t *testing.T) {
	workspace := t.TempDir()
	content := "alpha\nbeta\ngamma\n"
	if err := os.WriteFile(filepath.Join(workspace, "f.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(workspace)
	if _, err := ft.FilePatch(context.Background(), map[string]interface{}{
		"path": "f.txt", "old_str": "beta", "new_str": "BETA",
		"expected_sha256": sha256Hex([]byte(content)),
	}); err != nil {
		t.Fatalf("plain patch failed: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(workspace, "f.txt"))
	if string(data) != "alpha\nBETA\ngamma\n" {
		t.Fatalf("file = %q", data)
	}
}
