package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	defaultMaxJobs             = 128
	defaultMaxRunningJobs      = 16
	defaultJobOutputBytes      = 64 * 1024
	defaultJobRetention        = 30 * time.Minute
	defaultJobTerminationGrace = 750 * time.Millisecond
	legacyShellOutputBytes     = 16 * 1024
)

// JobState is the externally visible lifecycle state of a background command.
type JobState string

const (
	JobRunning   JobState = "running"
	JobCompleted JobState = "completed"
	JobFailed    JobState = "failed"
	JobCancelled JobState = "cancelled"
)

// JobSupervisorConfig bounds process concurrency, retained metadata, and
// captured output. Zero-valued fields use safe defaults.
type JobSupervisorConfig struct {
	MaxJobs          int
	MaxRunning       int
	OutputBytes      int
	Retention        time.Duration
	TerminationGrace time.Duration
}

func normalizeJobSupervisorConfig(cfg JobSupervisorConfig) JobSupervisorConfig {
	if cfg.MaxJobs <= 0 {
		cfg.MaxJobs = defaultMaxJobs
	}
	if cfg.MaxRunning <= 0 {
		cfg.MaxRunning = defaultMaxRunningJobs
	}
	if cfg.MaxRunning > cfg.MaxJobs {
		cfg.MaxRunning = cfg.MaxJobs
	}
	if cfg.OutputBytes <= 0 {
		cfg.OutputBytes = defaultJobOutputBytes
	}
	if cfg.Retention <= 0 {
		cfg.Retention = defaultJobRetention
	}
	if cfg.TerminationGrace <= 0 {
		cfg.TerminationGrace = defaultJobTerminationGrace
	}
	return cfg
}

type jobOwner struct {
	threadID  string
	workspace string
}

func jobOwnerFromContext(ctx context.Context, workspace string) jobOwner {
	return jobOwner{threadID: threadIDFromContext(ctx), workspace: workspace}
}

// JobSupervisor owns explicitly detached shell processes for one registry.
// Completed jobs are retained briefly for inspection and are reaped both
// opportunistically and by a TTL timer.
type JobSupervisor struct {
	mu        sync.Mutex
	cfg       JobSupervisorConfig
	jobs      map[string]*shellJob
	timers    map[string]*time.Timer
	closed    bool
	closeDone chan struct{}
	closeErr  error
}

// NewJobSupervisor creates a bounded in-process job supervisor.
func NewJobSupervisor(cfg JobSupervisorConfig) *JobSupervisor {
	return &JobSupervisor{
		cfg:       normalizeJobSupervisorConfig(cfg),
		jobs:      make(map[string]*shellJob),
		timers:    make(map[string]*time.Timer),
		closeDone: make(chan struct{}),
	}
}

// SetConfig changes supervisor limits before the first job starts. Runtime
// wiring calls this during startup; refusing to reconfigure a live supervisor
// keeps retention timers and output bounds internally consistent.
func (s *JobSupervisor) SetConfig(cfg JobSupervisorConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("background job supervisor is closed")
	}
	if len(s.jobs) != 0 {
		return fmt.Errorf("cannot reconfigure background jobs after jobs have started")
	}
	s.cfg = normalizeJobSupervisorConfig(cfg)
	return nil
}

func (s *JobSupervisor) config() JobSupervisorConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

func (s *JobSupervisor) start(owner jobOwner, command, workspace string, timeout time.Duration) (*shellJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("background job supervisor is closed")
	}

	now := time.Now()
	s.reapLocked(now)
	running := 0
	for _, existing := range s.jobs {
		if existing.stateValue() == JobRunning {
			running++
		}
	}
	if running >= s.cfg.MaxRunning {
		return nil, fmt.Errorf("background job capacity reached (%d running)", s.cfg.MaxRunning)
	}
	if len(s.jobs) >= s.cfg.MaxJobs {
		s.evictOldestFinishedLocked()
	}
	if len(s.jobs) >= s.cfg.MaxJobs {
		return nil, fmt.Errorf("background job capacity reached (%d retained)", s.cfg.MaxJobs)
	}

	id := "job_" + uuid.NewString()
	job := newShellJob(id, owner, command, workspace, timeout, s.cfg, nil)
	job.onDone = s.jobDone
	if err := job.start(); err != nil {
		return nil, fmt.Errorf("start background command: %w", err)
	}
	s.jobs[id] = job
	go monitorShellJob(context.Background(), job, timeout, true)
	return job, nil
}

func (s *JobSupervisor) jobDone(job *shellJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.jobs[job.id]
	if !ok || current != job {
		return
	}
	if s.closed {
		s.removeLocked(job.id)
		return
	}
	if old := s.timers[job.id]; old != nil {
		old.Stop()
	}
	id := job.id
	s.timers[id] = time.AfterFunc(s.cfg.Retention, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if current, exists := s.jobs[id]; exists && current == job && current.stateValue() != JobRunning {
			s.removeLocked(id)
		}
	})
}

func (s *JobSupervisor) reapLocked(now time.Time) {
	for id, job := range s.jobs {
		finished := job.finishedTime()
		if !finished.IsZero() && now.Sub(finished) >= s.cfg.Retention {
			s.removeLocked(id)
		}
	}
}

func (s *JobSupervisor) evictOldestFinishedLocked() {
	var oldest *shellJob
	for _, job := range s.jobs {
		if job.stateValue() == JobRunning {
			continue
		}
		if oldest == nil || job.finishedTime().Before(oldest.finishedTime()) {
			oldest = job
		}
	}
	if oldest != nil {
		s.removeLocked(oldest.id)
	}
}

func (s *JobSupervisor) removeLocked(id string) {
	if timer := s.timers[id]; timer != nil {
		timer.Stop()
		delete(s.timers, id)
	}
	delete(s.jobs, id)
}

// CancelThread stops all detached jobs owned by threadID and waits for process
// teardown. Other owners are unaffected.
func (s *JobSupervisor) CancelThread(ctx context.Context, threadID string) error {
	s.mu.Lock()
	jobs := make([]*shellJob, 0)
	for _, job := range s.jobs {
		if job.owner.threadID == threadID && job.stateValue() == JobRunning {
			jobs = append(jobs, job)
		}
	}
	s.mu.Unlock()
	return stopAndWaitJobs(ctx, jobs)
}

// Close rejects new jobs, gracefully terminates every running process group,
// waits for teardown, and releases all retention timers and snapshots.
func (s *JobSupervisor) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		jobs := make([]*shellJob, 0, len(s.jobs))
		for _, job := range s.jobs {
			jobs = append(jobs, job)
		}
		go s.finishClose(jobs)
	}
	done := s.closeDone
	s.mu.Unlock()

	select {
	case <-done:
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// finishClose is deliberately independent of the initiating caller's context:
// once shutdown begins, process teardown must continue even if that caller
// times out or disconnects. Close callers can still bound how long they wait.
func (s *JobSupervisor) finishClose(jobs []*shellJob) {
	err := stopAndWaitJobs(context.Background(), jobs)
	s.mu.Lock()
	for id := range s.jobs {
		s.removeLocked(id)
	}
	s.closeErr = err
	close(s.closeDone)
	s.mu.Unlock()
}

func stopAndWaitJobs(ctx context.Context, jobs []*shellJob) error {
	var errs []error
	for _, job := range jobs {
		if err := job.requestStop(stopReasonCancelled); err != nil {
			errs = append(errs, err)
		}
	}
	for _, job := range jobs {
		select {
		case <-job.done:
		case <-ctx.Done():
			return errors.Join(append(errs, ctx.Err())...)
		}
	}
	return errors.Join(errs...)
}

func (s *JobSupervisor) get(owner jobOwner, id string) (*shellJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked(time.Now())
	job, ok := s.jobs[id]
	if !ok || job.owner != owner {
		// Do not reveal whether a job belonging to another owner exists.
		return nil, fmt.Errorf("job %q not found", id)
	}
	return job, nil
}

func (s *JobSupervisor) list(owner jobOwner) []JobSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapLocked(time.Now())
	result := make([]JobSnapshot, 0)
	for _, job := range s.jobs {
		if job.owner == owner {
			result = append(result, job.snapshot(false, 0))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].StartedAt.After(result[j].StartedAt)
	})
	return result
}

type stopReason uint8

const (
	stopReasonNone stopReason = iota
	stopReasonCancelled
	stopReasonTimeout
)

type shellJob struct {
	mu sync.Mutex

	id         string
	owner      jobOwner
	command    string
	execArgv   []string // non-nil = spawn this argv directly (remote ssh); nil = bash -c command
	workspace  string
	timeout    time.Duration
	grace      time.Duration
	state      JobState
	startedAt  time.Time
	finishedAt time.Time
	exitCode   *int
	errText    string
	waitErr    error
	stopReason stopReason
	cmd        *exec.Cmd
	done       chan struct{}
	stdout     *boundedCapture
	stderr     *boundedCapture
	onDone     func(*shellJob)
}

func newShellJob(id string, owner jobOwner, command, workspace string, timeout time.Duration, cfg JobSupervisorConfig, execArgv []string) *shellJob {
	return &shellJob{
		id:        id,
		owner:     owner,
		command:   command,
		execArgv:  execArgv,
		workspace: workspace,
		timeout:   timeout,
		grace:     cfg.TerminationGrace,
		state:     JobRunning,
		done:      make(chan struct{}),
		stdout:    newBoundedCapture(cfg.OutputBytes),
		stderr:    newBoundedCapture(cfg.OutputBytes),
	}
}

func (j *shellJob) start() error {
	var cmd *exec.Cmd
	if len(j.execArgv) > 0 {
		// Remote execution: spawn the pre-built ssh argv directly. No local
		// shell parses the command — it travels as one argv element to ssh,
		// which hands it to the remote login shell.
		cmd = exec.Command(j.execArgv[0], j.execArgv[1:]...)
	} else {
		cmd = exec.Command("bash", "-c", j.command)
	}
	cmd.Dir = j.workspace
	setProcessGroup(cmd)
	cmd.Stdout = j.stdout
	cmd.Stderr = j.stderr

	j.mu.Lock()
	j.cmd = cmd
	j.startedAt = time.Now().UTC()
	j.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		j.finish(cmd.Wait())
	}()
	return nil
}

func monitorShellJob(ctx context.Context, job *shellJob, timeout time.Duration, detached bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	if detached {
		select {
		case <-job.done:
		case <-timer.C:
			_ = job.requestStop(stopReasonTimeout)
		}
		return
	}
	select {
	case <-job.done:
	case <-ctx.Done():
		_ = job.requestStop(stopReasonCancelled)
	case <-timer.C:
		_ = job.requestStop(stopReasonTimeout)
	}
}

func (j *shellJob) finish(waitErr error) {
	j.mu.Lock()
	j.waitErr = waitErr
	if j.cmd != nil && j.cmd.ProcessState != nil {
		code := j.cmd.ProcessState.ExitCode()
		j.exitCode = &code
	}
	switch j.stopReason {
	case stopReasonCancelled:
		j.state = JobCancelled
		j.errText = "cancelled"
	case stopReasonTimeout:
		j.state = JobFailed
		j.errText = fmt.Sprintf("command timed out after %v", j.timeout)
	default:
		if waitErr == nil {
			j.state = JobCompleted
		} else {
			j.state = JobFailed
			j.errText = waitErr.Error()
		}
	}
	j.finishedAt = time.Now().UTC()
	close(j.done)
	onDone := j.onDone
	j.mu.Unlock()
	if onDone != nil {
		onDone(j)
	}
}

func (j *shellJob) requestStop(reason stopReason) error {
	j.mu.Lock()
	if j.state != JobRunning || j.stopReason != stopReasonNone {
		j.mu.Unlock()
		return nil
	}
	j.stopReason = reason
	cmd := j.cmd
	grace := j.grace
	done := j.done
	j.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := killProcessGroup(cmd, syscall.SIGINT); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("interrupt job %s: %w", j.id, err)
	}
	go func(pid int) {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-done:
			return
		case <-timer.C:
			if err := killProcessGroupNeg(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return
			}
		}
	}(cmd.Process.Pid)
	return nil
}

func (j *shellJob) stateValue() JobState {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state
}

func (j *shellJob) finishedTime() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.finishedAt
}

// JobSnapshot is the JSON representation returned by async shell_exec and the
// job management tool.
type JobSnapshot struct {
	JobID           string     `json:"job_id"`
	State           JobState   `json:"state"`
	Command         string     `json:"command"`
	Workspace       string     `json:"workspace"`
	StartedAt       time.Time  `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	TimeoutSeconds  float64    `json:"timeout_seconds"`
	ExitCode        *int       `json:"exit_code,omitempty"`
	Error           string     `json:"error,omitempty"`
	CancelRequested bool       `json:"cancel_requested,omitempty"`
	StdoutTail      string     `json:"stdout_tail,omitempty"`
	StderrTail      string     `json:"stderr_tail,omitempty"`
	StdoutBytes     int64      `json:"stdout_bytes,omitempty"`
	StderrBytes     int64      `json:"stderr_bytes,omitempty"`
	StdoutTruncated bool       `json:"stdout_truncated,omitempty"`
	StderrTruncated bool       `json:"stderr_truncated,omitempty"`
	StdoutBinary    bool       `json:"stdout_binary,omitempty"`
	StderrBinary    bool       `json:"stderr_binary,omitempty"`
}

func (j *shellJob) snapshot(includeOutput bool, maxBytes int) JobSnapshot {
	j.mu.Lock()
	snapshot := JobSnapshot{
		JobID:           j.id,
		State:           j.state,
		Command:         truncateJobCommand(j.command),
		Workspace:       j.workspace,
		StartedAt:       j.startedAt,
		TimeoutSeconds:  j.timeout.Seconds(),
		Error:           j.errText,
		CancelRequested: j.stopReason == stopReasonCancelled,
	}
	if !j.finishedAt.IsZero() {
		finished := j.finishedAt
		snapshot.FinishedAt = &finished
	}
	if j.exitCode != nil {
		code := *j.exitCode
		snapshot.ExitCode = &code
	}
	j.mu.Unlock()
	if includeOutput {
		snapshot.StdoutTail, snapshot.StdoutBytes, snapshot.StdoutTruncated, snapshot.StdoutBinary = j.stdout.tailString(maxBytes)
		snapshot.StderrTail, snapshot.StderrBytes, snapshot.StderrTruncated, snapshot.StderrBinary = j.stderr.tailString(maxBytes)
	}
	return snapshot
}

func truncateJobCommand(command string) string {
	const max = 512
	if len(command) <= max {
		return command
	}
	return command[:max] + "…"
}

// JobTool manages background jobs created by shell_exec with async:true.
func (s *ShellExec) JobTool(ctx context.Context, args map[string]interface{}) (string, error) {
	action, _ := args["action"].(string)
	action = strings.TrimSpace(action)
	if action == "" {
		return "", fmt.Errorf("job action is required (list, status, tail, wait, or cancel)")
	}
	workspace, err := s.resolveWorkspace(ctx, args)
	if err != nil {
		return "", err
	}
	owner := jobOwnerFromContext(ctx, workspace)
	if action == "list" {
		return marshalJobSnapshot(struct {
			Jobs []JobSnapshot `json:"jobs"`
		}{Jobs: s.jobs.list(owner)})
	}
	if action != "status" && action != "tail" && action != "wait" && action != "cancel" {
		return "", fmt.Errorf("unknown job action %q (use list, status, tail, wait, or cancel)", action)
	}
	id, _ := args["job_id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("job_id is required for %s", action)
	}
	job, err := s.jobs.get(owner, id)
	if err != nil {
		return "", err
	}

	switch action {
	case "status":
		return marshalJobSnapshot(job.snapshot(false, 0))
	case "tail":
		maxBytes, err := positiveIntArg(args, "max_bytes", s.jobs.config().OutputBytes)
		if err != nil {
			return "", err
		}
		if limit := s.jobs.config().OutputBytes; maxBytes > limit {
			maxBytes = limit
		}
		return marshalJobSnapshot(job.snapshot(true, maxBytes))
	case "wait":
		waitFor, err := shellTimeout(args, defaultShellTimeout)
		if err != nil {
			return "", err
		}
		timer := time.NewTimer(waitFor)
		defer timer.Stop()
		waitTimedOut := false
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("wait for job %s: %w", id, ctx.Err())
		case <-job.done:
		case <-timer.C:
			waitTimedOut = true
		}
		return marshalJobSnapshot(struct {
			JobSnapshot
			WaitTimedOut bool `json:"wait_timed_out,omitempty"`
		}{JobSnapshot: job.snapshot(true, s.jobs.config().OutputBytes), WaitTimedOut: waitTimedOut})
	case "cancel":
		if err := job.requestStop(stopReasonCancelled); err != nil {
			return "", err
		}
		timer := time.NewTimer(s.jobs.config().TerminationGrace + 250*time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("cancel job %s: %w", id, ctx.Err())
		case <-job.done:
		case <-timer.C:
		}
		return marshalJobSnapshot(job.snapshot(false, 0))
	}
	panic("unreachable")
}

func positiveIntArg(args map[string]interface{}, key string, fallback int) (int, error) {
	raw, ok := args[key]
	if !ok {
		return fallback, nil
	}
	var value int
	switch number := raw.(type) {
	case int:
		value = number
	case int64:
		value = int(number)
	case float64:
		if number != float64(int(number)) {
			return 0, fmt.Errorf("%s must be a positive integer", key)
		}
		value = int(number)
	default:
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", key)
	}
	return value, nil
}

func marshalJobSnapshot(value interface{}) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode job result: %w", err)
	}
	return string(encoded), nil
}

// boundedCapture retains only a fixed-size prefix for legacy synchronous
// output plus a fixed-size tail for job inspection. total records the complete
// byte count without allowing command output to grow process memory without
// bound.
type boundedCapture struct {
	mu      sync.Mutex
	limit   int
	prefix  []byte
	tail    []byte
	total   int64
	invalid bool
	pending []byte
}

func newBoundedCapture(limit int) *boundedCapture {
	if limit <= 0 {
		limit = defaultJobOutputBytes
	}
	return &boundedCapture{limit: limit}
}

func (b *boundedCapture) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	originalLen := len(p)
	b.total += int64(originalLen)
	b.feedUTF8Locked(p)
	if len(b.prefix) < legacyShellOutputBytes {
		remaining := legacyShellOutputBytes - len(b.prefix)
		if remaining > len(p) {
			remaining = len(p)
		}
		b.prefix = append(b.prefix, p[:remaining]...)
	}
	if len(p) >= b.limit {
		b.tail = append(b.tail[:0], p[len(p)-b.limit:]...)
	} else {
		overflow := len(b.tail) + len(p) - b.limit
		if overflow > 0 {
			copy(b.tail, b.tail[overflow:])
			b.tail = b.tail[:len(b.tail)-overflow]
		}
		b.tail = append(b.tail, p...)
	}
	return originalLen, nil
}

func (b *boundedCapture) feedUTF8Locked(p []byte) {
	if b.invalid {
		return
	}
	data := p
	if len(b.pending) > 0 {
		combined := make([]byte, 0, len(b.pending)+len(p))
		combined = append(combined, b.pending...)
		combined = append(combined, p...)
		b.pending = b.pending[:0]
		data = combined
	}
	for len(data) > 0 {
		if !utf8.FullRune(data) {
			b.pending = append(b.pending[:0], data...)
			return
		}
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 {
			b.invalid = true
			b.pending = b.pending[:0]
			return
		}
		data = data[size:]
	}
}

func (b *boundedCapture) legacyString() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.total == 0 {
		return ""
	}
	if b.invalid || len(b.pending) != 0 {
		return fmt.Sprintf("[binary output suppressed: %d bytes]", b.total)
	}
	result := string(b.prefix)
	if b.total > int64(len(b.prefix)) {
		result += fmt.Sprintf("\n[...truncated %d bytes...]", b.total-int64(len(b.prefix)))
	}
	return result
}

func (b *boundedCapture) tailString(maxBytes int) (value string, total int64, truncated, binary bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if maxBytes <= 0 || maxBytes > b.limit {
		maxBytes = b.limit
	}
	start := len(b.tail) - maxBytes
	if start < 0 {
		start = 0
	}
	data := b.tail[start:]
	total = b.total
	truncated = total > int64(len(data))
	binary = b.invalid
	if binary {
		return fmt.Sprintf("[binary output suppressed: %d bytes]", total), total, truncated, true
	}
	return strings.ToValidUTF8(string(data), "�"), total, truncated, false
}
