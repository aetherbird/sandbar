package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Read-scheme expansion: file_read resolves URL-like paths before the
// workspace-jail resolution (these are not files):
//
//	pr://<n>                   GitHub pull request via the gh CLI
//	pr://<owner>/<repo>/<n>    same, explicit repository
//	issue://<n>                GitHub issue via the gh CLI
//	issue://<owner>/<repo>/<n> same, explicit repository
//	agent://<task-id>          a persisted subagent task transcript from the store
//
// pr:// and issue:// shell out to the user's gh binary (their configured
// auth — sandbar stores no GitHub credentials). agent:// renders the
// transcript a delegate_task run persisted in the subagent_tasks table.
//
// ghSchemeTimeout bounds one gh invocation; scheme reads never run
// unbounded.
const ghSchemeTimeout = 60 * time.Second

// readSchemeTimeout is the shared deadline for scheme resolution.
const readSchemeTimeout = 90 * time.Second

// SubagentStore is the durable subset of memory.Store used to resolve
// agent:// subagent transcripts. The registry wires the agent's store here;
// without it agent:// reads report the missing wiring instead of failing as
// an unknown file.
type SubagentStore interface {
	SubagentTranscript(taskID string) (string, error)
}

// readScheme resolves a URL-like path (pr://, issue://, agent://) before the
// normal file read. ok is false when the path is not a scheme URI and the
// caller should fall through to the jailed file read.
func readScheme(ctx context.Context, store SubagentStore, path string) (string, bool, error) {
	switch {
	case strings.HasPrefix(path, "pr://"):
		return readGH(ctx, path, "pr")
	case strings.HasPrefix(path, "issue://"):
		return readGH(ctx, path, "issue")
	case strings.HasPrefix(path, "agent://"):
		return readAgent(store, path)
	}
	return "", false, nil
}

// ghArgs returns the gh argv for a pr:// or issue:// URI and the display
// label. repo is "" when the URI omitted it (gh then uses the current
// git repository).
func ghArgs(uri, kind string) (argv []string, label string, err error) {
	rest, ok := strings.CutPrefix(uri, kind+"://")
	if !ok {
		return nil, "", fmt.Errorf("not a %s:// URI", kind)
	}
	if rest == "" {
		return nil, "", fmt.Errorf("missing %s number after %s://", kind, kind)
	}
	parts := strings.Split(rest, "/")
	num := ""
	switch len(parts) {
	case 1:
		num = parts[0]
	case 3: // owner/repo/n
		argv = []string{kind, "view", parts[2], "--repo", parts[0] + "/" + parts[1]}
		label = parts[0] + "/" + parts[1] + "/" + parts[2]
		num = parts[2]
	default:
		return nil, "", fmt.Errorf("malformed %s:// URI %q (want %s://<n> or %s://<owner>/<repo>/<n>)", kind, uri, kind, kind)
	}
	if argv == nil {
		argv = []string{kind, "view", num}
		label = num
	}
	if _, err := strconv.Atoi(num); err != nil {
		return nil, "", fmt.Errorf("%s number must be numeric, got %q", kind, num)
	}
	return argv, label, nil
}

// runGH runs one gh invocation, returning combined output. A missing gh
// binary yields a clear error.
func runGH(ctx context.Context, argv []string) (string, error) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return "", fmt.Errorf("gh CLI not found on PATH (install GitHub CLI to read GitHub content)")
	}
	cmdCtx, cancel := context.WithTimeout(ctx, ghSchemeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, gh, argv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("gh %s: %s", strings.Join(argv, " "), msg)
	}
	return string(out), nil
}

// readGH resolves a pr:// or issue:// URI through the gh CLI.
func readGH(ctx context.Context, path, kind string) (string, bool, error) {
	argv, label, err := ghArgs(path, kind)
	if err != nil {
		return "", true, fmt.Errorf("read %s: %w", path, err)
	}
	ctx, cancel := context.WithTimeout(ctx, readSchemeTimeout)
	defer cancel()
	out, err := runGH(ctx, argv)
	if err != nil {
		return "", true, fmt.Errorf("read %s: %w", path, err)
	}
	return fmt.Sprintf("%s (%s)\n\n%s\n", kind, label, strings.TrimRight(out, "\n")), true, nil
}

// readAgent resolves an agent://<task-id> URI to a persisted subagent task
// transcript. The store is optional (minimal hosts leave it nil), in which
// case the scheme explains the missing wiring.
func readAgent(store SubagentStore, path string) (string, bool, error) {
	id := strings.TrimPrefix(path, "agent://")
	if id == "" {
		return "", true, fmt.Errorf("read %s: missing task id after agent://", path)
	}
	if store == nil {
		return "", true, fmt.Errorf("read %s: agent:// reads require the session store (wired by the host)", path)
	}
	transcript, err := store.SubagentTranscript(id)
	if err != nil {
		return "", true, fmt.Errorf("read %s: %w", path, err)
	}
	return transcript, true, nil
}
