package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"sandbar/internal/backend"
	"sandbar/internal/config"
	"sandbar/internal/llm"
	"sandbar/internal/memory"
)

type fakeCLIBackend struct {
	mu sync.Mutex

	threads      []backend.ThreadSummary
	details      map[string]*backend.ThreadDetail
	models       []string
	defaultModel string
	workspace    string
	events       []llm.StreamEvent
	sendErr      error
	getThreadErr error

	lastThread  string
	lastModel   string
	lastMessage string
	renamedID   string
	renamedTo   string
	undoID      string
	undoSeq     int
}

func (b *fakeCLIBackend) ListThreads() ([]backend.ThreadSummary, error) {
	return append([]backend.ThreadSummary(nil), b.threads...), nil
}

func (b *fakeCLIBackend) GetThread(id string) (*backend.ThreadDetail, error) {
	if b.getThreadErr != nil {
		return nil, b.getThreadErr
	}
	if detail := b.details[id]; detail != nil {
		copy := *detail
		copy.Messages = append([]backend.Message(nil), detail.Messages...)
		return &copy, nil
	}
	return &backend.ThreadDetail{ThreadSummary: backend.ThreadSummary{ID: id}}, nil
}

type openStreamCLIBackend struct {
	*fakeCLIBackend
	stream <-chan llm.StreamEvent
	ctx    context.Context
}

func (b *openStreamCLIBackend) SendMessage(ctx context.Context, threadID, model, message, effort string, plan bool) (<-chan llm.StreamEvent, error) {
	b.mu.Lock()
	b.lastThread, b.lastModel, b.lastMessage = threadID, model, message
	b.ctx = ctx
	b.mu.Unlock()
	return b.stream, nil
}

func (b *fakeCLIBackend) CreateThread(model string) (*backend.ThreadSummary, error) {
	return &backend.ThreadSummary{ID: "created", Model: model, Workspace: b.workspace}, nil
}

func (b *fakeCLIBackend) DeleteThread(string) error { return nil }

func (b *fakeCLIBackend) RenameThread(id, title string) error {
	b.renamedID, b.renamedTo = id, title
	return nil
}

func (b *fakeCLIBackend) UndoThread(id string, seq int) error {
	b.undoID, b.undoSeq = id, seq
	return nil
}

func (b *fakeCLIBackend) SendMessage(_ context.Context, threadID, model, message, effort string, plan bool) (<-chan llm.StreamEvent, error) {
	b.mu.Lock()
	b.lastThread, b.lastModel, b.lastMessage = threadID, model, message
	events := append([]llm.StreamEvent(nil), b.events...)
	err := b.sendErr
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	ch := make(chan llm.StreamEvent, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch, nil
}

func (b *fakeCLIBackend) ListModels() []string { return append([]string(nil), b.models...) }
func (b *fakeCLIBackend) DefaultModel() string { return b.defaultModel }
func (b *fakeCLIBackend) Workspace() string    { return b.workspace }
func (b *fakeCLIBackend) GetContextInfo(string) (int, int, error) {
	return 100, 1000, nil
}

func TestOpenLocalRuntimeControlsSubagentSchemas(t *testing.T) {
	for _, test := range []struct {
		name     string
		disabled bool
		want     bool
	}{
		{name: "default registry", want: true},
		{name: "subagents disabled", disabled: true, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var toolNames []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				var body struct {
					Tools []struct {
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"tools"`
				}
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Errorf("decode provider request: %v", err)
				}
				for _, schema := range body.Tools {
					toolNames = append(toolNames, schema.Function.Name)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()

			supportsTools := true
			workspace := t.TempDir()
			cfg := &config.Config{
				Workspace: workspace,
				Persona: config.PersonaConfig{
					Name:         "Sandbar",
					SystemPrompt: "You are a test assistant.",
					TitleModel:   "missing-title-model",
				},
				Providers: []config.ProviderConfig{{
					Name:    "test-provider",
					BaseURL: server.URL,
					APIKey:  "fake",
					Models: map[string]config.ModelConfig{
						"test-model": {SupportsTools: &supportsTools},
					},
				}},
			}
			runtime, err := openLocalRuntime(cfg, filepath.Join(t.TempDir(), "runtime.db"), workspace, localRuntimeOptions{
				DisableSubagents: test.disabled,
			})
			if err != nil {
				t.Fatalf("open local runtime: %v", err)
			}
			defer runtime.close()

			events, err := runtime.backend.SendMessage(context.Background(), "", "test-model", "hello", "", false)
			if err != nil {
				t.Fatalf("send message: %v", err)
			}
			done := false
			for event := range events {
				if event.Type == "error" {
					t.Fatalf("local runtime stream error: %s", event.Content)
				}
				done = done || event.Type == "done"
			}
			if !done {
				t.Fatal("local runtime stream ended without done event")
			}

			hasDelegate, hasResume := false, false
			for _, name := range toolNames {
				hasDelegate = hasDelegate || name == "delegate_task"
				hasResume = hasResume || name == "resume_task"
			}
			if hasDelegate != test.want || hasResume != test.want {
				t.Fatalf("subagent schemas delegate=%v resume=%v, want both %v; schemas=%v", hasDelegate, hasResume, test.want, toolNames)
			}
		})
	}
}

func TestSlashRegistryOwnsAliasesHelpAndAvailability(t *testing.T) {
	aliases := map[string]string{
		"/branch":  "/fork",
		"/compact": "/compress",
		"/?":       "/help",
		"/q":       "/quit",
		"/exit":    "/quit",
	}
	seen := make(map[string]string)
	for i := range slashCommands {
		command := &slashCommands[i]
		if command.name == "" || command.desc == "" || command.run == nil {
			t.Fatalf("incomplete slash command: %+v", command)
		}
		for _, spelling := range append([]string{command.name}, command.aliases...) {
			if previous, ok := seen[spelling]; ok {
				t.Fatalf("duplicate spelling %q belongs to %s and %s", spelling, previous, command.name)
			}
			seen[spelling] = command.name
			resolved, ok := findSlashCommand(spelling)
			if !ok || resolved != command {
				t.Fatalf("findSlashCommand(%q) = (%v, %v), want %s", spelling, resolved, ok, command.name)
			}
		}
	}
	for alias, canonical := range aliases {
		if got := seen[alias]; got != canonical {
			t.Errorf("alias %s resolves to %q, want %s", alias, got, canonical)
		}
	}

	// A session without local services (e.g. a hand-built test model) must not
	// advertise the service-specific commands.
	bare := appModel{sess: &session{backend: &fakeCLIBackend{}}}
	bareHelp := stripANSI(bare.helpText())
	for _, command := range []string{"/fork", "/compress", "/search"} {
		resolved, ok := findSlashCommand(command)
		if !ok || resolved.available(&bare) {
			t.Errorf("%s must be unavailable without local services", command)
		}
		if strings.Contains(bareHelp, command) {
			t.Errorf("help without local services unexpectedly contains %s:\n%s", command, bareHelp)
		}
	}
	if !strings.Contains(bareHelp, "/sessions") || !strings.Contains(bareHelp, "/undo") {
		t.Fatalf("help without local services omitted backend commands:\n%s", bareHelp)
	}

	local := appModel{sess: &session{
		backend: &fakeCLIBackend{},
		local:   &localServices{store: new(memory.Store), ag: nil},
	}}
	fork, _ := findSlashCommand("/fork")
	search, _ := findSlashCommand("/search")
	compress, _ := findSlashCommand("/compress")
	if !fork.available(&local) || !search.available(&local) || compress.available(&local) {
		t.Fatalf("service-specific availability is wrong: fork=%v search=%v compress=%v", fork.available(&local), search.available(&local), compress.available(&local))
	}
}

func TestPrintWorkspaceWarningValidatesResume(t *testing.T) {
	t.Run("matching workspace", func(t *testing.T) {
		be := &fakeCLIBackend{details: map[string]*backend.ThreadDetail{
			"thread": {ThreadSummary: backend.ThreadSummary{ID: "thread", Workspace: "/current"}},
		}}
		var output bytes.Buffer
		if err := printWorkspaceWarning(be, "thread", "/current", &output); err != nil {
			t.Fatalf("printWorkspaceWarning: %v", err)
		}
		if output.Len() != 0 {
			t.Fatalf("unexpected warning: %q", output.String())
		}
	})

	t.Run("mismatch warns", func(t *testing.T) {
		be := &fakeCLIBackend{details: map[string]*backend.ThreadDetail{
			"thread": {ThreadSummary: backend.ThreadSummary{ID: "thread", Workspace: "/original"}},
		}}
		var output bytes.Buffer
		if err := printWorkspaceWarning(be, "thread", "/current", &output); err != nil {
			t.Fatalf("printWorkspaceWarning: %v", err)
		}
		if got := stripANSI(output.String()); !strings.Contains(got, "thread workspace: /original") || !strings.Contains(got, "tools will run in /current") {
			t.Fatalf("warning = %q", got)
		}
	})

	t.Run("lookup error surfaces", func(t *testing.T) {
		lookupErr := errors.New("HTTP 404: thread not found")
		be := &fakeCLIBackend{getThreadErr: lookupErr}
		if err := printWorkspaceWarning(be, "missing", "/current", &bytes.Buffer{}); !errors.Is(err, lookupErr) {
			t.Fatalf("error = %v, want wrapped lookup error", err)
		}
	})
}

func TestChooseInitialModelUsesBackendOnlyWhenNeeded(t *testing.T) {
	be := &fakeCLIBackend{models: []string{"z/model", "a/model"}}
	if got, err := chooseInitialModel(context.Background(), be, " explicit/model "); err != nil || got != "explicit/model" {
		t.Fatalf("explicit = %q, %v", got, err)
	}
	be.defaultModel = "default/model"
	if got, err := chooseInitialModel(context.Background(), be, ""); err != nil || got != "default/model" {
		t.Fatalf("default = %q, %v", got, err)
	}
	be.defaultModel = ""
	if got, err := chooseInitialModel(context.Background(), be, ""); err != nil || got != "a/model" {
		t.Fatalf("sorted fallback = %q, %v", got, err)
	}
	if !reflect.DeepEqual(be.models, []string{"z/model", "a/model"}) {
		t.Fatalf("model selection mutated backend slice: %v", be.models)
	}
}

func TestRunOneShotUsesBackendStream(t *testing.T) {
	be := &fakeCLIBackend{events: []llm.StreamEvent{
		{Type: "token", Content: "remote response"},
		{Type: "done", ThreadID: "remote-thread"},
	}}
	var stdout, stderr bytes.Buffer
	if err := runOneShot(be, nil, "remote/model", "resume-id", "question", "", false, false, strings.NewReader("stdin payload"), true, &stdout, &stderr); err != nil {
		t.Fatalf("runOneShot: %v", err)
	}
	if got := stdout.String(); got != "remote response\n" {
		t.Fatalf("stdout = %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if be.lastThread != "resume-id" || be.lastModel != "remote/model" {
		t.Fatalf("request thread/model = %q/%q", be.lastThread, be.lastModel)
	}
	if want := "question\n\n```\nstdin payload\n```"; be.lastMessage != want {
		t.Fatalf("message = %q, want %q", be.lastMessage, want)
	}
}

func TestRunOneShotJSONPreservesCanonicalEvents(t *testing.T) {
	want := []llm.StreamEvent{
		{Type: "token", Content: "hello"},
		{Type: "done", ThreadID: "thread-1"},
	}
	be := &fakeCLIBackend{events: want}
	var output bytes.Buffer
	if err := runOneShot(be, nil, "model", "", "message", "", false, true, strings.NewReader(""), false, &output, &bytes.Buffer{}); err != nil {
		t.Fatalf("runOneShot: %v", err)
	}
	dec := json.NewDecoder(&output)
	for i := range want {
		var got llm.StreamEvent
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("decode event %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, want[i]) {
			t.Fatalf("event %d = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestBackendThreadMutationsDoNotUseLocalStore(t *testing.T) {
	be := &fakeCLIBackend{details: map[string]*backend.ThreadDetail{
		"thread": {
			ThreadSummary: backend.ThreadSummary{ID: "thread"},
			Messages: []backend.Message{
				{Role: "user", Content: "first", Seq: 2},
				{Role: "assistant", Content: "answer", Seq: 3},
				{Role: "user", Content: "second", Seq: 7},
			},
		},
	}}
	m := appModel{sess: &session{backend: be, threadID: "thread"}}
	_ = m.setTitle("remote title")
	if be.renamedID != "thread" || be.renamedTo != "remote title" {
		t.Fatalf("rename request = %q %q", be.renamedID, be.renamedTo)
	}
	_ = m.undoLast()
	if be.undoID != "thread" || be.undoSeq != 7 {
		t.Fatalf("undo request = %q seq %d", be.undoID, be.undoSeq)
	}
}

func TestRunOneShotReturnsStreamError(t *testing.T) {
	be := &fakeCLIBackend{events: []llm.StreamEvent{{Type: "error", Content: "remote failed"}}}
	err := runOneShot(be, nil, "model", "", "message", "", false, false, strings.NewReader(""), false, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || err.Error() != "remote failed" {
		t.Fatalf("error = %v, want remote failed", err)
	}
}

func TestStreamsRequireCanonicalDone(t *testing.T) {
	t.Run("one-shot rejects unexpected EOF", func(t *testing.T) {
		be := &fakeCLIBackend{events: []llm.StreamEvent{{Type: "token", Content: "partial"}}}
		var output bytes.Buffer
		err := runOneShot(be, nil, "model", "", "message", "", false, false, strings.NewReader(""), false, &output, &bytes.Buffer{})
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("error = %v, want io.ErrUnexpectedEOF", err)
		}
		if output.String() != "partial\n" {
			t.Fatalf("partial output = %q", output.String())
		}
	})

	t.Run("TUI rejects unexpected EOF", func(t *testing.T) {
		be := &fakeCLIBackend{events: []llm.StreamEvent{{Type: "token", Content: "partial"}}}
		m := appModel{sess: &session{backend: be, modelAlias: "model"}, width: 80}
		ch := make(chan streamItem, 8)
		m.launchStreamGoroutine("message", ch)
		var terminal streamItem
		for i := 0; i < 3; i++ { // label, token, terminal error
			select {
			case terminal = <-ch:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for TUI stream")
			}
		}
		if terminal.kind != "err" || !errors.Is(terminal.err, io.ErrUnexpectedEOF) {
			t.Fatalf("terminal item = %+v, want unexpected-EOF error", terminal)
		}
	})
}

func TestStreamsTerminateOnTerminalEventWithoutWaitingForClose(t *testing.T) {
	t.Run("one-shot done", func(t *testing.T) {
		events := make(chan llm.StreamEvent, 1)
		be := &openStreamCLIBackend{fakeCLIBackend: &fakeCLIBackend{}, stream: events}
		result := make(chan error, 1)
		go func() {
			result <- runOneShot(be, nil, "model", "", "message", "", false, false, strings.NewReader(""), false, &bytes.Buffer{}, &bytes.Buffer{})
		}()
		events <- llm.StreamEvent{Type: "done", ThreadID: "thread"}
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("runOneShot: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("runOneShot waited for the event channel to close after done")
		}
	})

	t.Run("one-shot error", func(t *testing.T) {
		events := make(chan llm.StreamEvent, 1)
		be := &openStreamCLIBackend{fakeCLIBackend: &fakeCLIBackend{}, stream: events}
		result := make(chan error, 1)
		go func() {
			result <- runOneShot(be, nil, "model", "", "message", "", false, false, strings.NewReader(""), false, &bytes.Buffer{}, &bytes.Buffer{})
		}()
		events <- llm.StreamEvent{Type: "error", Content: "provider failed"}
		select {
		case err := <-result:
			if err == nil || err.Error() != "provider failed" {
				t.Fatalf("runOneShot error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("runOneShot waited for the event channel to close after error")
		}
	})

	t.Run("TUI done", func(t *testing.T) {
		events := make(chan llm.StreamEvent, 1)
		be := &openStreamCLIBackend{fakeCLIBackend: &fakeCLIBackend{}, stream: events}
		m := appModel{sess: &session{backend: be, modelAlias: "model"}, width: 80}
		items := make(chan streamItem, 8)
		m.launchStreamGoroutine("message", items)
		events <- llm.StreamEvent{Type: "done", ThreadID: "thread"}
		for _, want := range []string{"threadID", "done"} {
			select {
			case item := <-items:
				if item.kind != want {
					t.Fatalf("terminal sequence item = %q, want %q", item.kind, want)
				}
			case <-time.After(time.Second):
				t.Fatalf("TUI waited for the event channel to close after done; want %s", want)
			}
		}
	})

	t.Run("TUI error", func(t *testing.T) {
		events := make(chan llm.StreamEvent, 2)
		be := &openStreamCLIBackend{fakeCLIBackend: &fakeCLIBackend{}, stream: events}
		m := appModel{sess: &session{backend: be, modelAlias: "model"}, width: 80}
		items := make(chan streamItem, 8)
		m.launchStreamGoroutine("message", items)
		events <- llm.StreamEvent{Type: "token", Content: "partial"}
		events <- llm.StreamEvent{Type: "error", Content: "provider failed"}
		var terminal streamItem
		for i := 0; i < 3; i++ { // label, token, terminal error
			select {
			case terminal = <-items:
			case <-time.After(time.Second):
				t.Fatal("TUI waited for the event channel to close after error")
			}
		}
		if terminal.kind != "err" || terminal.err == nil || terminal.err.Error() != "provider failed" {
			t.Fatalf("terminal item = %+v", terminal)
		}
	})
}

func TestTUIStreamErrorPrintsPartialResponseBeforeError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newModel(&session{modelAlias: "model"})
	m.streamGen = 4
	m.streamCh = make(chan streamItem)
	m.streaming = true
	m.turnStart = time.Now()

	updated, _ := m.Update(streamItem{gen: 4, kind: "token", content: "partial answer"})
	m = updated.(appModel)
	updated, cmd := m.Update(streamItem{gen: 4, kind: "err", err: errors.New("provider failed")})
	m = updated.(appModel)
	if cmd == nil {
		t.Fatal("terminal error produced no output command")
	}
	output := stripANSI(executeTeaCommandText(t, cmd))
	partialAt := strings.Index(output, "partial answer")
	errorAt := strings.Index(output, "provider failed")
	if partialAt < 0 || errorAt < 0 || partialAt >= errorAt {
		t.Fatalf("partial response must print before error; output=%q", output)
	}
	if m.streaming {
		t.Fatal("model remained in streaming state after error")
	}
}

// executeTeaCommandText evaluates the command containers Bubble Tea uses for
// Batch and Sequence so presentation ordering can be asserted without running
// an interactive terminal program.
func executeTeaCommandText(t *testing.T, command tea.Cmd) string {
	t.Helper()
	var output strings.Builder
	var execute func(tea.Cmd)
	execute = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, nested := range batch {
				execute(nested)
			}
			return
		}
		value := reflect.ValueOf(msg)
		if value.IsValid() && value.Kind() == reflect.Slice {
			for i := 0; i < value.Len(); i++ {
				nested, ok := value.Index(i).Interface().(tea.Cmd)
				if !ok {
					t.Fatalf("unexpected Bubble Tea command container element %T", value.Index(i).Interface())
				}
				execute(nested)
			}
			return
		}
		fmt.Fprint(&output, msg)
	}
	execute(command)
	return output.String()
}

func TestResumeFailureKeepsActiveSession(t *testing.T) {
	be := &fakeCLIBackend{getThreadErr: errors.New("thread not found")}
	m := appModel{
		sess:    &session{backend: be, threadID: "active-thread"},
		ctxUsed: 42,
		ctxMax:  100,
		draft:   "keep this draft",
	}
	if cmd := m.resumeSession("missing-thread"); cmd == nil {
		t.Fatal("failed resume returned no error output")
	}
	if m.sess.threadID != "active-thread" || m.ctxUsed != 42 || m.ctxMax != 100 || m.draft != "keep this draft" {
		t.Fatalf("failed resume mutated state: thread=%q ctx=%d/%d draft=%q", m.sess.threadID, m.ctxUsed, m.ctxMax, m.draft)
	}
}

func TestSessionPickerTreatsEmptyWorkspaceAsUnknown(t *testing.T) {
	be := &fakeCLIBackend{threads: []backend.ThreadSummary{{ID: "legacy", Title: "old thread"}}}
	m := appModel{sess: &session{backend: be, workspace: ""}}
	if cmd := m.openSessionPicker(); cmd != nil {
		t.Fatalf("openSessionPicker returned a command: %v", cmd)
	}
	if len(m.pickItems) != 1 || m.pickItems[0].tag != "?" {
		t.Fatalf("legacy thread picker item = %+v, want unknown workspace tag", m.pickItems)
	}
}

func TestParseToolAllowlist(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{name: "unset", in: "", want: nil},
		{name: "unset whitespace", in: "   ", want: nil},
		{name: "single", in: "file_read", want: []string{"file_read"}},
		{name: "comma list", in: "file_read, shell_exec,search_files", want: []string{"file_read", "shell_exec", "search_files"}},
		{name: "duplicates collapse", in: "file_read,file_read", want: []string{"file_read"}},
		{name: "empty entry rejected", in: "file_read,,shell_exec", wantErr: true},
		{name: "trailing comma rejected", in: "file_read,", wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseToolAllowlist(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: expected error for %q, got %v", tc.name, tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if tc.want == nil && got != nil {
			t.Errorf("%s: expected nil allowlist, got %v", tc.name, got)
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: expected %v, got %v", tc.name, tc.want, got)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: expected %v, got %v", tc.name, tc.want, got)
				break
			}
		}
	}
}
