package tools

import (
	"context"
	"fmt"
)

// gitDispatchInWorkspace returns a dispatcher whose default repository root is
// the configured workspace. A request-scoped workspace injected by the agent
// takes precedence.
func gitDispatchInWorkspace(workspace string) func(context.Context, map[string]interface{}) (string, error) {
	return func(ctx context.Context, args map[string]interface{}) (string, error) {
		if WorkspaceFromContext(ctx) != "" || workspace == "" {
			return GitDispatch(ctx, args)
		}
		scopedArgs := make(map[string]interface{}, len(args)+1)
		for key, value := range args {
			scopedArgs[key] = value
		}
		scopedArgs["workspace"] = workspace
		return GitDispatch(ctx, scopedArgs)
	}
}

// GitDispatch routes git operations by action.
func GitDispatch(ctx context.Context, args map[string]interface{}) (string, error) {
	action, _ := args["action"].(string)

	// The request-scoped workspace is selected by the server/CLI and must take
	// precedence over model-supplied arguments. The argument remains a fallback
	// for direct programmatic callers that do not have request context.
	workspace := WorkspaceFromContext(ctx)
	if workspace == "" {
		workspace, _ = args["workspace"].(string)
	}
	if workspace == "" {
		workspace = "./workspace"
	}
	gt := NewGitTools(workspace)

	switch action {
	case "status":
		return gt.GitStatus(ctx, args)
	case "diff":
		return gt.GitDiff(ctx, args)
	case "add":
		return gt.GitAdd(ctx, args)
	case "commit":
		return gt.GitCommit(ctx, args)
	default:
		return "", fmt.Errorf("unknown git action: %s (use status, diff, add, commit)", action)
	}
}
