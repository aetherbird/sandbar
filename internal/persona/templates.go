package persona

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Prompt templates are markdown files behind ad-hoc slash commands:
//
//	.sandbar/prompts/<name>.md           workspace
//	~/.config/sandbar/prompts/<name>.md  user
//
// Typing "/name args" in the REPL (when no registered command claims the
// name) expands the template body with the args and submits it as the user
// message. Bodies take positional dollar-arguments; the frontmatter carries
// a `description` and an `argument-hint`.

// Template is one discovered markdown prompt template.
type Template struct {
	Name        string
	Description string
	ArgHint     string // frontmatter "argument-hint": usage help for the dollar-args
	Body        string // file content with frontmatter stripped
}

// DiscoverTemplates lists the *.md templates for a workspace and the user
// config (name = filename minus .md; dotfiles and directories are ignored).
// Duplicate names are de-duplicated with the workspace winning over user
// scope, and the result is sorted by name. Missing directories are not an
// error — an empty list means no templates.
func DiscoverTemplates(workspace string) []Template {
	dirs := []string{
		filepath.Join(workspace, ".sandbar", "prompts"),
		filepath.Join(UserConfigDir(), "prompts"),
	}
	var out []Template
	seen := map[string]bool{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil // unreadable scope: fail closed rather than half-list
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			t := parseTemplate(string(data), strings.TrimSuffix(name, ".md"))
			if seen[t.Name] {
				continue
			}
			seen[t.Name] = true
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FindTemplate returns the template with the exact name, if any.
func FindTemplate(templates []Template, name string) (Template, bool) {
	for _, t := range templates {
		if t.Name == name {
			return t, true
		}
	}
	return Template{}, false
}

// parseTemplate builds a Template from file content. The description falls
// back to the first non-empty body line when frontmatter omits it; the body
// loses its frontmatter block and leading blank lines.
func parseTemplate(content, name string) Template {
	fields, body := splitFrontmatter(content)
	body = strings.TrimLeft(body, "\r\n")
	desc := fields["description"]
	if desc == "" {
		for _, line := range strings.Split(body, "\n") {
			if line = strings.TrimSpace(strings.TrimRight(line, "\r")); line != "" {
				desc = line
				break
			}
		}
	}
	return Template{
		Name:        name,
		Description: desc,
		ArgHint:     fields["argument-hint"],
		Body:        body,
	}
}

// ExpandTemplate renders the template body with the whitespace-separated
// args substituted:
//
//	$1 .. $9             the Nth argument (empty when absent)
//	$@  $ARGUMENTS       all arguments, space-joined
//	${@} ${ARGUMENTS}    the same, brace forms
//	${@:N}               the arguments from the Nth on
//	${@:N:L}             L arguments starting at the Nth
//
// Any other dollar form is left verbatim.
func ExpandTemplate(t Template, args string) string {
	parts := strings.Fields(args)
	body := t.Body
	all := strings.Join(parts, " ")
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '$' || i+1 >= len(body) {
			b.WriteByte(c)
			continue
		}
		switch n := body[i+1]; {
		case n >= '1' && n <= '9':
			if int(n-'0') <= len(parts) {
				b.WriteString(parts[n-'1'])
			}
			i++
		case n == '@':
			b.WriteString(all)
			i++
		case strings.HasPrefix(body[i+1:], "ARGUMENTS") && !isWordByte(at(body, i+10)):
			b.WriteString(all)
			i += len("ARGUMENTS")
		case n == '{':
			if end := strings.IndexByte(body[i+2:], '}'); end >= 0 {
				inner := body[i+2 : i+2+end]
				if v, ok := expandBrace(inner, parts, all); ok {
					b.WriteString(v)
				} else {
					b.WriteString(body[i : i+3+end]) // unknown form: verbatim
				}
				i += 2 + end // loop increment consumes the '}'
			} else {
				b.WriteString(body[i:]) // unterminated: the rest is verbatim
				return b.String()
			}
		default:
			b.WriteByte('$')
		}
	}
	return b.String()
}

// expandBrace expands the inner text of a ${...} form. ok is false for
// unknown forms, which ExpandTemplate leaves verbatim. N is 1-based and must
// parse as a decimal >= 1; L must be >= 0 and clamps to the remaining args.
func expandBrace(inner string, args []string, all string) (string, bool) {
	if inner == "@" || inner == "ARGUMENTS" {
		return all, true
	}
	rest, ok := strings.CutPrefix(inner, "@:")
	if !ok {
		return "", false
	}
	nStr, lStr, hasL := strings.Cut(rest, ":")
	n, err := strconv.Atoi(nStr)
	if err != nil || n < 1 {
		return "", false
	}
	lo := min(n-1, len(args))
	if !hasL {
		return strings.Join(args[lo:], " "), true
	}
	l, err := strconv.Atoi(lStr)
	if err != nil || l < 0 {
		return "", false
	}
	return strings.Join(args[lo:min(lo+l, len(args))], " "), true
}

// at returns s[i] or 0 when i is out of range.
func at(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

// isWordByte reports whether c may continue an identifier-ish token.
func isWordByte(c byte) bool {
	return c == '_' || c == '-' ||
		(c >= '0' && c <= '9') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= 'a' && c <= 'z')
}

// splitFrontmatter splits a markdown document into its YAML-ish frontmatter
// fields and body. Frontmatter is the block between two leading "---" lines;
// a minimal hand parser recognizes only single "key: value" lines (no
// nesting, folding, or multi-line values), tolerates CRLF line endings and a
// leading UTF-8 BOM, and strips one matching pair of surrounding quotes from
// values. Files without frontmatter (or with an unterminated block) yield
// nil fields and the full content.
func splitFrontmatter(content string) (fields map[string]string, body string) {
	content = strings.TrimPrefix(content, "\ufeff")
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return nil, content
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			return parseFieldLines(lines[1:i]), strings.Join(lines[i+1:], "\n")
		}
	}
	return nil, content
}

// parseFieldLines reduces frontmatter lines to key→value pairs. Comment
// lines and indented (nested) lines are ignored; a key must be a single
// bare word. Later duplicate keys win, matching usual mapping semantics.
func parseFieldLines(lines []string) map[string]string {
	fields := make(map[string]string)
	for _, raw := range lines {
		if strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t") {
			continue // nested YAML: out of scope for the minimal parser
		}
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, " \t") {
			continue
		}
		fields[key] = unquote(strings.TrimSpace(value))
	}
	return fields
}

// unquote strips one matching pair of surrounding single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
