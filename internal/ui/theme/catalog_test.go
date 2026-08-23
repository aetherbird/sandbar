package theme

import (
	"reflect"
	"strings"
	"testing"
)

func TestCatalogContainsEveryWebPalette(t *testing.T) {
	want := []string{
		"light", "dark", "monochrome",
		"catppuccin-latte", "catppuccin-mocha",
		"tokyo-night", "tokyo-midnight", "tokyo-night-light",
		"rose-pine", "rose-pine-moon", "rose-pine-dawn",
		"gruvbox-dark", "gruvbox-light",
		"dracula", "one-dark", "everforest", "kanagawa-wave",
		"solarized-dark", "nord", "synthwave", "github-light",
	}
	if got := IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("theme IDs differ:\n got: %q\nwant: %q", got, want)
	}
	if got := len(List()); got != 21 {
		t.Fatalf("List returned %d palettes, want 21", got)
	}
	if err := ValidateCatalog(List()); err != nil {
		t.Fatalf("built-in catalog is invalid: %v", err)
	}
}

func TestLookupNormalizesID(t *testing.T) {
	p, ok := Lookup("  TOKYO-MIDNIGHT ")
	if !ok {
		t.Fatal("Lookup did not normalize the theme ID")
	}
	if p.ID != "tokyo-midnight" || p.Scheme != SchemeDark {
		t.Fatalf("Lookup returned %+v", p)
	}
	if _, ok := Lookup(System); ok {
		t.Fatal("system must not be exposed as a concrete palette")
	}
	if _, ok := Lookup("not-a-theme"); ok {
		t.Fatal("unknown theme unexpectedly resolved")
	}
}

func TestResolveSystemAndNamedTheme(t *testing.T) {
	tests := []struct {
		name           string
		darkBackground bool
		want           string
	}{
		{"", false, "light"},
		{"system", false, "light"},
		{"system", true, "dark"},
		{" SYSTEM ", true, "dark"},
		{"rose-pine-dawn", true, "rose-pine-dawn"},
	}
	for _, tc := range tests {
		p, err := Resolve(tc.name, tc.darkBackground)
		if err != nil {
			t.Fatalf("Resolve(%q, %v): %v", tc.name, tc.darkBackground, err)
		}
		if p.ID != tc.want {
			t.Errorf("Resolve(%q, %v) = %q, want %q", tc.name, tc.darkBackground, p.ID, tc.want)
		}
	}
	if _, err := Resolve("ocean-floor", false); err == nil || !strings.Contains(err.Error(), "unknown Sandbar theme") {
		t.Fatalf("Resolve unknown error = %v", err)
	}
}

func TestListAndIDsReturnIndependentSlices(t *testing.T) {
	palettes := List()
	palettes[0].ID = "mutated"
	if p, _ := Lookup("light"); p.ID != "light" {
		t.Fatalf("mutating List changed catalog: %+v", p)
	}

	ids := IDs()
	ids[0] = "mutated"
	if got := IDs()[0]; got != "light" {
		t.Fatalf("mutating IDs changed catalog: first ID is %q", got)
	}
}

func TestValidateRejectsMalformedPalette(t *testing.T) {
	valid, _ := Lookup("dark")
	tests := []struct {
		name   string
		mutate func(*Palette)
		match  string
	}{
		{"bad id", func(p *Palette) { p.ID = "Bad ID" }, "kebab-case"},
		{"empty label", func(p *Palette) { p.Label = " " }, "empty label"},
		{"empty group", func(p *Palette) { p.Group = "" }, "empty group"},
		{"bad scheme", func(p *Palette) { p.Scheme = "sepia" }, "invalid scheme"},
		{"bad opaque color", func(p *Palette) { p.Tokens.Text1 = "red" }, "text_1"},
		{"bad soft color", func(p *Palette) { p.Tokens.AccentSoft = "rgba(1,2" }, "accent_soft"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := valid
			tc.mutate(&p)
			err := Validate(p)
			if err == nil || !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("Validate error = %v, want substring %q", err, tc.match)
			}
		})
	}
}

func TestValidateCatalogRejectsEmptyAndDuplicate(t *testing.T) {
	if err := ValidateCatalog(nil); err == nil {
		t.Fatal("empty catalog should be rejected")
	}
	p, _ := Lookup("light")
	if err := ValidateCatalog([]Palette{p, p}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate catalog error = %v", err)
	}
}

func TestEveryPaletteHasResolvedStateColors(t *testing.T) {
	for _, p := range List() {
		if p.Tokens.Success == "" || p.Tokens.Warning == "" || p.Tokens.Danger == "" {
			t.Errorf("theme %q has unresolved state colors: %+v", p.ID, p.Tokens)
		}
	}
}
