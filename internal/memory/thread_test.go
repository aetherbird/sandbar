package memory

import (
	"testing"
)

func TestThreadCRUD(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	store, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	title := "Test Thread"
	model := "gemma4"
	thread, err := store.CreateThread(&title, &model)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if thread.ID == "" {
		t.Fatal("thread id is empty")
	}
	if thread.Title == nil || *thread.Title != title {
		t.Fatal("thread title mismatch")
	}

	content := "Hello, world!"
	msg, err := store.AppendMessage(thread.ID, "user", &content, nil)
	if err != nil {
		t.Fatalf("append message: %v", err)
	}
	if msg.Seq != 1 {
		t.Errorf("first message seq: got %d, want 1", msg.Seq)
	}

	content2 := "Hi there!"
	msg2, err := store.AppendMessage(thread.ID, "assistant", &content2, nil)
	if err != nil {
		t.Fatalf("append message 2: %v", err)
	}
	if msg2.Seq != 2 {
		t.Errorf("second message seq: got %d, want 2", msg2.Seq)
	}

	threads, err := store.ListThreads()
	if err != nil {
		t.Fatalf("list threads: %v", err)
	}
	if len(threads) != 1 {
		t.Errorf("thread count: got %d, want 1", len(threads))
	}

	gotThread, messages, err := store.GetThreadWithMessages(thread.ID)
	if err != nil {
		t.Fatalf("get thread with messages: %v", err)
	}
	if gotThread.ID != thread.ID {
		t.Errorf("thread id mismatch")
	}
	if len(messages) != 2 {
		t.Errorf("message count: got %d, want 2", len(messages))
	}
	if messages[0].Role != "user" || messages[1].Role != "assistant" {
		t.Errorf("message roles out of order")
	}
}

func TestThreadRestartSafety(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	store1, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store 1: %v", err)
	}

	thread, err := store1.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	content := "persist me"
	_, err = store1.AppendMessage(thread.ID, "user", &content, nil)
	if err != nil {
		t.Fatalf("append message: %v", err)
	}
	store1.Close()

	store2, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store 2: %v", err)
	}
	defer store2.Close()

	_, messages, err := store2.GetThreadWithMessages(thread.ID)
	if err != nil {
		t.Fatalf("get thread after restart: %v", err)
	}
	if len(messages) != 1 {
		t.Errorf("expected 1 message after restart, got %d", len(messages))
	}
	if messages[0].Content == nil || *messages[0].Content != "persist me" {
		t.Errorf("message content lost after restart")
	}
}

func TestDeleteThread(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	store, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	content := "hello"
	_, err = store.AppendMessage(thread.ID, "user", &content, nil)
	if err != nil {
		t.Fatalf("append message: %v", err)
	}

	if err := store.DeleteThread(thread.ID); err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	threads, err := store.ListThreads()
	if err != nil {
		t.Fatalf("list threads: %v", err)
	}
	if len(threads) != 0 {
		t.Errorf("expected 0 threads, got %d", len(threads))
	}

	_, messages, err := store.GetThreadWithMessages(thread.ID)
	if err == nil {
		t.Fatal("expected error getting deleted thread")
	}
	_ = messages
}

func TestDeleteMessagesAfter(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	store, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	for i, text := range []string{"a", "b", "c", "d"} {
		_, err = store.AppendMessage(thread.ID, []string{"user", "assistant", "user", "assistant"}[i], &text, nil)
		if err != nil {
			t.Fatalf("append message %d: %v", i, err)
		}
	}

	// Delete from seq 3 onward (c and d).
	if err := store.DeleteMessagesAfter(thread.ID, 3); err != nil {
		t.Fatalf("delete messages after: %v", err)
	}

	_, messages, err := store.GetThreadWithMessages(thread.ID)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(messages))
	}
	if messages[0].Content == nil || *messages[0].Content != "a" {
		t.Errorf("first message: got %v, want a", messages[0].Content)
	}
	if messages[1].Content == nil || *messages[1].Content != "b" {
		t.Errorf("second message: got %v, want b", messages[1].Content)
	}
}

func TestDeleteMessagesAfter_CascadesToolCalls(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	store, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	// Seq 1: user message.
	u1 := "user msg"
	msg1, err := store.AppendMessage(thread.ID, "user", &u1, nil)
	if err != nil {
		t.Fatalf("append user msg: %v", err)
	}
	_ = msg1

	// Seq 2: assistant message with tool call.
	a1 := "assistant msg"
	msg2, err := store.AppendMessage(thread.ID, "assistant", &a1, nil)
	if err != nil {
		t.Fatalf("append assistant msg: %v", err)
	}

	// Insert a tool_calls row referencing the assistant message.
	_, err = store.DB().Exec(
		`INSERT INTO tool_calls (id, message_id, tool_name, arguments, seq) VALUES (?, ?, ?, ?, ?)`,
		"tc-001", msg2.ID, "file_read", `{"path":"test.txt"}`, 1,
	)
	if err != nil {
		t.Fatalf("insert tool_call: %v", err)
	}

	// Seq 3: tool result.
	tr := "tool result"
	_, err = store.AppendMessage(thread.ID, "tool", &tr, strPtr("tc-001"))
	if err != nil {
		t.Fatalf("append tool result: %v", err)
	}

	// Seq 4: assistant follow-up.
	a2 := "follow-up"
	_, err = store.AppendMessage(thread.ID, "assistant", &a2, nil)
	if err != nil {
		t.Fatalf("append follow-up: %v", err)
	}

	// Verify initial state.
	_, messages, err := store.GetThreadWithMessages(thread.ID)
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(messages))
	}

	var toolCallCount int
	err = store.DB().QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE message_id = ?`, msg2.ID).Scan(&toolCallCount)
	if err != nil {
		t.Fatalf("count tool_calls: %v", err)
	}
	if toolCallCount != 1 {
		t.Fatalf("expected 1 tool_call, got %d", toolCallCount)
	}

	// Undo from seq 2 onward.
	if err := store.DeleteMessagesAfter(thread.ID, 2); err != nil {
		t.Fatalf("delete messages after: %v", err)
	}

	// Verify only seq 1 remains.
	_, messages, err = store.GetThreadWithMessages(thread.ID)
	if err != nil {
		t.Fatalf("get messages after delete: %v", err)
	}
	if len(messages) != 1 {
		t.Errorf("expected 1 message after undo, got %d", len(messages))
	}
	if messages[0].Content == nil || *messages[0].Content != u1 {
		t.Errorf("remaining message: got %v, want %q", messages[0].Content, u1)
	}

	// Verify tool_calls CASCADE deleted.
	err = store.DB().QueryRow(`SELECT COUNT(*) FROM tool_calls WHERE message_id = ?`, msg2.ID).Scan(&toolCallCount)
	if err != nil {
		t.Fatalf("count tool_calls after delete: %v", err)
	}
	if toolCallCount != 0 {
		t.Errorf("expected 0 tool_calls after CASCADE delete, got %d", toolCallCount)
	}
}

func strPtr(s string) *string { return &s }

func TestUpdateThreadTitleConditional(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"
	store, err := OpenWithMigrations(dbPath, "../../migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	thread, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	if err := store.UpdateThreadTitle(thread.ID, "Auto Title"); err != nil {
		t.Fatalf("update title: %v", err)
	}

	got, err := store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if got.Title == nil || *got.Title != "Auto Title" {
		t.Fatal("title not set")
	}

	// Second update should be ignored because title is no longer NULL.
	if err := store.UpdateThreadTitle(thread.ID, "New Title"); err != nil {
		t.Fatalf("update title 2: %v", err)
	}

	got, err = store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread 2: %v", err)
	}
	if *got.Title != "Auto Title" {
		t.Errorf("title was overwritten: got %q", *got.Title)
	}
}

func TestForkThread(t *testing.T) {
	store, err := OpenWithMigrations(t.TempDir()+"/t.db", "../../migrations")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	title, model := "original", "m1"
	src, err := store.CreateThread(&title, &model)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	u, a := "hello", "hi there"
	store.AppendMessage(src.ID, "user", &u, nil)
	store.AppendMessage(src.ID, "assistant", &a, nil)
	sourceTodos, err := store.CreateTodos(src.ID, []TodoItem{
		{Content: "inspect", Status: TodoCompleted},
		{Content: "implement", Status: TodoInProgress},
	})
	if err != nil {
		t.Fatalf("create source plan: %v", err)
	}

	newID, err := store.ForkThread(src.ID)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if newID == src.ID {
		t.Fatal("fork must create a new thread id")
	}

	nt, msgs, err := store.GetThreadWithMessages(newID)
	if err != nil {
		t.Fatalf("load fork: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || *msgs[0].Content != "hello" {
		t.Errorf("forked messages not copied correctly: %+v", msgs)
	}
	if nt.Title == nil || *nt.Title != "original" {
		t.Errorf("forked title not copied: %v", nt.Title)
	}
	forkPlan, err := store.GetPlan(newID)
	if err != nil {
		t.Fatalf("load forked plan: %v", err)
	}
	if forkPlan == nil || forkPlan.Revision != 1 || len(forkPlan.Items) != len(sourceTodos) {
		t.Fatalf("forked plan = %+v, want source snapshot", forkPlan)
	}
	for i := range sourceTodos {
		if got := forkPlan.Items[i]; got.ID != sourceTodos[i].ID || got.Content != sourceTodos[i].Content || got.Status != sourceTodos[i].Status {
			t.Fatalf("forked todo %d = %+v, want %+v", i, got, sourceTodos[i])
		}
	}
	changed := "fork-only change"
	if _, err := store.UpdateTodos(newID, []TodoUpdate{{ID: forkPlan.Items[0].ID, Content: &changed}}); err != nil {
		t.Fatalf("update forked plan: %v", err)
	}
	sourcePlan, err := store.GetPlan(src.ID)
	if err != nil || sourcePlan.Items[0].Content != "inspect" {
		t.Fatalf("fork mutation affected source plan: %+v, %v", sourcePlan, err)
	}

	// Source thread must be untouched.
	_, srcMsgs, _ := store.GetThreadWithMessages(src.ID)
	if len(srcMsgs) != 2 {
		t.Errorf("source thread was modified by fork: %d messages", len(srcMsgs))
	}
}

func TestForkThreadRemapsToolCallsAndResults(t *testing.T) {
	store, err := OpenWithMigrations(t.TempDir()+"/t.db", "../../migrations")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	title, model := "tool thread", "m1"
	src, err := store.CreateThread(&title, &model)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	userContent := "inspect both files"
	if _, err := store.AppendMessage(src.ID, "user", &userContent, nil); err != nil {
		t.Fatalf("append user message: %v", err)
	}
	assistantContent := ""
	srcAssistant, err := store.AppendAssistantMessageWithToolCalls(src.ID, &assistantContent, []AssistantToolCall{
		{ID: "call_read_a", ToolName: "file_read", Arguments: `{"path":"a.txt"}`},
		{ID: "call_read_b", ToolName: "file_read", Arguments: `{"path":"b.txt"}`},
	})
	if err != nil {
		t.Fatalf("append assistant tool calls: %v", err)
	}
	resultA, resultB := "contents a", "contents b"
	if _, err := store.AppendMessage(src.ID, "tool", &resultA, strPtr("call_read_a")); err != nil {
		t.Fatalf("append first tool result: %v", err)
	}
	if _, err := store.AppendMessage(src.ID, "tool", &resultB, strPtr("call_read_b")); err != nil {
		t.Fatalf("append second tool result: %v", err)
	}
	finalContent := "done"
	if _, err := store.AppendMessage(src.ID, "assistant", &finalContent, nil); err != nil {
		t.Fatalf("append final assistant message: %v", err)
	}

	newID, err := store.ForkThread(src.ID)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	_, forkMessages, err := store.GetThreadWithMessages(newID)
	if err != nil {
		t.Fatalf("load fork: %v", err)
	}
	if len(forkMessages) != 5 {
		t.Fatalf("fork message count: got %d, want 5", len(forkMessages))
	}

	srcCalls, err := store.GetToolCallsForMessage(srcAssistant.ID)
	if err != nil {
		t.Fatalf("load source tool calls: %v", err)
	}
	forkCalls, err := store.GetToolCallsForMessage(forkMessages[1].ID)
	if err != nil {
		t.Fatalf("load fork tool calls: %v", err)
	}
	if len(forkCalls) != len(srcCalls) {
		t.Fatalf("fork tool-call count: got %d, want %d", len(forkCalls), len(srcCalls))
	}
	for i := range srcCalls {
		if forkCalls[i].ID == srcCalls[i].ID {
			t.Errorf("tool call %d reused source id %q", i, srcCalls[i].ID)
		}
		if forkCalls[i].MessageID != forkMessages[1].ID {
			t.Errorf("tool call %d attached to message %d, want %d", i, forkCalls[i].MessageID, forkMessages[1].ID)
		}
		if forkCalls[i].ToolName != srcCalls[i].ToolName ||
			forkCalls[i].Arguments != srcCalls[i].Arguments ||
			forkCalls[i].Seq != srcCalls[i].Seq {
			t.Errorf("tool call %d metadata changed: source=%+v fork=%+v", i, srcCalls[i], forkCalls[i])
		}
		resultMessage := forkMessages[i+2]
		if resultMessage.ToolCallID == nil || *resultMessage.ToolCallID != forkCalls[i].ID {
			t.Errorf("tool result %d references %v, want remapped id %q", i, resultMessage.ToolCallID, forkCalls[i].ID)
		}
	}

	// The copied history must still form complete assistant-call/result groups,
	// suitable for reconstruction into a provider request.
	assertValidStoredToolHistory(t, store, forkMessages)

	_, srcMessages, err := store.GetThreadWithMessages(src.ID)
	if err != nil {
		t.Fatalf("reload source: %v", err)
	}
	if srcMessages[2].ToolCallID == nil || *srcMessages[2].ToolCallID != "call_read_a" ||
		srcMessages[3].ToolCallID == nil || *srcMessages[3].ToolCallID != "call_read_b" {
		t.Errorf("source tool-result references changed: %+v", srcMessages)
	}
}

func TestForkThreadRollsBackOnToolCallCopyFailure(t *testing.T) {
	store, err := OpenWithMigrations(t.TempDir()+"/t.db", "../../migrations")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	src, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	content := ""
	if _, err := store.AppendAssistantMessageWithToolCalls(src.ID, &content, []AssistantToolCall{
		{ID: "call_fail", ToolName: "fail_copy", Arguments: `{}`},
	}); err != nil {
		t.Fatalf("append source tool call: %v", err)
	}
	if _, err := store.DB().Exec(`
		CREATE TRIGGER reject_fork_tool_call
		BEFORE INSERT ON tool_calls
		WHEN NEW.tool_name = 'fail_copy'
		BEGIN
			SELECT RAISE(ABORT, 'copy rejected');
		END;
	`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	if _, err := store.ForkThread(src.ID); err == nil {
		t.Fatal("fork unexpectedly succeeded")
	}
	var threadCount int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&threadCount); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if threadCount != 1 {
		t.Errorf("failed fork left a partial thread: got %d threads, want 1", threadCount)
	}
	var sourceCallCount int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM tool_calls tc JOIN messages m ON m.id = tc.message_id WHERE m.thread_id = ?`,
		src.ID,
	).Scan(&sourceCallCount); err != nil {
		t.Fatalf("count source tool calls: %v", err)
	}
	if sourceCallCount != 1 {
		t.Errorf("failed fork modified source calls: got %d, want 1", sourceCallCount)
	}
}

func assertValidStoredToolHistory(t *testing.T, store *Store, messages []Message) {
	t.Helper()
	seenCallIDs := make(map[string]struct{})
	var pending map[string]bool

	assertPendingClosed := func() {
		t.Helper()
		for id, closed := range pending {
			if !closed {
				t.Errorf("tool call %q has no result", id)
			}
		}
		pending = nil
	}

	for _, message := range messages {
		if message.Role == "tool" {
			if message.ToolCallID == nil {
				t.Errorf("tool message at seq %d has no tool_call_id", message.Seq)
				continue
			}
			closed, ok := pending[*message.ToolCallID]
			if !ok {
				t.Errorf("tool message at seq %d references call %q outside its assistant group", message.Seq, *message.ToolCallID)
				continue
			}
			if closed {
				t.Errorf("tool call %q has duplicate results", *message.ToolCallID)
			}
			pending[*message.ToolCallID] = true
			continue
		}

		if pending != nil {
			assertPendingClosed()
		}
		if message.Role != "assistant" {
			continue
		}
		toolCalls, err := store.GetToolCallsForMessage(message.ID)
		if err != nil {
			t.Fatalf("load tool calls for message %d: %v", message.ID, err)
		}
		if len(toolCalls) == 0 {
			continue
		}
		pending = make(map[string]bool, len(toolCalls))
		for _, toolCall := range toolCalls {
			if _, duplicate := seenCallIDs[toolCall.ID]; duplicate {
				t.Errorf("tool call id %q is reused", toolCall.ID)
			}
			seenCallIDs[toolCall.ID] = struct{}{}
			pending[toolCall.ID] = false
		}
	}
	if pending != nil {
		assertPendingClosed()
	}
}

func TestThreadWorkspace(t *testing.T) {
	store, err := OpenWithMigrations(t.TempDir()+"/t.db", "../../migrations")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	// Workspace is recorded at creation and round-trips through GetThread.
	thread, err := store.CreateThreadWithWorkspace(nil, nil, "/work/dir1")
	if err != nil {
		t.Fatalf("create with workspace: %v", err)
	}
	got, err := store.GetThread(thread.ID)
	if err != nil {
		t.Fatalf("get thread: %v", err)
	}
	if got.Workspace != "/work/dir1" {
		t.Errorf("workspace: got %q, want %q", got.Workspace, "/work/dir1")
	}

	// ListThreads carries the workspace field.
	all, err := store.ListThreads()
	if err != nil {
		t.Fatalf("list threads: %v", err)
	}
	if len(all) != 1 || all[0].Workspace != "/work/dir1" {
		t.Errorf("list threads workspace: got %+v", all)
	}

	// Exact filter matches the recorded workspace only.
	only, err := store.ListThreadsByWorkspace("/work/dir1", false)
	if err != nil {
		t.Fatalf("list by workspace: %v", err)
	}
	if len(only) != 1 || only[0].ID != thread.ID {
		t.Errorf("strict filter: got %+v, want the dir1 thread", only)
	}
	none, err := store.ListThreadsByWorkspace("/work/elsewhere", false)
	if err != nil {
		t.Fatalf("list by workspace: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("strict filter elsewhere: got %d threads, want 0", len(none))
	}

	// Legacy threads (unknown workspace, created before migration 0005) are
	// excluded by the strict filter but surface with includeLegacy.
	legacy, err := store.CreateThread(nil, nil)
	if err != nil {
		t.Fatalf("create legacy thread: %v", err)
	}
	strict, err := store.ListThreadsByWorkspace("/work/dir1", false)
	if err != nil {
		t.Fatalf("list strict: %v", err)
	}
	for _, t2 := range strict {
		if t2.ID == legacy.ID {
			t.Error("legacy thread leaked into strict workspace filter")
		}
	}
	withLegacy, err := store.ListThreadsByWorkspace("/work/dir1", true)
	if err != nil {
		t.Fatalf("list with legacy: %v", err)
	}
	if len(withLegacy) != 2 {
		t.Errorf("filter with legacy: got %d threads, want 2", len(withLegacy))
	}

	// A fork inherits its source's workspace.
	forkID, err := store.ForkThread(thread.ID)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	forked, err := store.GetThread(forkID)
	if err != nil {
		t.Fatalf("get fork: %v", err)
	}
	if forked.Workspace != "/work/dir1" {
		t.Errorf("fork workspace: got %q, want %q", forked.Workspace, "/work/dir1")
	}
}
