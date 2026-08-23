package tools

import (
	"context"
	"fmt"
	"strings"
)

func filePreconditionSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":        "string",
		"description": "Full SHA-256 returned by file_read/current mutation result. Use \"absent\" only when creating a new file.",
		"anyOf": []interface{}{
			map[string]interface{}{"type": "string", "const": ExpectedFileAbsent},
			map[string]interface{}{"type": "string", "pattern": "^[A-Fa-f0-9]{64}$"},
		},
	}
}

func existingFilePreconditionSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":        "string",
		"pattern":     "^[A-Fa-f0-9]{64}$",
		"description": "Full SHA-256 returned by file_read/current mutation result. file_patch only edits an existing file.",
	}
}

func fileToolMetadata(tier AccessTier, action string) ToolMetadata {
	return ToolMetadata{Tier: tier, Resolver: func(_ context.Context, target ApprovalTarget) (ApprovalTarget, error) {
		target.Action = action
		target.Resource, _ = target.Arguments["path"].(string)
		return target, nil
	}}
}

func argumentToolMetadata(tier AccessTier, action, key string) ToolMetadata {
	return ToolMetadata{Tier: tier, Resolver: func(_ context.Context, target ApprovalTarget) (ApprovalTarget, error) {
		target.Action = action
		target.Resource, _ = target.Arguments[key].(string)
		return target, nil
	}}
}

func shellToolMetadata() ToolMetadata {
	return ToolMetadata{Tier: TierExec, Resolver: func(_ context.Context, target ApprovalTarget) (ApprovalTarget, error) {
		command, _ := target.Arguments["command"].(string)
		if strings.TrimSpace(command) == "" {
			return target, fmt.Errorf("shell command is empty")
		}
		target.Action = "execute"
		target.Resource = command
		return target, nil
	}}
}

func gitToolMetadata() ToolMetadata {
	return ToolMetadata{Tier: TierExec, Resolver: func(_ context.Context, target ApprovalTarget) (ApprovalTarget, error) {
		action, _ := target.Arguments["action"].(string)
		target.Action = action
		target.Resource, _ = target.Arguments["repo_path"].(string)
		switch action {
		case "status", "diff":
			target.Tier = TierRead
		case "add":
			target.Tier = TierWrite
		case "commit":
			// A commit invokes Git hooks and can therefore execute arbitrary
			// repository-controlled programs, even though its primary effect is
			// writing repository state.
			target.Tier = TierExec
		case "":
			return target, fmt.Errorf("git action is required")
		default:
			return target, fmt.Errorf("unsupported git action %q", action)
		}
		return target, nil
	}}
}

func todoToolMetadata() ToolMetadata {
	return ToolMetadata{Tier: TierWrite, Resolver: func(_ context.Context, target ApprovalTarget) (ApprovalTarget, error) {
		action, _ := target.Arguments["action"].(string)
		target.Action = action
		switch action {
		case "list":
			target.Tier = TierRead
		case "create", "update", "complete", "cancel":
			target.Tier = TierWrite
		case "":
			return target, fmt.Errorf("todo action is required")
		default:
			return target, fmt.Errorf("unsupported todo action %q", action)
		}
		return target, nil
	}}
}

func jobToolMetadata() ToolMetadata {
	return ToolMetadata{Tier: TierRead, Resolver: func(_ context.Context, target ApprovalTarget) (ApprovalTarget, error) {
		action, _ := target.Arguments["action"].(string)
		target.Action = action
		target.Resource, _ = target.Arguments["job_id"].(string)
		switch action {
		case "list", "status", "tail", "wait":
			target.Tier = TierRead
		case "cancel":
			target.Tier = TierExec
		case "":
			return target, fmt.Errorf("job action is required")
		default:
			return target, fmt.Errorf("unsupported job action %q", action)
		}
		return target, nil
	}}
}
