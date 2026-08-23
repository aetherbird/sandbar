package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaveGeneratedImageJailedToWorkspace asserts generated images land under
// the configured workspace root even when the process CWD is elsewhere.
// Regression: the upload dir was hard-coded to ./workspace/uploads, resolved
// against the CWD and escaping the workspace jail.
func TestSaveGeneratedImageJailedToWorkspace(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	workspace := t.TempDir()

	path, err := saveGeneratedImage(resolveUploadsDir(WithWorkspace(context.Background(), workspace)), []byte("png-bytes"))
	if err != nil {
		t.Fatalf("save generated image: %v", err)
	}

	if !strings.HasPrefix(path, workspace+string(filepath.Separator)) {
		t.Fatalf("saved image %q escapes workspace root %q", path, workspace)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("saved image not readable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "workspace", "uploads")); err == nil {
		t.Fatal("image was written under the process CWD despite a configured workspace")
	}
}

// TestResolveUploadsDirPrecedence pins the workspace resolution order:
// request-scoped (context) workspace first, then the args fallback.
func TestResolveUploadsDirPrecedence(t *testing.T) {
	ctxWorkspace := t.TempDir()
	fallbackWorkspace := t.TempDir()

	ctx := WithWorkspace(context.Background(), ctxWorkspace)
	if got := resolveUploadsDir(ctx); got != filepath.Join(ctxWorkspace, "uploads") {
		t.Fatalf("contextual workspace ignored: %q", got)
	}
	if got := resolveUploadsDir(WithWorkspace(context.Background(), fallbackWorkspace)); got != filepath.Join(fallbackWorkspace, "uploads") {
		t.Fatalf("closure-seeded workspace ignored: %q", got)
	}
	if got := resolveUploadsDir(context.Background()); got != filepath.Join(".", "uploads") {
		t.Fatalf("empty-workspace fallback = %q", got)
	}
}

// TestImageGenerateMissingAPIKeyIsActionable pins the provider-honesty error:
// with no OpenRouter key the tool errors instead of silently reporting
// success with no image.
func TestImageGenerateMissingAPIKeyIsActionable(t *testing.T) {
	_, err := ImageGenerate(context.Background(), map[string]interface{}{"prompt": "a cat"})
	if err == nil || !strings.Contains(err.Error(), "requires an OpenRouter provider in config") {
		t.Fatalf("missing key error = %v", err)
	}
}
