package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const defaultShellTimeout = 30 * time.Second

// ShellExec runs shell commands within a workspace. Synchronous commands keep
// the historical shell_exec result format, while commands with async:true are
// handed to the bounded background-job supervisor.
type ShellExec struct {
	workspace       string
	blockedCommands []string
	jobs            *JobSupervisor
	defaultTimeout  time.Duration
	sshCfg          SSHRuntimeConfig

	syncMu   sync.Mutex
	syncJobs map[string]*shellJob
	syncSeq  atomic.Uint64
	closed   bool
}

// NewShellExec creates a cancellable shell executor using conservative,
// process-local background job limits.
func NewShellExec(workspace string, blockedCommands []string) *ShellExec {
	return NewShellExecWithJobConfig(workspace, blockedCommands, JobSupervisorConfig{})
}

// NewShellExecWithJobConfig is equivalent to NewShellExec with explicit job
// supervisor limits. Zero-valued fields use the production defaults.
func NewShellExecWithJobConfig(workspace string, blockedCommands []string, cfg JobSupervisorConfig) *ShellExec {
	return &ShellExec{
		workspace:       workspace,
		blockedCommands: append([]string(nil), blockedCommands...),
		jobs:            NewJobSupervisor(cfg),
		defaultTimeout:  defaultShellTimeout,
		sshCfg:          DefaultSSHRuntimeConfig(),
		syncJobs:        make(map[string]*shellJob),
	}
}

// Execute runs a shell command. Set async:true to start an explicitly managed
// background job. A synchronous command can be cancelled via Cancel().
func (s *ShellExec) Execute(ctx context.Context, args map[string]interface{}) (string, error) {
	s.syncMu.Lock()
	closed := s.closed
	s.syncMu.Unlock()
	if closed {
		return "", fmt.Errorf("shell executor is closed")
	}
	command, _ := args["command"].(string)
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("command is required")
	}
	if err := s.checkBlocked(command); err != nil {
		return "", err
	}

	// Remote execution: when `host` is set the harness owns the ssh transport
	// and POSIX quoting. The command is passed to ssh as a single argv element
	// (no local shell layer) and runs under bash on the remote host. The
	// blocklist above applies to the remote command exactly as it does locally.
	host, _ := args["host"].(string)
	var execArgv []string
	if strings.TrimSpace(host) != "" {
		sshCfg := s.sshConfig()
		if err := validateRemoteHost(host, sshCfg.AllowedHosts); err != nil {
			return "", err
		}
		execArgv = buildRemoteArgv(host, command, sshCfg)
	}

	s.syncMu.Lock()
	defaultTimeout := s.defaultTimeout
	s.syncMu.Unlock()
	timeout, err := shellTimeout(args, defaultTimeout)
	if err != nil {
		return "", err
	}
	workspace, err := s.resolveWorkspace(ctx, args)
	if err != nil {
		return "", err
	}

	async := false
	if raw, ok := args["async"]; ok {
		var valid bool
		async, valid = raw.(bool)
		if !valid {
			return "", fmt.Errorf("async must be a boolean")
		}
	}
	if async {
		if SubagentTaskIDFromContext(ctx) != "" {
			return "", fmt.Errorf("background shell jobs are unavailable inside subagents; run the command synchronously so it remains supervised")
		}
		if execArgv != nil {
			return "", fmt.Errorf("background shell jobs cannot target remote hosts; run the command synchronously so it remains supervised")
		}
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("start background command: %w", err)
		}
		job, err := s.jobs.start(jobOwnerFromContext(ctx, workspace), command, workspace, timeout)
		if err != nil {
			return "", err
		}
		return marshalJobSnapshot(job.snapshot(false, 0))
	}

	return s.executeSync(ctx, command, workspace, timeout, execArgv)
}

// SetSSHConfig applies remote-execution settings (connect timeout, batch
// mode, host allowlist) before the first remote command runs.
func (s *ShellExec) SetSSHConfig(cfg SSHRuntimeConfig) error {
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = DefaultSSHRuntimeConfig().ConnectTimeout
	}
	cleaned := make([]string, 0, len(cfg.AllowedHosts))
	for _, host := range cfg.AllowedHosts {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		if err := validateRemoteHost(host, nil); err != nil {
			return fmt.Errorf("invalid allowed_hosts entry %q: %w", host, err)
		}
		cleaned = append(cleaned, host)
	}
	s.syncMu.Lock()
	closed := s.closed
	s.syncMu.Unlock()
	if closed {
		return fmt.Errorf("shell executor is closed")
	}
	s.syncMu.Lock()
	s.sshCfg = SSHRuntimeConfig{
		ConnectTimeout: cfg.ConnectTimeout,
		BatchMode:      cfg.BatchMode,
		AllowedHosts:   cleaned,
	}
	s.syncMu.Unlock()
	return nil
}

func (s *ShellExec) sshConfig() SSHRuntimeConfig {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	return s.sshCfg
}

// SetDefaultTimeout applies the configured shell timeout to both synchronous
// and explicitly asynchronous commands that omit timeout_seconds.
func (s *ShellExec) SetDefaultTimeout(timeout time.Duration) error {
	if timeout <= 0 {
		return fmt.Errorf("shell default timeout must be positive")
	}
	s.syncMu.Lock()
	if s.closed {
		s.syncMu.Unlock()
		return fmt.Errorf("shell executor is closed")
	}
	s.defaultTimeout = timeout
	s.syncMu.Unlock()
	return nil
}

// SetJobSupervisorConfig applies job bounds before the first job starts.
func (s *ShellExec) SetJobSupervisorConfig(cfg JobSupervisorConfig) error {
	s.syncMu.Lock()
	closed := s.closed
	s.syncMu.Unlock()
	if closed {
		return fmt.Errorf("shell executor is closed")
	}
	return s.jobs.SetConfig(cfg)
}

// SetJobSupervisor replaces this shell's detached-job supervisor with a shared
// one. Subagent registries call this with the parent's supervisor so a thread
// teardown (DeleteThread) cancels their background work through the same
// supervisor the parent uses. It must be called before the first job starts.
func (s *ShellExec) SetJobSupervisor(jobs *JobSupervisor) error {
	if jobs == nil {
		return fmt.Errorf("job supervisor is nil")
	}
	s.syncMu.Lock()
	closed := s.closed
	s.syncMu.Unlock()
	if closed {
		return fmt.Errorf("shell executor is closed")
	}
	s.jobs = jobs
	return nil
}

func (s *ShellExec) executeSync(ctx context.Context, command, workspace string, timeout time.Duration, execArgv []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("command cancelled: %w", err)
	}

	id := fmt.Sprintf("sync_%d", s.syncSeq.Add(1))
	job := newShellJob(id, jobOwnerFromContext(ctx, workspace), command, workspace, timeout, s.jobs.config(), execArgv)
	if err := job.start(); err != nil {
		return "", fmt.Errorf("start command: %w", err)
	}

	s.syncMu.Lock()
	if s.closed {
		s.syncMu.Unlock()
		_ = job.requestStop(stopReasonCancelled)
		<-job.done
		return "", fmt.Errorf("shell executor is closed")
	}
	s.syncJobs[id] = job
	s.syncMu.Unlock()
	defer func() {
		s.syncMu.Lock()
		delete(s.syncJobs, id)
		s.syncMu.Unlock()
	}()

	go monitorShellJob(ctx, job, timeout, false)
	<-job.done

	job.mu.Lock()
	reason := job.stopReason
	waitErr := job.waitErr
	exitCode := job.exitCode
	stdout := job.stdout.legacyString()
	stderr := job.stderr.legacyString()
	job.mu.Unlock()

	switch reason {
	case stopReasonTimeout:
		return "", fmt.Errorf("command timed out after %v", timeout)
	case stopReasonCancelled:
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("command cancelled: %w", err)
		}
		return "", fmt.Errorf("cancelled")
	}

	if waitErr != nil {
		var exitErr interface{ ExitCode() int }
		if !errors.As(waitErr, &exitErr) {
			return "", fmt.Errorf("execute command: %w", waitErr)
		}
	}
	if exitCode == nil {
		return "", fmt.Errorf("execute command: process ended without an exit status")
	}

	result := fmt.Sprintf("Exit code: %d\nStdout:\n%s", *exitCode, stdout)
	if stderr != "" {
		result += fmt.Sprintf("\nStderr:\n%s", stderr)
	}
	return result, nil
}

// neverBlocked are command words that may never be blocked, regardless of the
// configured blocklist. sudo is required for the agent to perform privileged
// deploys on hosts that grant it passwordless sudo, and it must stay runnable
// even if "sudo" re-enters a config via drift, template regen, or agent edits.
var neverBlocked = map[string]struct{}{
	"sudo": {},
}

func (s *ShellExec) checkBlocked(command string) error {
	blocked := s.blockedCommands
	if len(blocked) == 0 {
		// Defaults match exact command forms only — they never match an
		// arbitrary substring of the command line. sudo is intentionally
		// absent: deployment hosts grant the agent passwordless sudo.
		blocked = []string{"rm -rf /", "chmod 777"}
	}
	patterns := make([][]string, 0, len(blocked))
	for _, entry := range blocked {
		segs := shellSegments(entry)
		if len(segs) == 0 || len(segs[0]) == 0 {
			continue
		}
		if _, skip := neverBlocked[segs[0][0]]; skip {
			continue // policy: privileged deploy commands are never blockable
		}
		patterns = append(patterns, segs[0])
	}
	for _, segment := range shellSegments(command) {
		for _, pattern := range patterns {
			if tokenPrefix(segment, pattern) {
				return fmt.Errorf("command blocked by safety policy: matches %q", strings.Join(pattern, " "))
			}
		}
	}
	return nil
}

// tokenPrefix reports whether the first len(pattern) tokens of segment equal
// pattern. A single-token pattern therefore matches a command word wherever a
// pipeline/chain segment begins; a multi-token pattern matches a command
// segment whose leading arguments equal the pattern exactly (so "rm -rf /"
// blocks only a recursive delete of root, not "rm -rf /tmp/x").
func tokenPrefix(segment, pattern []string) bool {
	if len(segment) < len(pattern) {
		return false
	}
	for i, want := range pattern {
		if segment[i] != want {
			return false
		}
	}
	return true
}

// shellSegments splits a shell command into pipeline/chain segments and
// tokenizes each segment into words, honoring single quotes, double quotes,
// and backslash escapes. The operators |, ||, &&, ;, &, and newlines delimit
// segments. It is a pragmatic matcher, not a POSIX parser: its sole purpose is
// to surface each command word so the blocklist compares against the command
// being run — never an arbitrary substring of the command line.
func shellSegments(command string) [][]string {
	var segments [][]string
	var tokens []string
	var cur strings.Builder
	flushToken := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	flushSegment := func() {
		flushToken()
		if len(tokens) > 0 {
			segments = append(segments, tokens)
			tokens = nil
		}
	}

	inSingle, inDouble := false, false
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case inSingle:
			if r == '\'' {
				inSingle = false
			} else {
				cur.WriteRune(r)
			}
		case inDouble:
			if r == '"' {
				inDouble = false
			} else {
				cur.WriteRune(r)
			}
		case r == '\'':
			inSingle = true
		case r == '"':
			inDouble = true
		case r == '\\':
			if i+1 < len(runes) {
				cur.WriteRune(runes[i+1])
				i++
			}
		case r == '|', r == '&', r == ';', r == '\n':
			flushSegment()
			if (r == '|' || r == '&') && i+1 < len(runes) && runes[i+1] == r {
				i++ // consume the second rune of || / &&
			}
		case r == ' ' || r == '\t' || r == '\r':
			flushToken()
		default:
			cur.WriteRune(r)
		}
	}
	flushSegment()
	return segments
}

func (s *ShellExec) resolveWorkspace(ctx context.Context, args map[string]interface{}) (string, error) {
	workspace := s.workspace
	contextual := WorkspaceFromContext(ctx)
	if contextual != "" {
		workspace = contextual
	} else if raw, ok := args["workspace"]; ok {
		value, valid := raw.(string)
		if !valid {
			return "", fmt.Errorf("workspace must be a string")
		}
		if value != "" {
			workspace = value
		}
	}
	if workspace == "" {
		workspace = "."
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	return filepath.Clean(abs), nil
}

func shellTimeout(args map[string]interface{}, fallback time.Duration) (time.Duration, error) {
	raw, ok := args["timeout_seconds"]
	if !ok {
		return fallback, nil
	}
	var seconds float64
	switch value := raw.(type) {
	case float64:
		seconds = value
	case float32:
		seconds = float64(value)
	case int:
		seconds = float64(value)
	case int64:
		seconds = float64(value)
	case jsonNumber:
		parsed, err := value.Float64()
		if err != nil {
			return 0, fmt.Errorf("timeout_seconds must be a positive number")
		}
		seconds = parsed
	default:
		return 0, fmt.Errorf("timeout_seconds must be a positive number")
	}
	if seconds <= 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0, fmt.Errorf("timeout_seconds must be greater than zero")
	}
	nanoseconds := seconds * float64(time.Second)
	if nanoseconds > float64(math.MaxInt64) {
		return 0, fmt.Errorf("timeout_seconds is too large")
	}
	return time.Duration(nanoseconds), nil
}

// jsonNumber is the small portion of encoding/json.Number needed here. Using
// an interface keeps shell.go independent of a decoder choice at call sites.
type jsonNumber interface {
	Float64() (float64, error)
}

// Cancel gracefully interrupts every active synchronous shell command. It does
// not stop detached jobs; use the job tool's cancel action for those.
func (s *ShellExec) Cancel() error {
	s.syncMu.Lock()
	active := make([]*shellJob, 0, len(s.syncJobs))
	for _, job := range s.syncJobs {
		active = append(active, job)
	}
	s.syncMu.Unlock()

	var errs []error
	for _, job := range active {
		if err := job.requestStop(stopReasonCancelled); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CancelOwner interrupts only synchronous commands belonging to the request's
// thread/workspace owner. It is safe to use from a shared multi-client server.
func (s *ShellExec) CancelOwner(ctx context.Context) error {
	workspace, err := s.resolveWorkspace(ctx, nil)
	if err != nil {
		return err
	}
	owner := jobOwnerFromContext(ctx, workspace)
	s.syncMu.Lock()
	active := make([]*shellJob, 0)
	for _, job := range s.syncJobs {
		if job.owner == owner {
			active = append(active, job)
		}
	}
	s.syncMu.Unlock()
	var errs []error
	for _, job := range active {
		if stopErr := job.requestStop(stopReasonCancelled); stopErr != nil {
			errs = append(errs, stopErr)
		}
	}
	return errors.Join(errs...)
}

// CancelThread stops all synchronous and detached jobs owned by threadID,
// across any workspace previously used by that thread.
func (s *ShellExec) CancelThread(ctx context.Context, threadID string) error {
	s.syncMu.Lock()
	active := make([]*shellJob, 0)
	for _, job := range s.syncJobs {
		if job.owner.threadID == threadID {
			active = append(active, job)
		}
	}
	s.syncMu.Unlock()
	return errors.Join(stopAndWaitJobs(ctx, active), s.jobs.CancelThread(ctx, threadID))
}

// Close prevents new commands and tears down all synchronous and detached
// process groups before returning or the supplied context expires.
func (s *ShellExec) Close(ctx context.Context) error {
	s.syncMu.Lock()
	s.closed = true
	active := make([]*shellJob, 0, len(s.syncJobs))
	for _, job := range s.syncJobs {
		active = append(active, job)
	}
	s.syncMu.Unlock()
	return errors.Join(stopAndWaitJobs(ctx, active), s.jobs.Close(ctx))
}
