// Package cliadmin contains command-line administration primitives that are
// independent of Sandbar's concrete CLI parser and renderer.
package cliadmin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"sandbar/internal/config"
	uxtheme "sandbar/internal/ui/theme"
)

// ConfigScope identifies one of Sandbar's two configuration files.
type ConfigScope string

const (
	CoreConfig   ConfigScope = "core"
	ClientConfig ConfigScope = "client"
)

// ValueType is the stable, user-facing type of a configuration value.
type ValueType string

const (
	StringValue  ValueType = "string"
	IntegerValue ValueType = "integer"
	NumberValue  ValueType = "number"
	BooleanValue ValueType = "boolean"
	ListValue    ValueType = "list"
	ObjectValue  ValueType = "object"
	NullValue    ValueType = "null"
)

const redactedValue = "<redacted>"

var (
	// ErrCoreMutationUnsupported is returned by Set and Reset for the core
	// configuration. internal/config intentionally has no persistence API, so
	// cliadmin refuses to invent a second writer that could discard comments,
	// environment placeholders, or unknown future fields.
	ErrCoreMutationUnsupported = errors.New("core config is read-only: edit the YAML file directly, then run config validate")

	clientMutationMu sync.Mutex
)

// Field is a typed and, when necessary, redacted configuration leaf.
type Field struct {
	Scope    ConfigScope `json:"scope"`
	Key      string      `json:"key"`
	Type     ValueType   `json:"type"`
	Value    any         `json:"value"`
	Redacted bool        `json:"redacted"`
	Writable bool        `json:"writable"`
}

// Snapshot is the resolved, effective view of a configuration file. Core
// values include defaults and environment overrides applied by config.Load.
type Snapshot struct {
	Scope  ConfigScope `json:"scope"`
	Path   string      `json:"path"`
	Fields []Field     `json:"fields"`
}

// ValidationResult is suitable for both human and JSON config validate output.
// Invalid YAML/configuration is represented by Valid=false rather than an
// operational error so callers can render one consistent result.
type ValidationResult struct {
	Scope   ConfigScope `json:"scope"`
	Path    string      `json:"path"`
	Valid   bool        `json:"valid"`
	Message string      `json:"message"`
}

// Path resolves the configuration file for a scope. explicit follows
// config.Resolve semantics for the core scope. Client config has one canonical
// location and does not accept an override because ClientConfig.Save does not.
func Path(scope ConfigScope, explicit string) (string, error) {
	switch scope {
	case CoreConfig:
		return config.Resolve(explicit)
	case ClientConfig:
		path := clientConfigPath()
		if explicit != "" && filepath.Clean(explicit) != filepath.Clean(path) {
			return "", fmt.Errorf("client config path is fixed at %s", path)
		}
		return path, nil
	default:
		return "", fmt.Errorf("unknown config scope %q (want %q or %q)", scope, CoreConfig, ClientConfig)
	}
}

// Read returns all effective configuration leaves in deterministic key order.
func Read(scope ConfigScope, explicit string) (Snapshot, error) {
	path, err := Path(scope, explicit)
	if err != nil {
		return Snapshot{}, err
	}

	var value any
	switch scope {
	case CoreConfig:
		cfg, err := config.Load(path)
		if err != nil {
			return Snapshot{}, err
		}
		value, err = yamlValue(cfg)
		if err != nil {
			return Snapshot{}, fmt.Errorf("encode effective core config: %w", err)
		}
	case ClientConfig:
		cfg, err := loadClientStrict(path)
		if err != nil {
			return Snapshot{}, err
		}
		value, err = yamlValue(cfg)
		if err != nil {
			return Snapshot{}, fmt.Errorf("encode effective client config: %w", err)
		}
	}

	fields := make([]Field, 0, 32)
	flattenFields(&fields, scope, "", value)
	sort.Slice(fields, func(i, j int) bool { return fields[i].Key < fields[j].Key })
	return Snapshot{Scope: scope, Path: path, Fields: fields}, nil
}

// Get returns one effective configuration leaf by dotted YAML path, for
// example "server.port" or "providers.0.api_key".
func Get(scope ConfigScope, explicit, key string) (Field, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return Field{}, errors.New("config key is required")
	}
	snapshot, err := Read(scope, explicit)
	if err != nil {
		return Field{}, err
	}
	for _, field := range snapshot.Fields {
		if field.Key == key {
			return field, nil
		}
	}
	return Field{}, fmt.Errorf("unknown %s config key %q", scope, key)
}

// Set parses and persists one typed client preference through the existing
// ClientConfig.Save API. Core mutation is intentionally unsupported; see
// ErrCoreMutationUnsupported.
func Set(scope ConfigScope, explicit, key, rawValue string) (Field, error) {
	if scope == CoreConfig {
		return Field{}, ErrCoreMutationUnsupported
	}
	path, err := Path(scope, explicit)
	if err != nil {
		return Field{}, err
	}
	if scope != ClientConfig {
		return Field{}, fmt.Errorf("unknown config scope %q", scope)
	}

	clientMutationMu.Lock()
	defer clientMutationMu.Unlock()
	cfg, err := loadClientStrict(path)
	if err != nil {
		return Field{}, err
	}
	if err := setClientField(cfg, strings.TrimSpace(key), rawValue); err != nil {
		return Field{}, err
	}
	if err := cfg.Save(); err != nil {
		return Field{}, fmt.Errorf("save client config: %w", err)
	}
	return clientField(cfg, strings.TrimSpace(key))
}

// Reset restores one client preference to its built-in default and persists it
// through ClientConfig.Save. Core mutation is intentionally unsupported.
func Reset(scope ConfigScope, explicit, key string) (Field, error) {
	if scope == CoreConfig {
		return Field{}, ErrCoreMutationUnsupported
	}
	path, err := Path(scope, explicit)
	if err != nil {
		return Field{}, err
	}
	if scope != ClientConfig {
		return Field{}, fmt.Errorf("unknown config scope %q", scope)
	}

	clientMutationMu.Lock()
	defer clientMutationMu.Unlock()
	cfg, err := loadClientStrict(path)
	if err != nil {
		return Field{}, err
	}
	key = strings.TrimSpace(key)
	if err := resetClientField(cfg, key); err != nil {
		return Field{}, err
	}
	if err := cfg.Save(); err != nil {
		return Field{}, fmt.Errorf("save client config: %w", err)
	}
	return clientField(cfg, key)
}

// Validate parses and validates one config without exposing secret values.
func Validate(scope ConfigScope, explicit string) (ValidationResult, error) {
	path, err := Path(scope, explicit)
	if err != nil {
		return ValidationResult{}, err
	}
	result := ValidationResult{Scope: scope, Path: path}

	switch scope {
	case CoreConfig:
		if _, err := config.Load(path); err != nil {
			result.Message = err.Error()
			return result, nil
		}
	case ClientConfig:
		cfg, err := loadClientStrict(path)
		if err != nil {
			result.Message = err.Error()
			return result, nil
		}
		if err := validateClient(cfg); err != nil {
			result.Message = err.Error()
			return result, nil
		}
	default:
		return ValidationResult{}, fmt.Errorf("unknown config scope %q", scope)
	}

	result.Valid = true
	result.Message = "configuration is valid"
	return result, nil
}

func clientConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "sandbar", "client.yaml")
}

func loadClientStrict(path string) (*config.ClientConfig, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		cfg, loadErr := config.LoadClientConfig()
		if loadErr != nil {
			return nil, fmt.Errorf("load client config: %w", loadErr)
		}
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read client config: %w", err)
	}
	var syntaxCheck config.ClientConfig
	if err := yaml.Unmarshal(data, &syntaxCheck); err != nil {
		return nil, fmt.Errorf("parse client config: %w", err)
	}
	cfg, err := config.LoadClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load client config: %w", err)
	}
	return cfg, nil
}

func validateClient(cfg *config.ClientConfig) error {
	if cfg == nil {
		return errors.New("client config is nil")
	}
	if _, err := resolveTheme(cfg.Theme); err != nil {
		return err
	}
	switch cfg.ColorMode {
	case config.ColorModeAuto, config.ColorModeAlways, config.ColorModeNever:
	default:
		return fmt.Errorf("color_mode must be auto, always, or never, got %q", cfg.ColorMode)
	}
	if cfg.FontSize <= 0 {
		return fmt.Errorf("font_size must be positive, got %d", cfg.FontSize)
	}
	return nil
}

func resolveTheme(value string) (string, error) {
	id := strings.ToLower(strings.TrimSpace(value))
	if id == "" {
		id = uxtheme.System
	}
	if id == uxtheme.System {
		return id, nil
	}
	if _, ok := uxtheme.Lookup(id); !ok {
		return "", fmt.Errorf("unknown theme %q (use system or one of: %s)", value, strings.Join(uxtheme.IDs(), ", "))
	}
	return id, nil
}

func setClientField(cfg *config.ClientConfig, key, raw string) error {
	switch key {
	case "default_model":
		cfg.DefaultModel = strings.TrimSpace(raw)
	case "theme":
		value, err := resolveTheme(raw)
		if err != nil {
			return err
		}
		cfg.Theme = value
	case "color_mode":
		value := strings.ToLower(strings.TrimSpace(raw))
		switch value {
		case config.ColorModeAuto, config.ColorModeAlways, config.ColorModeNever:
			cfg.ColorMode = value
		default:
			return fmt.Errorf("color_mode must be auto, always, or never, got %q", raw)
		}
	case "font_size":
		value, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("font_size must be an integer: %w", err)
		}
		if value <= 0 {
			return fmt.Errorf("font_size must be positive, got %d", value)
		}
		cfg.FontSize = value
	default:
		return fmt.Errorf("unknown or read-only client config key %q", key)
	}
	return nil
}

func resetClientField(cfg *config.ClientConfig, key string) error {
	switch key {
	case "default_model":
		cfg.DefaultModel = ""
	case "theme":
		cfg.Theme = uxtheme.System
	case "color_mode":
		cfg.ColorMode = config.ColorModeAuto
	case "font_size":
		cfg.FontSize = 15
	default:
		return fmt.Errorf("unknown or read-only client config key %q", key)
	}
	return nil
}

func clientField(cfg *config.ClientConfig, key string) (Field, error) {
	value, ok := map[string]any{
		"default_model": cfg.DefaultModel,
		"theme":         cfg.Theme,
		"color_mode":    cfg.ColorMode,
		"font_size":     cfg.FontSize,
	}[key]
	if !ok {
		return Field{}, fmt.Errorf("unknown client config key %q", key)
	}
	redacted := isSecretPath(key)
	if redacted && value != "" {
		value = redactedValue
	}
	return Field{
		Scope: ClientConfig, Key: key, Type: valueType(value), Value: value,
		Redacted: redacted, Writable: true,
	}, nil
}

func yamlValue(value any) (any, error) {
	data, err := yaml.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func flattenFields(dst *[]Field, scope ConfigScope, prefix string, value any) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			flattenFields(dst, scope, joinKey(prefix, key), typed[key])
		}
	case []any:
		if len(typed) == 0 || scalarSlice(typed) {
			appendField(dst, scope, prefix, typed)
			return
		}
		for i, item := range typed {
			flattenFields(dst, scope, joinKey(prefix, strconv.Itoa(i)), item)
		}
	default:
		appendField(dst, scope, prefix, typed)
	}
}

func appendField(dst *[]Field, scope ConfigScope, key string, value any) {
	redacted := isSecretPath(key)
	if redacted && !isEmptyValue(value) {
		value = redactedValue
	}
	*dst = append(*dst, Field{
		Scope: scope, Key: key, Type: valueType(value), Value: value,
		Redacted: redacted, Writable: scope == ClientConfig && isClientKey(key),
	})
}

func isClientKey(key string) bool {
	switch key {
	case "default_model", "theme", "color_mode", "font_size":
		return true
	default:
		return false
	}
}

func joinKey(prefix, part string) string {
	if prefix == "" {
		return part
	}
	return prefix + "." + part
}

func scalarSlice(values []any) bool {
	for _, value := range values {
		switch value.(type) {
		case map[string]any, []any:
			return false
		}
	}
	return true
}

func isSecretPath(path string) bool {
	for _, part := range strings.Split(strings.ToLower(path), ".") {
		if part == "api_key" || strings.HasSuffix(part, "_api_key") ||
			part == "auth_token" || strings.HasSuffix(part, "_token") ||
			strings.Contains(part, "password") || strings.Contains(part, "secret") {
			return true
		}
	}
	return false
}

func isEmptyValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

func valueType(value any) ValueType {
	switch value.(type) {
	case nil:
		return NullValue
	case string:
		return StringValue
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return IntegerValue
	case float32, float64:
		return NumberValue
	case bool:
		return BooleanValue
	case []any, []string:
		return ListValue
	default:
		return ObjectValue
	}
}
