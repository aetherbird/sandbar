package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aetherbird/sandbar/internal/llm"
	"github.com/aetherbird/sandbar/internal/memory"
)

// seedThreadWithTodos creates a thread with one durable todo item.
func seedThreadWithTodos(t *testing.T, store *memory.Store) *memory.Thread {
	t.Helper()
	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if _, err := store.CreateTodos(thread.ID, []memory.TodoItem{
		{Content: "draft the plan", Status: memory.TodoPending},
	}); err != nil {
		t.Fatalf("seed todos: %v", err)
	}
	return thread
}

func TestBuildMessagesTodoReminder(t *testing.T) {
	a := newTestAgentWithFakeSummarizer(t, &fakeSummarizer{})

	// A thread without a todo list gets no reminder block.
	empty, err := a.store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	msgs := mustBuildMessages(t, a, empty.ID, a.cfg.Workspace, "")
	for _, m := range msgs {
		if m.Kind == "todo_reminder" || strings.Contains(m.Msg.Content, "[Task list for this thread") {
			t.Fatalf("todo reminder injected for a thread without todos: %+v", m)
		}
	}

	// A thread with a durable todo list gets the reminder right after the
	// system prompt, rendered with the todo tool's status icons.
	thread := seedThreadWithTodos(t, a.store)
	_, beforeMsgs, err := a.store.GetThreadWithMessages(thread.ID)
	if err != nil {
		t.Fatalf("load messages: %v", err)
	}
	msgs = mustBuildMessages(t, a, thread.ID, a.cfg.Workspace, "")
	if len(msgs) < 2 || msgs[1].Kind != "todo_reminder" || !msgs[1].Synthetic {
		t.Fatalf("expected synthetic todo_reminder at index 1: %+v", msgs)
	}
	content := msgs[1].Msg.Content
	if !strings.Contains(content, "[Task list for this thread") ||
		!strings.Contains(content, "[ ]") ||
		!strings.Contains(content, "draft the plan") {
		t.Fatalf("todo reminder content malformed: %q", content)
	}

	// The reminder is a provider-facing injection only: nothing is persisted.
	_, afterMsgs, err := a.store.GetThreadWithMessages(thread.ID)
	if err != nil {
		t.Fatalf("reload messages: %v", err)
	}
	if len(afterMsgs) != len(beforeMsgs) {
		t.Fatalf("buildMessages persisted messages: before=%d after=%d", len(beforeMsgs), len(afterMsgs))
	}
}

func TestBuildMessagesCliFormattingDirective(t *testing.T) {
	a := newTestAgentWithFakeSummarizer(t, &fakeSummarizer{})
	thread, err := a.store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	cli := mustBuildMessages(t, a, thread.ID, a.cfg.Workspace, "cli")
	if !strings.Contains(cli[0].Msg.Content, cliFormattingPrompt) {
		t.Fatalf("cli source missing formatting directive: %q", cli[0].Msg.Content)
	}
	for _, source := range []string{"", "web"} {
		msgs := mustBuildMessages(t, a, thread.ID, a.cfg.Workspace, source)
		if strings.Contains(msgs[0].Msg.Content, cliFormattingPrompt) {
			t.Fatalf("formatting directive leaked into source %q", source)
		}
	}
}

func TestChatRequestSourceFromContext(t *testing.T) {
	var firstBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if firstBody == "" {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read request body: %v", err)
			}
			firstBody = string(data)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}]}`)
	}))
	defer ts.Close()

	a, _, cleanup := setupTestAgent(t, true)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = ts.URL
	a.cfg.Persona.TitleModel = "missing-title-model"

	// The CLI annotates the turn context instead of setting Request.Source.
	ctx := WithRequestSource(context.Background(), "cli")
	if _, err := a.Chat(ctx, Request{
		ModelAlias:  "test-model",
		UserMessage: "hello",
	}, func(llm.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if !strings.Contains(firstBody, "the CLI renders standard Markdown") {
		t.Fatalf("context-carried source did not reach the system prompt: %q", firstBody)
	}
}

// chatRoundRecorder is a fake provider that answers with scripted tool-call
// rounds and records every raw request body it receives.
type chatRoundRecorder struct {
	t      *testing.T
	bodies []string
	calls  int
	rounds int // number of tool-call rounds before the terminal answer
	todoAt int // 1-based round that calls the todo tool instead; 0 = never
}

func (rec *chatRoundRecorder) handler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		rec.t.Errorf("read request body: %v", err)
	}
	rec.bodies = append(rec.bodies, string(body))
	rec.calls++
	if rec.calls <= rec.rounds {
		name := "file_read"
		args := fmt.Sprintf(`{"path":"f%d.txt"}`, rec.calls)
		if rec.calls == rec.todoAt {
			name = "todo"
			args = `{"action":"list"}`
		}
		respondJSONBody(w, body, fmt.Sprintf(`{
			"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test",
			"choices":[{
				"index":0,"message":{
					"role":"assistant","content":"",
					"tool_calls":[{
						"id":"call_%d",
						"type":"function",
						"function":{"name":%q,"arguments":%q}
					}]
				},
				"finish_reason":"tool_calls"
			}]
		}`, rec.calls, name, args))
		return
	}
	respondJSONBody(w, body, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Done"},"finish_reason":"stop"}]}`)
}

// nudgeMentions counts nudge occurrences across all recorded provider payloads.
func (rec *chatRoundRecorder) nudgeMentions() int {
	n := 0
	for _, body := range rec.bodies {
		n += strings.Count(body, "active task list")
	}
	return n
}

func runRecordedChat(t *testing.T, rec *chatRoundRecorder) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer ts.Close()

	a, store, cleanup := setupTestAgent(t, true)
	defer cleanup()
	a.cfg.Providers[0].BaseURL = ts.URL
	a.cfg.Persona.TitleModel = "missing-title-model"

	thread := seedThreadWithTodos(t, store)
	if _, err := a.Chat(context.Background(), Request{
		ThreadID:    thread.ID,
		ModelAlias:  "test-model",
		UserMessage: "work through the files",
	}, func(llm.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("chat: %v", err)
	}
}

func TestChatTodoNudgeAfterTwelveRounds(t *testing.T) {
	// 13 tool-call rounds, none touching the todo tool: the nudge must fire
	// after the 12th round and be injected exactly once.
	rec := &chatRoundRecorder{t: t, rounds: 13}
	runRecordedChat(t, rec)
	if rec.nudgeMentions() == 0 {
		t.Fatal("no todo nudge reached the provider after 12 todo-free rounds")
	}
	last := rec.bodies[len(rec.bodies)-1]
	if got := strings.Count(last, "active task list"); got != 1 {
		t.Fatalf("nudge injected %d times in final payload, want 1", got)
	}
}

func TestChatTodoNudgeSkippedWhenTodoToolUsed(t *testing.T) {
	// 11 todo-free rounds, a todo call at round 12 (resetting the counter),
	// then 10 more todo-free rounds: the threshold is never reached.
	rec := &chatRoundRecorder{t: t, rounds: 22, todoAt: 12}
	runRecordedChat(t, rec)
	if got := rec.nudgeMentions(); got != 0 {
		t.Fatalf("todo nudge fired despite todo tool usage (%d mentions)", got)
	}
}
