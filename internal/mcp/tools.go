package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aetherbird/sandbar/internal/config"
	"github.com/aetherbird/sandbar/internal/tools"
)

// Attach connects every configured server in parallel under BootBudget,
// wraps each healthy connection's tools, and registers them into the
// registry as mcp_<server>_<tool>. It is the single startup entry point:
// per-server failures are isolated and returned as warnings for the CLI to
// print at boot, and the returned Manager must be Closed on shutdown to tear
// down sessions and child processes. An empty server set returns a nil
// Manager with no work done. The error is non-nil only when ctx is
// cancelled.
func Attach(ctx context.Context, registry *tools.Registry, servers map[string]config.MCPServerConfig) (*Manager, []string, error) {
	conns, warnings, err := ConnectAll(ctx, servers)
	if err != nil {
		return nil, warnings, err
	}
	// Bound the tool-listing phase with the same boot budget so a server
	// that connects but never answers ListTools cannot stall startup.
	listCtx, cancel := context.WithTimeout(ctx, BootBudget)
	defer cancel()
	warnings = append(warnings, RegisterAll(listCtx, registry, conns)...)
	return &Manager{conns: conns}, warnings, nil
}

// WrapTools lists conn's tools and adapts each into a tools.Tool. Names
// become "mcp_<server>_<tool>" sanitized to [a-zA-Z0-9_]; Description and
// Schema pass the server's values through. ctx bounds the listing.
func WrapTools(ctx context.Context, conn *Conn, serverName string) ([]tools.Tool, error) {
	listings, err := conn.Tools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tools.Tool, 0, len(listings))
	for _, l := range listings {
		out = append(out, newTool(conn, serverName, l))
	}
	return out, nil
}

// RegisterAll wraps every connection's tools and registers them into the
// registry. A server whose tools cannot be listed yields a warning and is
// skipped.
func RegisterAll(ctx context.Context, registry *tools.Registry, conns []*Conn) []string {
	var warnings []string
	for _, conn := range conns {
		ts, err := WrapTools(ctx, conn, conn.Name())
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("mcp: skipping tools from server %q: %v", conn.Name(), err))
			continue
		}
		for _, t := range ts {
			registry.Register(t)
		}
	}
	return warnings
}

// newTool adapts one server tool listing to a registry Tool.
func newTool(conn *Conn, serverName string, l ToolListing) tools.Tool {
	name := "mcp_" + sanitizeName(serverName) + "_" + sanitizeName(l.Name)
	description := l.Description
	if description == "" {
		description = fmt.Sprintf("MCP tool %q from server %q.", l.Name, serverName)
	}
	toolName := l.Name
	return tools.Tool{
		Name:        name,
		Description: description,
		Schema:      schemaMap(l.Schema),
		// A foreign server's blast radius is unknowable, so every MCP tool
		// starts at write tier: approvals gate it outside yolo mode without
		// jumping straight to exec classification. Per-tool overrides work
		// through tools.approval.rules, which matches exact tool names —
		// including mcp_<server>_<tool>.
		Metadata: tools.ToolMetadata{Tier: tools.TierWrite},
		Execute: func(ctx context.Context, args map[string]interface{}) (string, error) {
			raw, err := json.Marshal(args)
			if err != nil {
				return "", fmt.Errorf("mcp tool %s: encoding arguments: %w", name, err)
			}
			return conn.Call(ctx, toolName, raw)
		},
	}
}

// schemaMap normalizes a server's inputSchema into the map form the tool
// registry consumes. Wire-decoded schemas already are maps, but a server
// reached over an in-process transport may hand over json.RawMessage.
// Anything that is not (or does not decode to) a JSON object falls back to a
// bare object schema so the tool still registers and renders.
func schemaMap(schema any) map[string]interface{} {
	var decoded map[string]interface{}
	switch v := schema.(type) {
	case nil:
	case map[string]interface{}:
		decoded = v
	case json.RawMessage:
		_ = json.Unmarshal(v, &decoded)
	default:
		if raw, err := json.Marshal(schema); err == nil {
			_ = json.Unmarshal(raw, &decoded)
		}
	}
	if decoded == nil {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	return decoded
}

// renderContent flattens a call result's content parts into one string.
// Text parts join with newlines; other kinds render as "[<contentType> part]".
func renderContent(parts []mcpsdk.Content) string {
	rendered := make([]string, 0, len(parts))
	for _, p := range parts {
		switch v := p.(type) {
		case *mcpsdk.TextContent:
			rendered = append(rendered, v.Text)
		case *mcpsdk.ImageContent:
			rendered = append(rendered, "[image part]")
		case *mcpsdk.AudioContent:
			rendered = append(rendered, "[audio part]")
		case *mcpsdk.ResourceLink:
			rendered = append(rendered, "[resource_link part]")
		case *mcpsdk.EmbeddedResource:
			rendered = append(rendered, "[resource part]")
		default:
			rendered = append(rendered, "[unknown part]")
		}
	}
	return strings.Join(rendered, "\n")
}

// sanitizeName maps s onto [a-zA-Z0-9_], replacing anything else with '_'.
func sanitizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
