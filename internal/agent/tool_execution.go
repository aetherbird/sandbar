package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/sashabaranov/go-openai"

	"github.com/aetherbird/sandbar/internal/tools"
)

const maxParallelToolCalls = 8

func toolBatchCanRunConcurrently(calls []openai.ToolCall, canRunConcurrently func(string) bool) bool {
	if len(calls) < 2 {
		return false
	}
	if canRunConcurrently == nil {
		return false
	}
	for _, call := range calls {
		if call.Type != openai.ToolTypeFunction {
			return false
		}
		if !canRunConcurrently(call.Function.Name) {
			return false
		}
	}
	return true
}

// executeToolCallBatch runs independent calls concurrently but always returns
// one result per input index. Emission and persistence remain the caller's job,
// allowing it to commit results in provider order after all workers finish.
func (a *Agent) executeToolCallBatch(ctx context.Context, calls []openai.ToolCall, workspace string, decision toolLoopDecision) []string {
	return executeToolCallBatchWith(ctx, calls, decision, a.tools.CanRunConcurrently, func(callCtx context.Context, call openai.ToolCall) string {
		return a.executeOneToolCall(callCtx, call, workspace)
	})
}

func executeToolCallBatchWith(ctx context.Context, calls []openai.ToolCall, decision toolLoopDecision, canRunConcurrently func(string) bool, execute func(context.Context, openai.ToolCall) string) []string {
	results := make([]string, len(calls))
	if decision.Skip {
		output := repeatedToolCallResult(decision)
		for i := range results {
			results[i] = output
		}
		return results
	}

	if !toolBatchCanRunConcurrently(calls, canRunConcurrently) {
		for i, call := range calls {
			if err := ctx.Err(); err != nil {
				results[i] = fmt.Sprintf("error: tool call was not completed: %s", err.Error())
				continue
			}
			results[i] = execute(ctx, call)
		}
		return results
	}

	workers := len(calls)
	if workers > maxParallelToolCalls {
		workers = maxParallelToolCalls
	}
	jobs := make(chan int, len(calls))
	for i := range calls {
		jobs <- i
	}
	close(jobs)

	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for i := range jobs {
				if err := ctx.Err(); err != nil {
					results[i] = fmt.Sprintf("error: tool call was not completed: %s", err.Error())
					continue
				}
				results[i] = execute(ctx, calls[i])
			}
		}()
	}
	wg.Wait()
	return results
}

func (a *Agent) executeOneToolCall(ctx context.Context, call openai.ToolCall, workspace string) string {
	if call.Type != openai.ToolTypeFunction {
		return fmt.Sprintf("error: unsupported tool call type %q", call.Type)
	}

	callCtx := tools.WithToolCallID(ctx, call.ID)
	var cancelDone chan struct{}
	if call.Function.Name == "shell_exec" {
		// shell_exec batches are sequential. Preserve graceful interruption for
		// the one active command before CommandContext escalates termination.
		cancelDone = make(chan struct{})
		go func() {
			select {
			case <-callCtx.Done():
				// Registry instances are shared by every server request. Scope the
				// graceful interrupt to this call's thread/workspace owner.
				_ = a.tools.CancelActiveToolFor(callCtx)
			case <-cancelDone:
			}
		}()
	}

	output, err := a.executeTool(callCtx, call.Function.Name, call.Function.Arguments, workspace)
	if cancelDone != nil {
		close(cancelDone)
	}
	if err != nil {
		return fmt.Sprintf("error: %s", err.Error())
	}
	return output
}
