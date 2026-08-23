package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/tools"
)

// The stdio tests re-execute this test binary as an MCP server. TestMain is
// the fork point: when SANDBAR_MCP_TEST_SERVER is set we are the child.
func TestMain(m *testing.M) {
	if os.Getenv("SANDBAR_MCP_TEST_SERVER") != "" {
		if os.Getenv("AGENT") != "1" {
			fmt.Fprintln(os.Stderr, "helper: AGENT=1 not exported")
			os.Exit(2)
		}
		if os.Getenv("SANDBAR_MCP_TEST_SLOW") != "" {
			time.Sleep(time.Second) // hold initialize open past the timeout budget
		}
		srv := newTestServer()
		_ = srv.Run(context.Background(), &mcpsdk.StdioTransport{})
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// newTestServer builds an in-process SDK server with echo/add/fail/mixed
// tools over the raw (unvalidated) schema form.
func newTestServer() *mcpsdk.Server {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "testsrv", Version: "v0.0.1"}, nil)
	srv.AddTool(&mcpsdk.Tool{
		Name:        "echo",
		Description: "Echoes text back.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var args struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: args.Text}}}, nil
	})
	srv.AddTool(&mcpsdk.Tool{
		Name:        "add",
		Description: "Adds two numbers.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}`),
	}, func(_ context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		var args struct {
			A, B float64
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: fmt.Sprintf("%v", args.A+args.B)}}}, nil
	})
	srv.AddTool(&mcpsdk.Tool{
		Name:        "fail",
		Description: "Always reports a tool-level error.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			IsError: true,
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "denominator is zero"}},
		}, nil
	})
	srv.AddTool(&mcpsdk.Tool{
		Name:        "mixed",
		Description: "Returns text plus an image.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: "chart follows"},
			&mcpsdk.ImageContent{Data: []byte("aGVsbG8="), MIMEType: "image/png"},
		}}, nil
	})
	return srv
}

// newMemConn connects a client to an in-process server over the SDK's
// in-memory transport, bypassing process spawn for the fast tests.
func newMemConn(t *testing.T) *Conn {
	t.Helper()
	ct, st := mcpsdk.NewInMemoryTransports()
	if _, err := newTestServer().Connect(context.Background(), st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	conn, err := connectTransport(context.Background(), "mem", ct)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestConnToolsListsServerTools(t *testing.T) {
	conn := newMemConn(t)
	got, err := conn.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	want := map[string]string{
		"echo":  "Echoes text back.",
		"add":   "Adds two numbers.",
		"fail":  "Always reports a tool-level error.",
		"mixed": "Returns text plus an image.",
	}
	if len(got) != len(want) {
		t.Fatalf("Tools() = %d listings, want %d: %+v", len(got), len(want), got)
	}
	for _, l := range got {
		if l.Description != want[l.Name] {
			t.Errorf("tool %q description = %q, want %q", l.Name, l.Description, want[l.Name])
		}
		if l.Schema == nil {
			t.Errorf("tool %q has nil schema", l.Name)
		}
	}
}

func TestConnCallRoundTrips(t *testing.T) {
	conn := newMemConn(t)
	cases := []struct {
		tool string
		args string
		want string
	}{
		{"echo", `{"text":"hello world"}`, "hello world"},
		{"echo", `{"text":""}`, ""},
		{"add", `{"a":2,"b":3}`, "5"},
		{"add", `{"a":2.5,"b":0.25}`, "2.75"},
		{"mixed", `{}`, "chart follows\n[image part]"},
	}
	for _, tc := range cases {
		got, err := conn.Call(context.Background(), tc.tool, json.RawMessage(tc.args))
		if err != nil {
			t.Errorf("Call(%s, %s): %v", tc.tool, tc.args, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Call(%s, %s) = %q, want %q", tc.tool, tc.args, got, tc.want)
		}
	}
}

func TestConnCallToolErrorKeepsText(t *testing.T) {
	conn := newMemConn(t)
	text, err := conn.Call(context.Background(), "fail", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("Call(fail) error = nil, want tool-level error")
	}
	if !strings.Contains(err.Error(), "fail") || !strings.Contains(err.Error(), "denominator is zero") {
		t.Errorf("error %q should name the tool and carry the server text", err)
	}
	if text != "denominator is zero" {
		t.Errorf("text = %q, want server content", text)
	}
}

func TestRenderContent(t *testing.T) {
	cases := []struct {
		name  string
		parts []mcpsdk.Content
		want  string
	}{
		{"nil", nil, ""},
		{"one text", []mcpsdk.Content{&mcpsdk.TextContent{Text: "hi"}}, "hi"},
		{
			"two texts join with newline",
			[]mcpsdk.Content{&mcpsdk.TextContent{Text: "a"}, &mcpsdk.TextContent{Text: "b"}},
			"a\nb",
		},
		{
			"non-text parts",
			[]mcpsdk.Content{
				&mcpsdk.TextContent{Text: "t"},
				&mcpsdk.ImageContent{MIMEType: "image/png"},
				&mcpsdk.AudioContent{MIMEType: "audio/wav"},
				&mcpsdk.ResourceLink{URI: "file:///x"},
				&mcpsdk.EmbeddedResource{},
			},
			"t\n[image part]\n[audio part]\n[resource_link part]\n[resource part]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderContent(tc.parts); got != tc.want {
				t.Errorf("renderContent = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSanitizeName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"has-dash", "has_dash"},
		{"has.dot", "has_dot"},
		{"sp ace", "sp_ace"},
		{"uni✓code", "uni_code"},
		{"", ""},
		{"already_fine123", "already_fine123"},
	}
	for _, tc := range cases {
		if got := sanitizeName(tc.in); got != tc.want {
			t.Errorf("sanitizeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWrapTools(t *testing.T) {
	conn := newMemConn(t)
	ts, err := WrapTools(context.Background(), conn, "calcy server")
	if err != nil {
		t.Fatalf("WrapTools: %v", err)
	}
	if len(ts) != 4 {
		t.Fatalf("WrapTools returned %d tools, want 4", len(ts))
	}
	byName := map[string]tools.Tool{}
	for _, tool := range ts {
		byName[tool.Name] = tool
	}
	for _, want := range []string{"mcp_calcy_server_echo", "mcp_calcy_server_add", "mcp_calcy_server_fail", "mcp_calcy_server_mixed"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("missing wrapped tool %q; got %v", want, keysOf(byName))
		}
	}
	echo := byName["mcp_calcy_server_echo"]
	if echo.Description != "Echoes text back." {
		t.Errorf("description = %q", echo.Description)
	}
	if echo.Metadata.Tier != tools.TierWrite {
		t.Errorf("tier = %q, want %q (unknown blast radius)", echo.Metadata.Tier, tools.TierWrite)
	}
	schema := echo.Schema
	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want object", schema["type"])
	}
	props, _ := schema["properties"].(map[string]interface{})
	if _, ok := props["text"]; !ok {
		t.Errorf("schema properties missing text: %v", schema)
	}
	if echo.Execute == nil {
		t.Errorf("wrapped tool has no executor")
	}
}

func TestWrapToolsExecuteRoundTrip(t *testing.T) {
	conn := newMemConn(t)
	ts, err := WrapTools(context.Background(), conn, "mem")
	if err != nil {
		t.Fatalf("WrapTools: %v", err)
	}
	var echo, fail tools.Tool
	for _, tool := range ts {
		switch tool.Name {
		case "mcp_mem_echo":
			echo = tool
		case "mcp_mem_fail":
			fail = tool
		}
	}
	if echo.Execute == nil || fail.Execute == nil {
		t.Fatalf("wrapped tools missing echo/fail; got %+v", ts)
	}
	got, err := echo.Execute(context.Background(), map[string]interface{}{"text": "wrapped!"})
	if err != nil {
		t.Fatalf("echo execute: %v", err)
	}
	if got != "wrapped!" {
		t.Errorf("echo content = %q, want %q", got, "wrapped!")
	}
	if _, err := fail.Execute(context.Background(), map[string]interface{}{}); err == nil {
		t.Error("fail execute error = nil, want tool-level error surfaced")
	}
}

// TestWrappedToolRunsThroughRegistry verifies the registry choke point
// consults MCP tier metadata like any built-in: write tier in write mode
// runs without prompting, and an exact-name rule can re-gate it.
func TestWrappedToolRunsThroughRegistry(t *testing.T) {
	conn := newMemConn(t)
	registry := tools.NewRegistry("", "", "", nil)
	warnings := RegisterAll(context.Background(), registry, []*Conn{conn})
	if len(warnings) != 0 {
		t.Fatalf("RegisterAll warnings = %v, want none", warnings)
	}
	tool, err := registry.Get("mcp_mem_echo")
	if err != nil {
		t.Fatalf("registry missing mcp_mem_echo: %v (has %v)", err, registry.List())
	}
	if tool.Metadata.Tier != tools.TierWrite {
		t.Fatalf("tier = %q, want write", tool.Metadata.Tier)
	}
	if _, err := registry.Execute(context.Background(), "mcp_mem_echo", map[string]interface{}{"text": "via registry"}); err != nil {
		t.Fatalf("Execute in yolo mode: %v", err)
	}
	// An exact-name rule override must reach the MCP tool like built-ins.
	if err := registry.SetApprovalConfig(tools.ApprovalConfig{
		Mode:         tools.ApprovalModeYolo,
		ToolPolicies: map[string]tools.ApprovalPolicy{"mcp_mem_echo": tools.PolicyDeny},
	}); err != nil {
		t.Fatalf("SetApprovalConfig: %v", err)
	}
	if _, err := registry.Execute(context.Background(), "mcp_mem_echo", map[string]interface{}{"text": "denied"}); err == nil {
		t.Error("Execute under deny rule = nil error, want denial")
	}
}

func TestRegisterAll(t *testing.T) {
	conn := newMemConn(t)
	registry := tools.NewRegistry("", "", "", nil)
	warnings := RegisterAll(context.Background(), registry, []*Conn{conn})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	want := []string{"mcp_mem_add", "mcp_mem_echo", "mcp_mem_fail", "mcp_mem_mixed"}
	for _, w := range want {
		tool, err := registry.Get(w)
		if err != nil {
			t.Fatalf("registry missing %q (has %v)", w, registry.List())
		}
		if tool.Metadata.Tier != tools.TierWrite {
			t.Errorf("%s tier = %q, want write", w, tool.Metadata.Tier)
		}
	}
	if got := len(registry.List()) - len(tools.NewRegistry("", "", "", nil).List()); got != len(want) {
		t.Errorf("registry gained %d tools over the built-in set, want %d", got, len(want))
	}
}

// connectLocal spawns the test binary as a stdio MCP server via Connect.
func connectLocal(t *testing.T, name string, timeoutMS int) *Conn {
	t.Helper()
	cfg := config.MCPServerConfig{
		Type:      "local",
		Command:   []string{os.Args[0]},
		TimeoutMS: timeoutMS,
	}
	t.Setenv("SANDBAR_MCP_TEST_SERVER", "1")
	conn, err := Connect(context.Background(), name, cfg)
	if err != nil {
		t.Fatalf("Connect(local): %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestConnectLocalStdioEndToEnd(t *testing.T) {
	if os.Getenv("SANDBAR_MCP_TEST_SERVER") != "" {
		t.Skip("child process")
	}
	conn := connectLocal(t, "helper", 0)

	listings, err := conn.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(listings) != 4 {
		t.Fatalf("Tools() = %+v, want 4 tools", listings)
	}
	got, err := conn.Call(context.Background(), "add", json.RawMessage(`{"a":20,"b":22}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got != "42" {
		t.Errorf("Call(add) = %q, want 42", got)
	}

	registry := tools.NewRegistry("", "", "", nil)
	_ = RegisterAll(context.Background(), registry, []*Conn{conn})
	tool, err := registry.Get("mcp_helper_echo")
	if err != nil {
		t.Fatalf("registry missing mcp_helper_echo: %v", registry.List())
	}
	out, err := tool.Execute(context.Background(), map[string]interface{}{"text": "over stdio"})
	if err != nil || out != "over stdio" {
		t.Errorf("execute = (%q, %v), want plain echo", out, err)
	}
}

func TestConnectErrors(t *testing.T) {
	cases := []struct {
		name       string
		cfg        config.MCPServerConfig
		wantSubstr []string
	}{
		{
			name:       "unknown type",
			cfg:        config.MCPServerConfig{Type: "carrier-pigeon"},
			wantSubstr: []string{`unknown type "carrier-pigeon"`},
		},
		{
			name:       "local without command",
			cfg:        config.MCPServerConfig{Type: "local"},
			wantSubstr: []string{"local server needs a command"},
		},
		{
			name:       "remote without url",
			cfg:        config.MCPServerConfig{Type: "remote"},
			wantSubstr: []string{"remote server needs a url"},
		},
		{
			name: "spawn failure includes argv",
			cfg:  config.MCPServerConfig{Type: "local", Command: []string{"sandbar-no-such-binary-xyz", "--flag"}},
			wantSubstr: []string{
				`[sandbar-no-such-binary-xyz --flag]`,
				"sandbar-no-such-binary-xyz",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Connect(context.Background(), "culprit", tc.cfg)
			if err == nil {
				t.Fatal("Connect error = nil, want failure")
			}
			for _, sub := range tc.wantSubstr {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q missing %q", err, sub)
				}
			}
		})
	}
}

func TestConnectInitializeTimeout(t *testing.T) {
	if os.Getenv("SANDBAR_MCP_TEST_SERVER") != "" {
		t.Skip("child process")
	}
	t.Setenv("SANDBAR_MCP_TEST_SERVER", "1")
	t.Setenv("SANDBAR_MCP_TEST_SLOW", "1")
	cfg := config.MCPServerConfig{
		Type:      "local",
		Command:   []string{os.Args[0]},
		TimeoutMS: 250, // ms; the helper sleeps 1s before answering initialize
	}
	_, err := Connect(context.Background(), "slowpoke", cfg)
	if err == nil {
		t.Fatal("Connect error = nil, want timeout")
	}
	if !strings.Contains(err.Error(), "timed out after 250 ms") {
		t.Errorf("error = %q, want initialize timeout with budget", err)
	}
}

func TestConnectAllSkipsBadServer(t *testing.T) {
	if os.Getenv("SANDBAR_MCP_TEST_SERVER") != "" {
		t.Skip("child process")
	}
	t.Setenv("SANDBAR_MCP_TEST_SERVER", "1")
	servers := map[string]config.MCPServerConfig{
		"aaa-broken": {Type: "local", Command: []string{"sandbar-no-such-binary-xyz"}},
		"zzz-helper": {Type: "local", Command: []string{os.Args[0]}},
		"bad-type":   {Type: "nope"},
	}
	conns, warnings, err := ConnectAll(context.Background(), servers)
	if err != nil {
		t.Fatalf("ConnectAll error: %v", err)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	if len(conns) != 1 || conns[0].Name() != "zzz-helper" {
		t.Fatalf("conns = %v, want only zzz-helper", conns)
	}
	joined := strings.Join(warnings, "\n")
	for _, sub := range []string{
		`skipping server "aaa-broken"`,
		"sandbar-no-such-binary-xyz",
		`skipping server "bad-type"`,
	} {
		if !strings.Contains(joined, sub) {
			t.Errorf("warnings %q missing %q", warnings, sub)
		}
	}
}

func TestConnectRemoteStreamableHTTP(t *testing.T) {
	server := newTestServer()
	var gotHeader atomic.Value
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server {
		return server
	}, &mcpsdk.StreamableHTTPOptions{JSONResponse: true})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader.Store(r.Header.Get("X-Sandbar-Token"))
		handler.ServeHTTP(w, r)
	}))
	defer ts.Close()

	cfg := config.MCPServerConfig{
		Type:      "remote",
		URL:       ts.URL,
		Headers:   map[string]string{"X-Sandbar-Token": "sekrit"},
		TimeoutMS: 5000,
	}
	conn, err := Connect(context.Background(), "remote-test", cfg)
	if err != nil {
		t.Fatalf("Connect(remote): %v", err)
	}
	defer conn.Close()

	listings, err := conn.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(listings) != 4 {
		t.Fatalf("Tools() = %d listings, want 4", len(listings))
	}
	got, err := conn.Call(context.Background(), "echo", json.RawMessage(`{"text":"over http"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got != "over http" {
		t.Errorf("Call(echo) = %q, want %q", got, "over http")
	}
	if h, _ := gotHeader.Load().(string); h != "sekrit" {
		t.Errorf("header seen by server = %q, want sekrit", h)
	}
}

// TestAttachRoundTripInMemory drives the full startup entry point against an
// in-process server via a fake stdio child is unnecessary; Attach itself is
// exercised through ConnectAll and RegisterAll above. This test covers the
// Manager wrapper: Close tears down connections and is idempotent.
func TestManagerClose(t *testing.T) {
	conn := newMemConn(t)
	m := &Manager{conns: []*Conn{conn}}
	if len(m.Connections()) != 1 {
		t.Fatalf("Connections = %d, want 1", len(m.Connections()))
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v (want idempotent)", err)
	}
	var nilManager *Manager
	if err := nilManager.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func keysOf(m map[string]tools.Tool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
