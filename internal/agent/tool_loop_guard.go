package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/sashabaranov/go-openai"
)

const (
	// A repeated call receives a corrective tool result before the agent is
	// stopped. This gives a model several chances to change course without
	// allowing an unlimited no-progress loop when max_turns is intentionally 0.
	repeatedToolCallWarningAt = 3
	repeatedToolCallAbortAt   = 6
)

// ErrRepeatedToolCallLoop identifies semantic no-progress termination. It is
// deliberately separate from max_turns: distinct tool work remains unlimited.
var ErrRepeatedToolCallLoop = &repeatedToolCallLoopError{}

type repeatedToolCallLoopError struct{}

func (*repeatedToolCallLoopError) Error() string {
	return "repeated identical tool-call loop detected"
}

type toolLoopDecision struct {
	Consecutive int
	Skip        bool
	Abort       bool
}

type toolLoopGuard struct {
	lastFingerprint string
	consecutive     int
}

// Observe records one assistant tool-call round. Call IDs are intentionally
// ignored because providers generate a new ID even when the model repeats the
// exact same request. JSON arguments are canonicalized so key order does not
// disguise an otherwise identical call.
func (g *toolLoopGuard) Observe(calls []openai.ToolCall) toolLoopDecision {
	fingerprint := toolCallGroupFingerprint(calls)
	if fingerprint == "" || fingerprint != g.lastFingerprint {
		g.lastFingerprint = fingerprint
		g.consecutive = 1
	} else {
		g.consecutive++
	}
	return toolLoopDecision{
		Consecutive: g.consecutive,
		Skip:        g.consecutive >= repeatedToolCallWarningAt,
		Abort:       g.consecutive >= repeatedToolCallAbortAt,
	}
}

func toolCallGroupFingerprint(calls []openai.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	h := sha256.New()
	for _, call := range calls {
		h.Write([]byte(call.Type))
		h.Write([]byte{0})
		h.Write([]byte(call.Function.Name))
		h.Write([]byte{0})
		h.Write([]byte(canonicalToolArguments(call.Function.Arguments)))
		h.Write([]byte{0xff})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalToolArguments(raw string) string {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value interface{}
	if err := decoder.Decode(&value); err != nil {
		return strings.TrimSpace(raw)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return strings.TrimSpace(raw)
	}
	return string(encoded)
}

func repeatedToolCallResult(decision toolLoopDecision) string {
	message := "error: repeated identical tool call detected; this call was not executed. " +
		"Do not repeat it unchanged—use different arguments or take a different action."
	if decision.Abort {
		message += " The agent is stopping to prevent an infinite no-progress loop."
	}
	return message
}
