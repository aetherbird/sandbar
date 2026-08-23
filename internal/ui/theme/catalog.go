// Package theme provides Sandbar's shared semantic color-theme catalog.
//
// The catalog is deliberately UI-toolkit agnostic. Terminal clients can map
// these tokens to Lip Gloss styles; this Go catalog is the source of truth for
// palette definitions.
package theme

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	// Version is incremented when the generated catalog contract changes.
	Version = 1

	// System asks a client to select the light or dark base palette after
	// inspecting its display environment. It is not itself a concrete palette.
	System = "system"
)

// Scheme describes the background family a palette was designed for.
type Scheme string

const (
	SchemeLight Scheme = "light"
	SchemeDark  Scheme = "dark"
)

// Tokens is the common semantic color vocabulary shared by Sandbar clients.
// Opaque colors use six-digit hexadecimal notation. AccentSoft is a CSS-ready
// translucent color and is normally used for focus rings and subtle fills.
type Tokens struct {
	Surface0   string `json:"surface_0" yaml:"surface_0"`
	Surface1   string `json:"surface_1" yaml:"surface_1"`
	Surface2   string `json:"surface_2" yaml:"surface_2"`
	Surface3   string `json:"surface_3" yaml:"surface_3"`
	Text1      string `json:"text_1" yaml:"text_1"`
	Text2      string `json:"text_2" yaml:"text_2"`
	Text3      string `json:"text_3" yaml:"text_3"`
	Border1    string `json:"border_1" yaml:"border_1"`
	Border2    string `json:"border_2" yaml:"border_2"`
	Accent     string `json:"accent" yaml:"accent"`
	AccentSoft string `json:"accent_soft" yaml:"accent_soft"`
	AccentFG   string `json:"accent_fg" yaml:"accent_fg"`
	Success    string `json:"success" yaml:"success"`
	Warning    string `json:"warning" yaml:"warning"`
	Danger     string `json:"danger" yaml:"danger"`
}

// Palette is one concrete Sandbar color theme.
type Palette struct {
	ID     string `json:"id" yaml:"id"`
	Label  string `json:"label" yaml:"label"`
	Group  string `json:"group" yaml:"group"`
	Scheme Scheme `json:"scheme" yaml:"scheme"`
	Tokens Tokens `json:"tokens" yaml:"tokens"`
}

const (
	defaultSuccess = "#10b981"
	defaultWarning = "#f59e0b"
	defaultDanger  = "#ef4444"
)

func palette(id, label, group string, scheme Scheme, tokens Tokens) Palette {
	// State colors are currently shared by the web design system. Keeping the
	// defaults here makes every returned Palette fully resolved while avoiding
	// three noisy repeated fields in every catalog entry.
	if tokens.Success == "" {
		tokens.Success = defaultSuccess
	}
	if tokens.Warning == "" {
		tokens.Warning = defaultWarning
	}
	if tokens.Danger == "" {
		tokens.Danger = defaultDanger
	}
	return Palette{ID: id, Label: label, Group: group, Scheme: scheme, Tokens: tokens}
}

// builtinPalettes follows the order used by Sandbar's appearance picker.
var builtinPalettes = mustCatalog([]Palette{
	palette("light", "Light", "System", SchemeLight, Tokens{
		Surface0: "#ffffff", Surface1: "#fafafa", Surface2: "#f4f4f5", Surface3: "#e4e4e7",
		Text1: "#18181b", Text2: "#52525b", Text3: "#a1a1aa",
		Border1: "#e4e4e7", Border2: "#d4d4d8",
		Accent: "#3b82f6", AccentSoft: "rgba(59, 130, 246, 0.1)", AccentFG: "#ffffff",
	}),
	palette("dark", "Dark", "System", SchemeDark, Tokens{
		Surface0: "#0a0a0a", Surface1: "#131316", Surface2: "#1c1c20", Surface3: "#2a2a2f",
		Text1: "#fafafa", Text2: "#a1a1aa", Text3: "#71717a",
		Border1: "#27272a", Border2: "#3f3f46",
		Accent: "#60a5fa", AccentSoft: "rgba(96, 165, 250, 0.15)", AccentFG: "#0a0a0a",
	}),
	palette("monochrome", "Monochrome", "System", SchemeLight, Tokens{
		Surface0: "#ffffff", Surface1: "#f5f5f5", Surface2: "#ebebeb", Surface3: "#d4d4d4",
		Text1: "#0a0a0a", Text2: "#404040", Text3: "#737373",
		Border1: "#d4d4d4", Border2: "#a3a3a3",
		Accent: "#0a0a0a", AccentSoft: "rgba(0, 0, 0, 0.08)", AccentFG: "#ffffff",
	}),
	palette("catppuccin-latte", "Catppuccin Latte", "Catppuccin", SchemeLight, Tokens{
		Surface0: "#eff1f5", Surface1: "#e6e9ef", Surface2: "#dce0e8", Surface3: "#ccd0da",
		Text1: "#4c4f69", Text2: "#6c6f85", Text3: "#8c8fa1",
		Border1: "#ccd0da", Border2: "#bcc0cc",
		Accent: "#1e66f5", AccentSoft: "rgba(30, 102, 245, 0.12)", AccentFG: "#ffffff",
	}),
	palette("catppuccin-mocha", "Catppuccin Mocha", "Catppuccin", SchemeDark, Tokens{
		Surface0: "#1e1e2e", Surface1: "#181825", Surface2: "#313244", Surface3: "#45475a",
		Text1: "#cdd6f4", Text2: "#bac2de", Text3: "#a6adc8",
		Border1: "#313244", Border2: "#45475a",
		Accent: "#cba6f7", AccentSoft: "rgba(203, 166, 247, 0.15)", AccentFG: "#1e1e2e",
	}),
	palette("tokyo-night", "Tokyo Night", "Tokyo Night", SchemeDark, Tokens{
		Surface0: "#1a1b26", Surface1: "#16161e", Surface2: "#24283b", Surface3: "#414868",
		Text1: "#c0caf5", Text2: "#a9b1d6", Text3: "#787c99",
		Border1: "#24283b", Border2: "#414868",
		Accent: "#7aa2f7", AccentSoft: "rgba(122, 162, 247, 0.15)", AccentFG: "#1a1b26",
	}),
	palette("tokyo-midnight", "Tokyo Midnight", "Tokyo Night", SchemeDark, Tokens{
		Surface0: "#06060d", Surface1: "#0a0a14", Surface2: "#10101e", Surface3: "#1a1a2e",
		Text1: "#e8eaf6", Text2: "#c5c8e8", Text3: "#7b7fa8",
		Border1: "#1a1a2e", Border2: "#2a2a4a",
		Accent: "#82aaff", AccentSoft: "rgba(130, 170, 255, 0.18)", AccentFG: "#06060d",
	}),
	palette("tokyo-night-light", "Tokyo Night Light", "Tokyo Night", SchemeLight, Tokens{
		Surface0: "#e1e2e7", Surface1: "#d5d6db", Surface2: "#cbccd1", Surface3: "#b7b9c5",
		Text1: "#343b58", Text2: "#4f566a", Text3: "#6c6e75",
		Border1: "#b7b9c5", Border2: "#9699a3",
		Accent: "#2959aa", AccentSoft: "rgba(41, 89, 170, 0.12)", AccentFG: "#ffffff",
	}),
	palette("rose-pine", "Rosé Pine", "Rosé Pine", SchemeDark, Tokens{
		Surface0: "#191724", Surface1: "#1f1d2e", Surface2: "#26233a", Surface3: "#393552",
		Text1: "#e0def4", Text2: "#908caa", Text3: "#6e6a86",
		Border1: "#26233a", Border2: "#393552",
		Accent: "#ebbcba", AccentSoft: "rgba(235, 188, 186, 0.15)", AccentFG: "#191724",
	}),
	palette("rose-pine-moon", "Rosé Pine Moon", "Rosé Pine", SchemeDark, Tokens{
		Surface0: "#232136", Surface1: "#2a273f", Surface2: "#393552", Surface3: "#44415a",
		Text1: "#e0def4", Text2: "#908caa", Text3: "#6e6a86",
		Border1: "#393552", Border2: "#44415a",
		Accent: "#c4a7e7", AccentSoft: "rgba(196, 167, 231, 0.15)", AccentFG: "#232136",
	}),
	palette("rose-pine-dawn", "Rosé Pine Dawn", "Rosé Pine", SchemeLight, Tokens{
		Surface0: "#faf4ed", Surface1: "#fffaf3", Surface2: "#f2e9e1", Surface3: "#dfdad9",
		Text1: "#575279", Text2: "#797593", Text3: "#9893a5",
		Border1: "#dfdad9", Border2: "#cecacd",
		Accent: "#b4637a", AccentSoft: "rgba(180, 99, 122, 0.12)", AccentFG: "#ffffff",
	}),
	palette("gruvbox-dark", "Gruvbox Dark", "Gruvbox", SchemeDark, Tokens{
		Surface0: "#282828", Surface1: "#1d2021", Surface2: "#3c3836", Surface3: "#504945",
		Text1: "#ebdbb2", Text2: "#d5c4a1", Text3: "#a89984",
		Border1: "#3c3836", Border2: "#504945",
		Accent: "#fabd2f", AccentSoft: "rgba(250, 189, 47, 0.15)", AccentFG: "#282828",
	}),
	palette("gruvbox-light", "Gruvbox Light", "Gruvbox", SchemeLight, Tokens{
		Surface0: "#fbf1c7", Surface1: "#f2e5bc", Surface2: "#ebdbb2", Surface3: "#d5c4a1",
		Text1: "#3c3836", Text2: "#504945", Text3: "#7c6f64",
		Border1: "#d5c4a1", Border2: "#bdae93",
		Accent: "#af3a03", AccentSoft: "rgba(175, 58, 3, 0.12)", AccentFG: "#fbf1c7",
	}),
	palette("dracula", "Dracula", "Other Dark", SchemeDark, Tokens{
		Surface0: "#282a36", Surface1: "#21222c", Surface2: "#44475a", Surface3: "#6272a4",
		Text1: "#f8f8f2", Text2: "#bfbfbf", Text3: "#6272a4",
		Border1: "#44475a", Border2: "#6272a4",
		Accent: "#bd93f9", AccentSoft: "rgba(189, 147, 249, 0.15)", AccentFG: "#282a36",
	}),
	palette("one-dark", "One Dark", "Other Dark", SchemeDark, Tokens{
		Surface0: "#282c34", Surface1: "#21252b", Surface2: "#2c313a", Surface3: "#3e4451",
		Text1: "#abb2bf", Text2: "#828997", Text3: "#5c6370",
		Border1: "#3e4451", Border2: "#4b5263",
		Accent: "#61afef", AccentSoft: "rgba(97, 175, 239, 0.15)", AccentFG: "#282c34",
	}),
	palette("everforest", "Everforest", "Other Dark", SchemeDark, Tokens{
		Surface0: "#2d353b", Surface1: "#232a2e", Surface2: "#343f44", Surface3: "#4f585e",
		Text1: "#d3c6aa", Text2: "#a7c080", Text3: "#859289",
		Border1: "#343f44", Border2: "#4f585e",
		Accent: "#a7c080", AccentSoft: "rgba(167, 192, 128, 0.15)", AccentFG: "#2d353b",
	}),
	palette("kanagawa-wave", "Kanagawa Wave", "Other Dark", SchemeDark, Tokens{
		Surface0: "#1f1f28", Surface1: "#16161d", Surface2: "#2a2a37", Surface3: "#363646",
		Text1: "#dcd7ba", Text2: "#c8c093", Text3: "#727169",
		Border1: "#2a2a37", Border2: "#363646",
		Accent: "#7e9cd8", AccentSoft: "rgba(126, 156, 216, 0.15)", AccentFG: "#1f1f28",
	}),
	palette("solarized-dark", "Solarized Dark", "Other Dark", SchemeDark, Tokens{
		Surface0: "#002b36", Surface1: "#073642", Surface2: "#094352", Surface3: "#586e75",
		Text1: "#eee8d5", Text2: "#93a1a1", Text3: "#839496",
		Border1: "#073642", Border2: "#586e75",
		Accent: "#268bd2", AccentSoft: "rgba(38, 139, 210, 0.15)", AccentFG: "#fdf6e3",
	}),
	palette("nord", "Nord", "Other Dark", SchemeDark, Tokens{
		Surface0: "#2e3440", Surface1: "#3b4252", Surface2: "#434c5e", Surface3: "#4c566a",
		Text1: "#eceff4", Text2: "#d8dee9", Text3: "#a0a8b8",
		Border1: "#3b4252", Border2: "#4c566a",
		Accent: "#88c0d0", AccentSoft: "rgba(136, 192, 208, 0.15)", AccentFG: "#2e3440",
	}),
	palette("synthwave", "Synthwave '84", "Other Dark", SchemeDark, Tokens{
		Surface0: "#241b2f", Surface1: "#1a1325", Surface2: "#34294f", Surface3: "#495495",
		Text1: "#ffffff", Text2: "#f0c8ff", Text3: "#b893ce",
		Border1: "#34294f", Border2: "#495495",
		Accent: "#f97e72", AccentSoft: "rgba(249, 126, 114, 0.18)", AccentFG: "#241b2f",
	}),
	palette("github-light", "GitHub Light", "Other Light", SchemeLight, Tokens{
		Surface0: "#ffffff", Surface1: "#f6f8fa", Surface2: "#eaeef2", Surface3: "#d0d7de",
		Text1: "#1f2328", Text2: "#656d76", Text3: "#8c959f",
		Border1: "#d0d7de", Border2: "#afb8c1",
		Accent: "#0969da", AccentSoft: "rgba(9, 105, 218, 0.1)", AccentFG: "#ffffff",
	}),
})

var builtinByID = indexCatalog(builtinPalettes)

var (
	idPattern        = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	hexColorPattern  = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	softColorPattern = regexp.MustCompile(`^(?:#[0-9a-fA-F]{6}|rgba?\([^\r\n()]+\))$`)
)

// List returns all concrete palettes in stable appearance-picker order.
// The returned slice is independent of the package's catalog.
func List() []Palette {
	return append([]Palette(nil), builtinPalettes...)
}

// IDs returns the concrete palette identifiers in stable picker order.
// System is intentionally omitted because it resolves to light or dark.
func IDs() []string {
	ids := make([]string, len(builtinPalettes))
	for i, p := range builtinPalettes {
		ids[i] = p.ID
	}
	return ids
}

// Lookup returns a concrete palette by ID. IDs are case-insensitive and may
// contain surrounding whitespace. System is not a concrete palette.
func Lookup(id string) (Palette, bool) {
	p, ok := builtinByID[normalizeID(id)]
	return p, ok
}

// Resolve resolves a configured name to a concrete palette. Empty and system
// select the light or dark base palette according to darkBackground.
func Resolve(name string, darkBackground bool) (Palette, error) {
	id := normalizeID(name)
	if id == "" || id == System {
		id = "light"
		if darkBackground {
			id = "dark"
		}
	}
	if p, ok := Lookup(id); ok {
		return p, nil
	}
	return Palette{}, fmt.Errorf("unknown Sandbar theme %q (available: %s, %s)", name, System, strings.Join(IDs(), ", "))
}

// Validate checks one palette for a complete, well-formed semantic contract.
func Validate(p Palette) error {
	if !idPattern.MatchString(p.ID) {
		return fmt.Errorf("theme id %q must use lowercase kebab-case", p.ID)
	}
	if strings.TrimSpace(p.Label) == "" {
		return fmt.Errorf("theme %q has an empty label", p.ID)
	}
	if strings.TrimSpace(p.Group) == "" {
		return fmt.Errorf("theme %q has an empty group", p.ID)
	}
	if p.Scheme != SchemeLight && p.Scheme != SchemeDark {
		return fmt.Errorf("theme %q has invalid scheme %q", p.ID, p.Scheme)
	}

	opaque := []struct {
		name, value string
	}{
		{"surface_0", p.Tokens.Surface0}, {"surface_1", p.Tokens.Surface1},
		{"surface_2", p.Tokens.Surface2}, {"surface_3", p.Tokens.Surface3},
		{"text_1", p.Tokens.Text1}, {"text_2", p.Tokens.Text2}, {"text_3", p.Tokens.Text3},
		{"border_1", p.Tokens.Border1}, {"border_2", p.Tokens.Border2},
		{"accent", p.Tokens.Accent}, {"accent_fg", p.Tokens.AccentFG},
		{"success", p.Tokens.Success}, {"warning", p.Tokens.Warning}, {"danger", p.Tokens.Danger},
	}
	for _, token := range opaque {
		if !hexColorPattern.MatchString(token.value) {
			return fmt.Errorf("theme %q token %s must be a six-digit hex color, got %q", p.ID, token.name, token.value)
		}
	}
	if !softColorPattern.MatchString(p.Tokens.AccentSoft) {
		return fmt.Errorf("theme %q token accent_soft has invalid color %q", p.ID, p.Tokens.AccentSoft)
	}
	return nil
}

// ValidateCatalog validates every palette and rejects duplicate IDs.
func ValidateCatalog(palettes []Palette) error {
	if len(palettes) == 0 {
		return fmt.Errorf("theme catalog is empty")
	}
	seen := make(map[string]struct{}, len(palettes))
	for i, p := range palettes {
		if err := Validate(p); err != nil {
			return fmt.Errorf("palette %d: %w", i, err)
		}
		if _, ok := seen[p.ID]; ok {
			return fmt.Errorf("duplicate theme id %q", p.ID)
		}
		seen[p.ID] = struct{}{}
	}
	return nil
}

func mustCatalog(palettes []Palette) []Palette {
	if err := ValidateCatalog(palettes); err != nil {
		panic("invalid built-in Sandbar theme catalog: " + err.Error())
	}
	seen := make(map[string]bool, len(palettes))
	for _, p := range palettes {
		seen[p.ID] = true
	}
	if !seen["light"] || !seen["dark"] {
		panic("invalid built-in Sandbar theme catalog: light and dark palettes are required")
	}
	return palettes
}

func indexCatalog(palettes []Palette) map[string]Palette {
	indexed := make(map[string]Palette, len(palettes))
	for _, p := range palettes {
		indexed[p.ID] = p
	}
	return indexed
}

func normalizeID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}
