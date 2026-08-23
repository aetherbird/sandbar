package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func jobTestContext(threadID, workspace string) context.Context {
	ctx := WithThreadID(context.Background(), threadID)
	return WithWorkspace(ctx, workspace)
}

func startTestJob(t *testing.T, shell *ShellExec, ctx context.Context, command string, extra map[string]interface{}) JobSnapshot {
	t.Helper()
	args := map[string]interface{}{"command": command, "async": true}
	for key, value := range extra {
		args[key] = value
	}
	out, err := shell.Execute(ctx, args)
	if err != nil {
		t.Fatalf("start async job: %v", err)
	}
	var snapshot JobSnapshot
	if err := json.Unmarshal([]byte(out), &snapshot); err != nil {
		t.Fatalf("decode async result %q: %v", out, err)
	}
	if !strings.HasPrefix(snapshot.JobID, "job_") {
		t.Fatalf("unexpected job id %q", snapshot.JobID)
	}
	return snapshot
}

func callJobTool(t *testing.T, shell *ShellExec, ctx context.Context, args map[string]interface{}) JobSnapshot {
	t.Helper()
	out, err := shell.JobTool(ctx, args)
	if err != nil {
		t.Fatalf("job tool: %v", err)
	}
	var snapshot JobSnapshot
	if err := json.Unmarshal([]byte(out), &snapshot); err != nil {
		t.Fatalf("decode job result %q: %v", out, err)
	}
	return snapshot
}

func TestAsyncShellJobLifecycleAndTail(t *testing.T) {
	workspace := t.TempDir()
	shell := NewShellExecWithJobConfig(workspace, nil, JobSupervisorConfig{
		OutputBytes: 128,
		Retention:   time.Minute,
	})
	ctx := jobTestContext("thread-a", workspace)
	started := startTestJob(t, shell, ctx, "printf begin; sleep 0.05; printf end", nil)

	finished := callJobTool(t, shell, ctx, map[string]interface{}{
		"action":          "wait",
		"job_id":          started.JobID,
		"timeout_seconds": 2,
	})
	if finished.State != JobCompleted {
		t.Fatalf("state = %q, want completed (error %q)", finished.State, finished.Error)
	}
	if finished.ExitCode == nil || *finished.ExitCode != 0 {
		t.Fatalf("exit code = %v, want 0", finished.ExitCode)
	}
	if finished.StdoutTail != "beginend" {
		t.Fatalf("stdout tail = %q, want beginend", finished.StdoutTail)
	}
	if finished.StartedAt.IsZero() || finished.FinishedAt == nil {
		t.Fatalf("missing lifecycle timestamps: %+v", finished)
	}

	status := callJobTool(t, shell, ctx, map[string]interface{}{"action": "status", "job_id": started.JobID})
	if status.StdoutTail != "" {
		t.Fatalf("status unexpectedly included output: %q", status.StdoutTail)
	}
	tail := callJobTool(t, shell, ctx, map[string]interface{}{"action": "tail", "job_id": started.JobID, "max_bytes": 3})
	if tail.StdoutTail != "end" || !tail.StdoutTruncated || tail.StdoutBytes != 8 {
		t.Fatalf("unexpected bounded tail: %+v", tail)
	}
}

func TestAsyncShellJobNonzeroAndTimeoutStates(t *testing.T) {
	workspace := t.TempDir()
	shell := NewShellExecWithJobConfig(workspace, nil, JobSupervisorConfig{TerminationGrace: 20 * time.Millisecond})
	ctx := jobTestContext("thread-a", workspace)

	failed := startTestJob(t, shell, ctx, "printf nope >&2; exit 7", nil)
	failed = callJobTool(t, shell, ctx, map[string]interface{}{
		"action": "wait", "job_id": failed.JobID, "timeout_seconds": 2,
	})
	if failed.State != JobFailed || failed.ExitCode == nil || *failed.ExitCode != 7 || failed.StderrTail != "nope" {
		t.Fatalf("unexpected failed job: %+v", failed)
	}

	timedOut := startTestJob(t, shell, ctx, "sleep 30", map[string]interface{}{"timeout_seconds": 0.05})
	timedOut = callJobTool(t, shell, ctx, map[string]interface{}{
		"action": "wait", "job_id": timedOut.JobID, "timeout_seconds": 2,
	})
	if timedOut.State != JobFailed || !strings.Contains(timedOut.Error, "timed out") {
		t.Fatalf("unexpected timed-out job: %+v", timedOut)
	}
}

func TestAsyncShellJobOwnerIsolation(t *testing.T) {
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	shell := NewShellExec(workspaceA, nil)
	ownerA := jobTestContext("thread-a", workspaceA)
	otherThread := jobTestContext("thread-b", workspaceA)
	otherWorkspace := jobTestContext("thread-a", workspaceB)

	job := startTestJob(t, shell, ownerA, "sleep 30", nil)
	defer func() {
		_, _ = shell.JobTool(ownerA, map[string]interface{}{"action": "cancel", "job_id": job.JobID})
	}()

	for name, ctx := range map[string]context.Context{
		"other thread":    otherThread,
		"other workspace": otherWorkspace,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := shell.JobTool(ctx, map[string]interface{}{"action": "status", "job_id": job.JobID})
			if err == nil || !strings.Contains(err.Error(), "not found") {
				t.Fatalf("cross-owner lookup error = %v, want not found", err)
			}
			out, err := shell.JobTool(ctx, map[string]interface{}{"action": "list"})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			var listed struct {
				Jobs []JobSnapshot `json:"jobs"`
			}
			if err := json.Unmarshal([]byte(out), &listed); err != nil {
				t.Fatalf("decode list: %v", err)
			}
			if len(listed.Jobs) != 0 {
				t.Fatalf("cross-owner list leaked jobs: %+v", listed.Jobs)
			}
		})
	}
}

func TestAsyncShellJobCancellationEscalates(t *testing.T) {
	workspace := t.TempDir()
	shell := NewShellExecWithJobConfig(workspace, nil, JobSupervisorConfig{
		TerminationGrace: 40 * time.Millisecond,
	})
	ctx := jobTestContext("thread-cancel", workspace)
	// Both bash and its child ignore SIGINT, forcing the supervisor to escalate
	// to SIGKILL for the entire process group after the grace period.
	job := startTestJob(t, shell, ctx, "trap '' INT; (trap '' INT; sleep 30) & wait", nil)
	started := time.Now()
	cancelled := callJobTool(t, shell, ctx, map[string]interface{}{"action": "cancel", "job_id": job.JobID})
	if cancelled.State != JobCancelled {
		t.Fatalf("state = %q, want cancelled: %+v", cancelled.State, cancelled)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("cancel escalation took too long: %v", elapsed)
	}
}

func TestAsyncShellJobOutputIsBounded(t *testing.T) {
	workspace := t.TempDir()
	shell := NewShellExecWithJobConfig(workspace, nil, JobSupervisorConfig{OutputBytes: 64})
	ctx := jobTestContext("thread-output", workspace)
	job := startTestJob(t, shell, ctx, "printf '%0200d' 0", nil)
	finished := callJobTool(t, shell, ctx, map[string]interface{}{
		"action": "wait", "job_id": job.JobID, "timeout_seconds": 2,
	})
	if finished.StdoutBytes != 200 || len(finished.StdoutTail) != 64 || !finished.StdoutTruncated {
		t.Fatalf("unexpected capture bounds: bytes=%d len(tail)=%d truncated=%v", finished.StdoutBytes, len(finished.StdoutTail), finished.StdoutTruncated)
	}
	if got := len(jobFromSupervisor(t, shell, job.JobID).stdout.tail); got > 64 {
		t.Fatalf("retained stdout = %d bytes, limit 64", got)
	}
}

func jobFromSupervisor(t *testing.T, shell *ShellExec, id string) *shellJob {
	t.Helper()
	shell.jobs.mu.Lock()
	defer shell.jobs.mu.Unlock()
	job := shell.jobs.jobs[id]
	if job == nil {
		t.Fatalf("job %q not retained", id)
	}
	return job
}

func TestAsyncShellJobCapacityAndTTL(t *testing.T) {
	workspace := t.TempDir()
	shell := NewShellExecWithJobConfig(workspace, nil, JobSupervisorConfig{
		MaxJobs:          2,
		MaxRunning:       1,
		Retention:        30 * time.Millisecond,
		TerminationGrace: 20 * time.Millisecond,
	})
	ctx := jobTestContext("thread-limits", workspace)
	first := startTestJob(t, shell, ctx, "sleep 30", nil)
	if _, err := shell.Execute(ctx, map[string]interface{}{"command": "sleep 30", "async": true}); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("second running job error = %v, want capacity error", err)
	}
	callJobTool(t, shell, ctx, map[string]interface{}{"action": "cancel", "job_id": first.JobID})

	completed := startTestJob(t, shell, ctx, "true", nil)
	callJobTool(t, shell, ctx, map[string]interface{}{"action": "wait", "job_id": completed.JobID, "timeout_seconds": 2})
	deadline := time.Now().Add(time.Second)
	for {
		out, err := shell.JobTool(ctx, map[string]interface{}{"action": "list"})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var listed struct {
			Jobs []JobSnapshot `json:"jobs"`
		}
		if err := json.Unmarshal([]byte(out), &listed); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		if len(listed.Jobs) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("jobs were not reaped after TTL: %+v", listed.Jobs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAsyncShellJobsConcurrentOwners(t *testing.T) {
	workspace := t.TempDir()
	shell := NewShellExecWithJobConfig(workspace, nil, JobSupervisorConfig{
		MaxJobs:     64,
		MaxRunning:  32,
		OutputBytes: 128,
	})

	const owners = 12
	var wg sync.WaitGroup
	errs := make(chan error, owners)
	for i := 0; i < owners; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := jobTestContext(fmt.Sprintf("thread-%d", i), workspace)
			out, err := shell.Execute(ctx, map[string]interface{}{
				"command": fmt.Sprintf("printf owner-%d", i),
				"async":   true,
			})
			if err != nil {
				errs <- err
				return
			}
			var started JobSnapshot
			if err := json.Unmarshal([]byte(out), &started); err != nil {
				errs <- err
				return
			}
			waited, err := shell.JobTool(ctx, map[string]interface{}{
				"action": "wait", "job_id": started.JobID, "timeout_seconds": 2,
			})
			if err != nil {
				errs <- err
				return
			}
			var finished JobSnapshot
			if err := json.Unmarshal([]byte(waited), &finished); err != nil {
				errs <- err
				return
			}
			if finished.State != JobCompleted || finished.StdoutTail != fmt.Sprintf("owner-%d", i) {
				errs <- fmt.Errorf("owner %d got %+v", i, finished)
				return
			}
			list, err := shell.JobTool(ctx, map[string]interface{}{"action": "list"})
			if err != nil {
				errs <- err
				return
			}
			var listed struct {
				Jobs []JobSnapshot `json:"jobs"`
			}
			if err := json.Unmarshal([]byte(list), &listed); err != nil {
				errs <- err
				return
			}
			if len(listed.Jobs) != 1 || listed.Jobs[0].JobID != started.JobID {
				errs <- fmt.Errorf("owner %d list leaked jobs: %+v", i, listed.Jobs)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestShellExecCancelAllConcurrentSyncCommands(t *testing.T) {
	workspace := t.TempDir()
	shell := NewShellExecWithJobConfig(workspace, nil, JobSupervisorConfig{TerminationGrace: 20 * time.Millisecond})
	const count = 3
	started := make(chan struct{}, count)
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() {
			started <- struct{}{}
			_, err := shell.Execute(context.Background(), map[string]interface{}{"command": "sleep 30"})
			errs <- err
		}()
	}
	for i := 0; i < count; i++ {
		<-started
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		shell.syncMu.Lock()
		active := len(shell.syncJobs)
		shell.syncMu.Unlock()
		if active == count {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d sync commands became active", active, count)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := shell.Cancel(); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	for i := 0; i < count; i++ {
		if err := <-errs; err == nil || !strings.Contains(err.Error(), "cancel") {
			t.Errorf("command %d error = %v, want cancellation", i, err)
		}
	}
}

func TestShellExecCancelOwnerDoesNotInterruptOtherThreads(t *testing.T) {
	workspace := t.TempDir()
	shell := NewShellExecWithJobConfig(workspace, nil, JobSupervisorConfig{TerminationGrace: 20 * time.Millisecond})
	ctxA := jobTestContext("thread-a", workspace)
	ctxB := jobTestContext("thread-b", workspace)
	errA := make(chan error, 1)
	errB := make(chan error, 1)
	go func() {
		_, err := shell.Execute(ctxA, map[string]interface{}{"command": "sleep 30"})
		errA <- err
	}()
	go func() {
		_, err := shell.Execute(ctxB, map[string]interface{}{"command": "sleep 30"})
		errB <- err
	}()
	waitForSyncJobs(t, shell, 2)
	if err := shell.CancelOwner(ctxA); err != nil {
		t.Fatalf("cancel owner A: %v", err)
	}
	select {
	case err := <-errA:
		if err == nil || !strings.Contains(err.Error(), "cancel") {
			t.Fatalf("owner A error = %v, want cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owner A command was not cancelled")
	}
	select {
	case err := <-errB:
		t.Fatalf("owner B command was interrupted: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := shell.CancelOwner(ctxB); err != nil {
		t.Fatalf("cancel owner B: %v", err)
	}
	<-errB
}

func TestShellExecTrustedContextOverridesWorkspaceArgument(t *testing.T) {
	trusted := t.TempDir()
	modelSupplied := t.TempDir()
	shell := NewShellExec(trusted, nil)
	defer func() { _ = shell.Close(context.Background()) }()
	ctx := jobTestContext("thread-workspace", trusted)
	out, err := shell.Execute(ctx, map[string]interface{}{
		"command": "pwd", "workspace": modelSupplied,
	})
	if err != nil {
		t.Fatalf("execute shell: %v", err)
	}
	if !strings.Contains(out, trusted) || strings.Contains(out, modelSupplied) {
		t.Fatalf("shell output %q does not use trusted workspace %q", out, trusted)
	}
}

func waitForSyncJobs(t *testing.T, shell *ShellExec, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		shell.syncMu.Lock()
		active := len(shell.syncJobs)
		shell.syncMu.Unlock()
		if active == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("active sync jobs = %d, want %d", active, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestShellExecCloseTearsDownJobsAndTimers(t *testing.T) {
	workspace := t.TempDir()
	shell := NewShellExecWithJobConfig(workspace, nil, JobSupervisorConfig{
		Retention: time.Minute, TerminationGrace: 20 * time.Millisecond,
	})
	ctx := jobTestContext("thread-close", workspace)
	started := startTestJob(t, shell, ctx, "trap '' INT; sleep 30", nil)
	job := jobFromSupervisor(t, shell, started.JobID)
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := shell.Close(closeCtx); err != nil {
		t.Fatalf("close shell: %v", err)
	}
	if state := job.stateValue(); state != JobCancelled {
		t.Fatalf("closed job state = %q, want cancelled", state)
	}
	shell.jobs.mu.Lock()
	jobs, timers, closed := len(shell.jobs.jobs), len(shell.jobs.timers), shell.jobs.closed
	shell.jobs.mu.Unlock()
	if jobs != 0 || timers != 0 || !closed {
		t.Fatalf("closed supervisor jobs=%d timers=%d closed=%v", jobs, timers, closed)
	}
	if _, err := shell.Execute(ctx, map[string]interface{}{"command": "true", "async": true}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("execute after close error = %v", err)
	}
}

func TestShellExecCloseContinuesTeardownAfterCallerCancellation(t *testing.T) {
	workspace := t.TempDir()
	shell := NewShellExecWithJobConfig(workspace, nil, JobSupervisorConfig{
		Retention: time.Minute, TerminationGrace: 30 * time.Millisecond,
	})
	ctx := jobTestContext("thread-close-cancel", workspace)
	started := startTestJob(t, shell, ctx, "trap '' INT; sleep 30", nil)
	job := jobFromSupervisor(t, shell, started.JobID)
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := shell.Close(cancelledCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("close with cancelled caller error = %v, want context canceled", err)
	}
	select {
	case <-shell.jobs.closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor teardown stopped when Close caller cancelled")
	}
	if state := job.stateValue(); state != JobCancelled {
		t.Fatalf("eventually closed job state = %q, want cancelled", state)
	}
	shell.jobs.mu.Lock()
	jobs, timers := len(shell.jobs.jobs), len(shell.jobs.timers)
	shell.jobs.mu.Unlock()
	if jobs != 0 || timers != 0 {
		t.Fatalf("eventual supervisor cleanup jobs=%d timers=%d", jobs, timers)
	}
}

func TestJobEvictionStopsRetentionTimer(t *testing.T) {
	workspace := t.TempDir()
	shell := NewShellExecWithJobConfig(workspace, nil, JobSupervisorConfig{
		MaxJobs: 1, MaxRunning: 1, Retention: time.Minute,
	})
	ctx := jobTestContext("thread-timer", workspace)
	first := startTestJob(t, shell, ctx, "true", nil)
	callJobTool(t, shell, ctx, map[string]interface{}{"action": "wait", "job_id": first.JobID, "timeout_seconds": 2})
	shell.jobs.mu.Lock()
	oldTimer := shell.jobs.timers[first.JobID]
	shell.jobs.mu.Unlock()
	if oldTimer == nil {
		t.Fatal("completed job has no retention timer")
	}
	second := startTestJob(t, shell, ctx, "sleep 30", nil)
	defer func() { _, _ = shell.JobTool(ctx, map[string]interface{}{"action": "cancel", "job_id": second.JobID}) }()
	shell.jobs.mu.Lock()
	_, oldJobRetained := shell.jobs.jobs[first.JobID]
	_, oldTimerRetained := shell.jobs.timers[first.JobID]
	shell.jobs.mu.Unlock()
	if oldJobRetained || oldTimerRetained {
		t.Fatalf("evicted job retained: job=%v timer=%v", oldJobRetained, oldTimerRetained)
	}
	if oldTimer.Stop() {
		t.Fatal("eviction left the old retention timer active")
	}
}

func TestJobToolValidation(t *testing.T) {
	workspace := t.TempDir()
	shell := NewShellExec(workspace, nil)
	ctx := jobTestContext("thread-validation", workspace)
	for _, args := range []map[string]interface{}{
		{},
		{"action": "wat"},
		{"action": "status"},
		{"action": "tail", "job_id": "missing", "max_bytes": 0},
	} {
		if _, err := shell.JobTool(ctx, args); err == nil {
			t.Errorf("JobTool(%v) unexpectedly succeeded", args)
		}
	}
	if _, err := shell.Execute(ctx, map[string]interface{}{"command": "true", "async": "yes"}); err == nil {
		t.Fatal("non-boolean async unexpectedly succeeded")
	}
	subagentCtx := WithSubagentTaskID(ctx, "task-1")
	if _, err := shell.Execute(subagentCtx, map[string]interface{}{"command": "true", "async": true}); err == nil || !strings.Contains(err.Error(), "unavailable inside subagents") {
		t.Fatalf("subagent async error = %v, want an orphan-prevention error", err)
	}
}
