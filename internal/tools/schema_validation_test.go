package tools

import (
	"context"
	"strings"
	"testing"
)

func TestValidateToolArgumentsEnforcesClosedObjectAndTypes(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"name":  map[string]interface{}{"type": "string", "minLength": 1},
			"count": map[string]interface{}{"type": "integer", "minimum": 1},
		},
		"required":             []string{"name"},
		"additionalProperties": false,
	}
	for _, tc := range []struct {
		name string
		args map[string]interface{}
		want string
	}{
		{name: "valid", args: map[string]interface{}{"name": "ok", "count": 2.0}},
		{name: "unknown", args: map[string]interface{}{"name": "ok", "workspace": "/elsewhere"}, want: "unsupported fields: workspace"},
		{name: "wrong type", args: map[string]interface{}{"name": 7}, want: "arguments.name must be a string"},
		{name: "fractional integer", args: map[string]interface{}{"name": "ok", "count": 1.5}, want: "arguments.count must be an integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateToolArguments(schema, tc.args)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validation error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRegistryRejectsInvalidSchemaArgumentsBeforeExecution(t *testing.T) {
	registry := NewRegistry(t.TempDir(), "", "", nil)
	called := false
	registry.Register(Tool{
		Name:     "closed_tool",
		Metadata: ToolMetadata{Tier: TierRead},
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"value": map[string]interface{}{"type": "string"},
			},
			"required":             []string{"value"},
			"additionalProperties": false,
		},
		Execute: func(context.Context, map[string]interface{}) (string, error) {
			called = true
			return "unexpected", nil
		},
	})

	_, err := registry.Execute(context.Background(), "closed_tool", map[string]interface{}{
		"value":     "ok",
		"workspace": "/model-selected",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported fields: workspace") {
		t.Fatalf("Execute error = %v", err)
	}
	if called {
		t.Fatal("executor ran with schema-invalid arguments")
	}
}

func TestRegistryRejectsNonStringFilePatchReplacement(t *testing.T) {
	registry := NewRegistry(t.TempDir(), "", "", nil)
	_, err := registry.Execute(context.Background(), "file_patch", map[string]interface{}{
		"path":            "value.txt",
		"old_str":         "old",
		"new_str":         7,
		"expected_sha256": strings.Repeat("0", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "arguments.new_str must be a string") {
		t.Fatalf("file_patch error = %v", err)
	}
}
