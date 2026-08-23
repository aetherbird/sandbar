// Package mcp attaches configured MCP servers (internal/config) to sandbar
// using the official Go SDK (github.com/modelcontextprotocol/go-sdk).
//
// Connect spawns (Type "local") or dials (Type "remote") one server and
// completes the initialize handshake, Conn exposes listing and calling, and
// Attach is the runtime entry point: it connects every configured server in
// parallel under a bounded boot budget, registers each server's tools as
// mcp_<server>_<tool>, and collects per-server failures as warnings so one
// bad server is skipped instead of blocking startup.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aetherbird/sandbar/internal/config"
)

// BootBudget bounds the whole parallel connect phase so a hung server cannot
// stall runtime construction, regardless of per-server timeout_ms settings.
const BootBudget = 5 * time.Second

// clientImpl identifies sandbar to servers during the initialize handshake.
var clientImpl = &mcpsdk.Implementation{Name: "sandbar", Version: "0.1.0"}

// ToolListing is one tool advertised by a connected server.
type ToolListing struct {
	Name        string
	Description string
	// Schema is the server's inputSchema, passed through uninterpreted
	// (a JSON Schema object, decoded as map[string]any).
	Schema any
}

// Conn is one connected MCP server session.
type Conn struct {
	name    string
	session *mcpsdk.ClientSession
}

// Name returns the configured server name.
func (c *Conn) Name() string { return c.name }

// Tools lists the tools the server advertises, following the server's
// pagination cursor until exhausted. ctx bounds the listing round-trips and
// is honored by cancelation between pages.
func (c *Conn) Tools(ctx context.Context) ([]ToolListing, error) {
	var out []ToolListing
	cursor := ""
	for {
		res, err := c.session.ListTools(ctx, &mcpsdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("mcp server %q: listing tools: %w", c.name, err)
		}
		for _, t := range res.Tools {
			out = append(out, ToolListing{Name: t.Name, Description: t.Description, Schema: t.InputSchema})
		}
		if res.NextCursor == "" || res.NextCursor == cursor {
			return out, nil
		}
		cursor = res.NextCursor
	}
}

// Call invokes one tool and renders its reply as text: text content parts
// are concatenated, non-text parts render as "[<contentType> part]". A
// server-side tool error (result IsError) yields both the rendered text and
// a non-nil error whose message carries that text, so the model still sees
// what went wrong.
func (c *Conn) Call(ctx context.Context, toolName string, arguments json.RawMessage) (string, error) {
	var args any
	if len(arguments) > 0 {
		args = arguments // json.RawMessage marshals as raw JSON
	}
	res, err := c.session.CallTool(ctx, &mcpsdk.CallToolParams{Name: toolName, Arguments: args})
	if err != nil {
		return "", fmt.Errorf("mcp server %q: calling tool %q: %w", c.name, toolName, err)
	}
	text := renderContent(res.Content)
	if res.IsError {
		msg := text
		if msg == "" {
			msg = "no content returned"
		}
		return text, fmt.Errorf("mcp server %q: tool %q failed: %s", c.name, toolName, msg)
	}
	return text, nil
}

// Close closes the session; for local servers this also shuts the process
// down (stdin close, then SIGTERM/KILL if it lingers).
func (c *Conn) Close() error {
	if err := c.session.Close(); err != nil {
		return fmt.Errorf("mcp server %q: close: %w", c.name, err)
	}
	return nil
}

// Connect spawns (Type "local") or dials (Type "remote") one configured MCP
// server and completes the initialize handshake. cfg.TimeoutMS bounds
// startup + initialize (0 = only the caller's deadline applies). Local
// commands inherit the parent environment plus AGENT=1; remote servers may
// carry static headers.
func Connect(ctx context.Context, name string, cfg config.MCPServerConfig) (*Conn, error) {
	if cfg.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(cfg.TimeoutMS)*time.Millisecond)
		defer cancel()
	}
	var transport mcpsdk.Transport
	switch cfg.Type {
	case "local":
		if len(cfg.Command) == 0 {
			return nil, fmt.Errorf("mcp server %q: local server needs a command", name)
		}
		cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
		cmd.Env = append(os.Environ(), "AGENT=1")
		// A local server's stderr is its log channel; discarding keeps the
		// TUI render clean.
		cmd.Stderr = nil
		transport = &mcpsdk.CommandTransport{Command: cmd}
	case "remote":
		if cfg.URL == "" {
			return nil, fmt.Errorf("mcp server %q: remote server needs a url", name)
		}
		transport = &mcpsdk.StreamableClientTransport{
			Endpoint:   cfg.URL,
			HTTPClient: &http.Client{Transport: headerTransport{headers: cfg.Headers}},
		}
	default:
		return nil, fmt.Errorf("mcp server %q: unknown type %q (want \"local\" or \"remote\")", name, cfg.Type)
	}
	conn, err := connectTransport(ctx, name, transport)
	if err != nil {
		return nil, connectError(name, cfg, err, ctx.Err())
	}
	return conn, nil
}

// connectTransport runs the initialize handshake over an existing transport
// (the seam the in-memory tests use).
func connectTransport(ctx context.Context, name string, t mcpsdk.Transport) (*Conn, error) {
	client := mcpsdk.NewClient(clientImpl, nil)
	session, err := client.Connect(ctx, t, nil)
	if err != nil {
		return nil, err
	}
	return &Conn{name: name, session: session}, nil
}

// connectError rewrites handshake failures into messages a user can act on:
// spawn failures carry the argv, timeout carries the budget. ctxErr is the
// handshake context's own error (the SDK does not always wrap it into err).
func connectError(name string, cfg config.MCPServerConfig, err, ctxErr error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctxErr, context.DeadlineExceeded) {
		return fmt.Errorf("mcp server %q: initialize timed out after %d ms", name, cfg.TimeoutMS)
	}
	if cfg.Type == "local" {
		return fmt.Errorf("mcp server %q: starting %v failed: %w", name, cfg.Command, err)
	}
	return fmt.Errorf("mcp server %q: connecting to %s failed: %w", name, cfg.URL, err)
}

// ConnectAll connects every configured server in parallel under BootBudget.
// Per-server failures are isolated: a bad server yields a warning and is
// skipped so startup is never blocked. A server configured with a
// timeout_ms larger than BootBudget extends the overall budget (plus 1s
// slack) so an explicit timeout can always take effect. The returned slice
// holds only healthy connections, in sorted-name order. The error is
// non-nil only when ctx is cancelled (any connections made along the way
// are closed first).
func ConnectAll(ctx context.Context, servers map[string]config.MCPServerConfig) ([]*Conn, []string, error) {
	budget := time.Duration(BootBudget)
	for _, cfg := range servers {
		if d := time.Duration(cfg.TimeoutMS) * time.Millisecond; d+time.Second > budget {
			budget = d + time.Second
		}
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	names := slices.Sorted(maps.Keys(servers))
	conns := make([]*Conn, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string, cfg config.MCPServerConfig) {
			defer wg.Done()
			conns[i], errs[i] = Connect(ctx, name, cfg)
		}(i, name, servers[name])
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		for _, c := range conns {
			if c != nil {
				c.Close()
			}
		}
		return nil, nil, err
	}
	var out []*Conn
	var warnings []string
	for i, name := range names {
		if errs[i] != nil {
			warnings = append(warnings, fmt.Sprintf("mcp: skipping server %q: %v", name, errs[i]))
			continue
		}
		out = append(out, conns[i])
	}
	return out, warnings, nil
}

// Manager owns the MCP server sessions attached to one runtime.
type Manager struct {
	conns []*Conn
}

// Connections returns the healthy connections in sorted-name order.
func (m *Manager) Connections() []*Conn {
	if m == nil {
		return nil
	}
	return m.conns
}

// Close closes every connection; it is safe to call on a nil Manager.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	var errs []error
	for _, c := range m.conns {
		if err := c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	m.conns = nil
	return errors.Join(errs...)
}

// headerTransport injects configured static headers into every request to a
// remote server.
type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range t.headers {
		clone.Header.Set(k, v)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}
