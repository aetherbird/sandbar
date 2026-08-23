package cliadmin

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// CompletionShell is a supported completion-script target.
type CompletionShell string

const (
	Bash CompletionShell = "bash"
	Zsh  CompletionShell = "zsh"
	Fish CompletionShell = "fish"
)

// ValueHint tells a shell how to complete a flag value or positional argument.
type ValueHint string

const (
	NoValueHint   ValueHint = ""
	FileHint      ValueHint = "file"
	DirectoryHint ValueHint = "directory"
)

// ArgumentSpec describes a positional argument or a flag's value. Choices are
// shell-independent literal values; FileHint and DirectoryHint delegate to the
// shell's native path completer.
type ArgumentSpec struct {
	Name        string
	Description string
	Choices     []string
	Hint        ValueHint
	Repeatable  bool
}

// FlagSpec describes one logical flag and all accepted spellings, such as
// []string{"-m", "--model"}. A nil Value denotes a boolean flag.
type FlagSpec struct {
	Names       []string
	Description string
	Value       *ArgumentSpec
}

// CommandSpec is an abstract command tree. PersistentFlags apply to this
// command and all descendants. Keeping this descriptor outside the generator
// lets the executable use the same registry for parsing/help/completions.
type CommandSpec struct {
	Name            string
	Aliases         []string
	Description     string
	Flags           []FlagSpec
	PersistentFlags []FlagSpec
	Arguments       []ArgumentSpec
	Subcommands     []CommandSpec
}

var flagNamePattern = regexp.MustCompile(`^--?[A-Za-z0-9][A-Za-z0-9_-]*$`)

// GenerateCompletion validates a command descriptor and emits a standalone
// completion script for bash, zsh, or fish.
func GenerateCompletion(shell CompletionShell, root CommandSpec) (string, error) {
	if err := ValidateCommandSpec(root); err != nil {
		return "", err
	}
	nodes := collectCommandNodes(root)
	switch shell {
	case Bash:
		return renderBashCompletion(root, nodes), nil
	case Zsh:
		return renderZshCompletion(root, nodes), nil
	case Fish:
		return renderFishCompletion(root, nodes), nil
	default:
		return "", fmt.Errorf("unsupported completion shell %q (want bash, zsh, or fish)", shell)
	}
}

// ValidateCommandSpec rejects ambiguous command trees and values that cannot
// be represented consistently by all three generated shell scripts.
func ValidateCommandSpec(root CommandSpec) error {
	if err := validateCommand(root, nil); err != nil {
		return err
	}
	return nil
}

type commandNode struct {
	path      string
	command   CommandSpec
	inherited []FlagSpec
}

func collectCommandNodes(root CommandSpec) []commandNode {
	var nodes []commandNode
	var walk func(CommandSpec, string, []FlagSpec)
	walk = func(command CommandSpec, path string, inherited []FlagSpec) {
		path = joinCommandPath(path, command.Name)
		activePersistent := append(append([]FlagSpec(nil), inherited...), command.PersistentFlags...)
		nodes = append(nodes, commandNode{path: path, command: command, inherited: activePersistent})
		for _, child := range command.Subcommands {
			walk(child, path, activePersistent)
		}
	}
	walk(root, "", nil)
	return nodes
}

func validateCommand(command CommandSpec, ancestors []string) error {
	path := strings.Join(append(append([]string(nil), ancestors...), command.Name), " ")
	if err := validateCommandName(command.Name); err != nil {
		return fmt.Errorf("command %q: %w", path, err)
	}
	seenNames := map[string]bool{command.Name: true}
	for _, alias := range command.Aliases {
		if err := validateCommandName(alias); err != nil {
			return fmt.Errorf("command %q alias: %w", path, err)
		}
		if seenNames[alias] {
			return fmt.Errorf("command %q has duplicate name or alias %q", path, alias)
		}
		seenNames[alias] = true
	}

	seenFlags := make(map[string]bool)
	for _, flag := range append(append([]FlagSpec(nil), command.Flags...), command.PersistentFlags...) {
		if len(flag.Names) == 0 {
			return fmt.Errorf("command %q has a flag with no names", path)
		}
		for _, name := range flag.Names {
			if !flagNamePattern.MatchString(name) {
				return fmt.Errorf("command %q has invalid flag name %q", path, name)
			}
			if seenFlags[name] {
				return fmt.Errorf("command %q has duplicate flag %q", path, name)
			}
			seenFlags[name] = true
		}
		if flag.Value != nil {
			if err := validateArgument(*flag.Value); err != nil {
				return fmt.Errorf("command %q flag %s: %w", path, flag.Names[0], err)
			}
		}
	}
	for _, argument := range command.Arguments {
		if err := validateArgument(argument); err != nil {
			return fmt.Errorf("command %q argument %q: %w", path, argument.Name, err)
		}
	}

	seenChildren := make(map[string]string)
	for _, child := range command.Subcommands {
		for _, name := range append([]string{child.Name}, child.Aliases...) {
			if prior, ok := seenChildren[name]; ok {
				return fmt.Errorf("command %q subcommand name %q is shared by %q and %q", path, name, prior, child.Name)
			}
			seenChildren[name] = child.Name
		}
		if err := validateCommand(child, append(ancestors, command.Name)); err != nil {
			return err
		}
	}
	return nil
}

func validateCommandName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.HasPrefix(name, "-") || strings.ContainsAny(name, " /|\t\r\n") {
		return fmt.Errorf("invalid name %q", name)
	}
	return nil
}

func validateArgument(argument ArgumentSpec) error {
	if argument.Hint != NoValueHint && argument.Hint != FileHint && argument.Hint != DirectoryHint {
		return fmt.Errorf("invalid value hint %q", argument.Hint)
	}
	for _, choice := range argument.Choices {
		if choice == "" || strings.IndexFunc(choice, unicode.IsSpace) >= 0 {
			return fmt.Errorf("choice %q must be non-empty and contain no whitespace", choice)
		}
	}
	return nil
}

func renderBashCompletion(root CommandSpec, nodes []commandNode) string {
	function := "_" + shellIdentifier(root.Name) + "_completion"
	var out strings.Builder
	fmt.Fprintf(&out, "# bash completion for %s; generated from the command descriptor\n", root.Name)
	fmt.Fprintf(&out, "%s() {\n", function)
	out.WriteString("  local cur prev path word expect i candidates\n")
	out.WriteString("  COMPREPLY=()\n")
	out.WriteString("  cur=\"${COMP_WORDS[COMP_CWORD]}\"\n")
	out.WriteString("  prev=\"${COMP_WORDS[COMP_CWORD-1]}\"\n")
	fmt.Fprintf(&out, "  path=%s\n", shellSingleQuote(root.Name))
	out.WriteString("  expect=0\n")
	out.WriteString("  for ((i=1; i<COMP_CWORD; i++)); do\n")
	out.WriteString("    word=\"${COMP_WORDS[i]}\"\n")
	out.WriteString("    if ((expect)); then expect=0; continue; fi\n")
	out.WriteString("    case \"$path|$word\" in\n")
	writeScanCases(&out, nodes, "      ", "expect=1", "path=")
	out.WriteString("    esac\n")
	out.WriteString("  done\n")
	out.WriteString("  case \"$path|$prev\" in\n")
	for _, node := range nodes {
		for _, flag := range flagsForNode(node) {
			if flag.Value == nil {
				continue
			}
			patterns := make([]string, 0, len(flag.Names))
			for _, name := range flag.Names {
				patterns = append(patterns, shellSingleQuote(node.path+"|"+name))
			}
			fmt.Fprintf(&out, "    %s) %s; return ;;\n", strings.Join(patterns, "|"), bashValueCompletion(*flag.Value))
		}
	}
	out.WriteString("  esac\n")
	out.WriteString("  case \"$path\" in\n")
	for _, node := range nodes {
		fmt.Fprintf(&out, "    %s) candidates=%s ;;\n", shellSingleQuote(node.path), shellSingleQuote(strings.Join(nodeCandidates(node, false), " ")))
	}
	out.WriteString("  esac\n")
	out.WriteString("  COMPREPLY=( $(compgen -W \"$candidates\" -- \"$cur\") )\n")
	out.WriteString("}\n")
	fmt.Fprintf(&out, "complete -o bashdefault -o default -F %s %s\n", function, shellSingleQuote(root.Name))
	return out.String()
}

func renderZshCompletion(root CommandSpec, nodes []commandNode) string {
	function := "_" + shellIdentifier(root.Name)
	var out strings.Builder
	fmt.Fprintf(&out, "#compdef %s\n", root.Name)
	fmt.Fprintf(&out, "# zsh completion for %s; generated from the command descriptor\n", root.Name)
	fmt.Fprintf(&out, "%s() {\n", function)
	out.WriteString("  local cur prev path word expect i\n")
	out.WriteString("  local -a candidates\n")
	out.WriteString("  cur=\"${words[CURRENT]}\"\n")
	out.WriteString("  prev=\"${words[CURRENT-1]}\"\n")
	fmt.Fprintf(&out, "  path=%s\n", shellSingleQuote(root.Name))
	out.WriteString("  expect=0\n")
	out.WriteString("  for ((i=2; i<CURRENT; i++)); do\n")
	out.WriteString("    word=\"${words[i]}\"\n")
	out.WriteString("    if ((expect)); then expect=0; continue; fi\n")
	out.WriteString("    case \"$path|$word\" in\n")
	writeScanCases(&out, nodes, "      ", "expect=1", "path=")
	out.WriteString("    esac\n")
	out.WriteString("  done\n")
	out.WriteString("  case \"$path|$prev\" in\n")
	for _, node := range nodes {
		for _, flag := range flagsForNode(node) {
			if flag.Value == nil {
				continue
			}
			patterns := make([]string, 0, len(flag.Names))
			for _, name := range flag.Names {
				patterns = append(patterns, shellSingleQuote(node.path+"|"+name))
			}
			fmt.Fprintf(&out, "    %s) %s; return ;;\n", strings.Join(patterns, "|"), zshValueCompletion(*flag.Value))
		}
	}
	out.WriteString("  esac\n")
	out.WriteString("  case \"$path\" in\n")
	for _, node := range nodes {
		fmt.Fprintf(&out, "    %s)\n", shellSingleQuote(node.path))
		out.WriteString("      candidates=(\n")
		for _, candidate := range nodeCandidates(node, true) {
			fmt.Fprintf(&out, "        %s\n", shellSingleQuote(candidate))
		}
		out.WriteString("      )\n")
		out.WriteString("      _describe 'command or option' candidates\n")
		if hint := positionalHint(node.command.Arguments); hint != NoValueHint {
			fmt.Fprintf(&out, "      if [[ \"$cur\" != -* ]]; then %s; fi\n", zshValueCompletion(ArgumentSpec{Hint: hint}))
		}
		out.WriteString("      ;;\n")
	}
	out.WriteString("  esac\n")
	out.WriteString("}\n")
	fmt.Fprintf(&out, "%s \"$@\"\n", function)
	return out.String()
}

func renderFishCompletion(root CommandSpec, nodes []commandNode) string {
	function := "__" + shellIdentifier(root.Name) + "_completion"
	var out strings.Builder
	fmt.Fprintf(&out, "# fish completion for %s; generated from the command descriptor\n", root.Name)
	fmt.Fprintf(&out, "function %s\n", function)
	out.WriteString("  set -l words (commandline -opc)\n")
	out.WriteString("  set -l current (commandline -ct)\n")
	fmt.Fprintf(&out, "  set -l path %s\n", shellSingleQuote(root.Name))
	out.WriteString("  set -l expect 0\n")
	out.WriteString("  for word in $words[2..-1]\n")
	out.WriteString("    if test $expect -eq 1; set expect 0; continue; end\n")
	out.WriteString("    switch \"$path|$word\"\n")
	writeFishScanCases(&out, nodes)
	out.WriteString("    end\n")
	out.WriteString("  end\n")
	out.WriteString("  set -l prev $words[-1]\n")
	out.WriteString("  switch \"$path|$prev\"\n")
	for _, node := range nodes {
		for _, flag := range flagsForNode(node) {
			if flag.Value == nil {
				continue
			}
			patterns := make([]string, 0, len(flag.Names))
			for _, name := range flag.Names {
				patterns = append(patterns, shellSingleQuote(node.path+"|"+name))
			}
			fmt.Fprintf(&out, "    case %s\n", strings.Join(patterns, " "))
			fmt.Fprintf(&out, "      %s\n", fishValueCompletion(*flag.Value))
			out.WriteString("      return\n")
		}
	}
	out.WriteString("  end\n")
	out.WriteString("  switch $path\n")
	for _, node := range nodes {
		fmt.Fprintf(&out, "    case %s\n", shellSingleQuote(node.path))
		for _, candidate := range fishNodeCandidates(node) {
			fmt.Fprintf(&out, "      printf '%%s\\n' %s\n", shellSingleQuote(candidate))
		}
		if hint := positionalHint(node.command.Arguments); hint != NoValueHint {
			fmt.Fprintf(&out, "      %s\n", fishValueCompletion(ArgumentSpec{Hint: hint}))
		}
	}
	out.WriteString("  end\n")
	out.WriteString("end\n")
	fmt.Fprintf(&out, "complete -c %s -f -a '(%s)'\n", shellSingleQuote(root.Name), function)
	return out.String()
}

func writeScanCases(out *strings.Builder, nodes []commandNode, indent, expectStatement, pathPrefix string) {
	for _, node := range nodes {
		for _, flag := range flagsForNode(node) {
			if flag.Value == nil {
				continue
			}
			patterns := make([]string, 0, len(flag.Names))
			for _, name := range flag.Names {
				patterns = append(patterns, shellSingleQuote(node.path+"|"+name))
			}
			fmt.Fprintf(out, "%s%s) %s ;;\n", indent, strings.Join(patterns, "|"), expectStatement)
		}
		for _, child := range node.command.Subcommands {
			childPath := joinCommandPath(node.path, child.Name)
			for _, name := range append([]string{child.Name}, child.Aliases...) {
				fmt.Fprintf(out, "%s%s) %s%s ;;\n", indent, shellSingleQuote(node.path+"|"+name), pathPrefix, shellSingleQuote(childPath))
			}
		}
	}
}

func writeFishScanCases(out *strings.Builder, nodes []commandNode) {
	for _, node := range nodes {
		for _, flag := range flagsForNode(node) {
			if flag.Value == nil {
				continue
			}
			patterns := make([]string, 0, len(flag.Names))
			for _, name := range flag.Names {
				patterns = append(patterns, shellSingleQuote(node.path+"|"+name))
			}
			fmt.Fprintf(out, "      case %s\n", strings.Join(patterns, " "))
			out.WriteString("        set expect 1\n")
		}
		for _, child := range node.command.Subcommands {
			childPath := joinCommandPath(node.path, child.Name)
			for _, name := range append([]string{child.Name}, child.Aliases...) {
				fmt.Fprintf(out, "      case %s\n", shellSingleQuote(node.path+"|"+name))
				fmt.Fprintf(out, "        set path %s\n", shellSingleQuote(childPath))
			}
		}
	}
}

func flagsForNode(node commandNode) []FlagSpec {
	flags := append(append([]FlagSpec(nil), node.inherited...), node.command.Flags...)
	seen := make(map[string]bool)
	result := make([]FlagSpec, 0, len(flags))
	for _, flag := range flags {
		filtered := flag
		filtered.Names = nil
		for _, name := range flag.Names {
			if !seen[name] {
				seen[name] = true
				filtered.Names = append(filtered.Names, name)
			}
		}
		if len(filtered.Names) > 0 {
			result = append(result, filtered)
		}
	}
	return result
}

func nodeCandidates(node commandNode, descriptions bool) []string {
	var candidates []string
	for _, flag := range flagsForNode(node) {
		for _, name := range flag.Names {
			candidates = append(candidates, describeCandidate(name, flag.Description, descriptions))
		}
	}
	for _, child := range node.command.Subcommands {
		for _, name := range append([]string{child.Name}, child.Aliases...) {
			candidates = append(candidates, describeCandidate(name, child.Description, descriptions))
		}
	}
	for _, argument := range node.command.Arguments {
		for _, choice := range argument.Choices {
			candidates = append(candidates, describeCandidate(choice, argument.Description, descriptions))
		}
	}
	sort.Strings(candidates)
	return candidates
}

func fishNodeCandidates(node commandNode) []string {
	var candidates []string
	add := func(value, description string) {
		description = strings.ReplaceAll(description, "\t", " ")
		description = strings.ReplaceAll(description, "\n", " ")
		if strings.TrimSpace(description) == "" {
			candidates = append(candidates, value)
			return
		}
		candidates = append(candidates, value+"\t"+description)
	}
	for _, flag := range flagsForNode(node) {
		for _, name := range flag.Names {
			add(name, flag.Description)
		}
	}
	for _, child := range node.command.Subcommands {
		for _, name := range append([]string{child.Name}, child.Aliases...) {
			add(name, child.Description)
		}
	}
	for _, argument := range node.command.Arguments {
		for _, choice := range argument.Choices {
			add(choice, argument.Description)
		}
	}
	sort.Strings(candidates)
	return candidates
}

func positionalHint(arguments []ArgumentSpec) ValueHint {
	for _, argument := range arguments {
		if argument.Hint != NoValueHint {
			return argument.Hint
		}
	}
	return NoValueHint
}

func describeCandidate(value, description string, enabled bool) string {
	if !enabled || strings.TrimSpace(description) == "" {
		return value
	}
	value = strings.ReplaceAll(value, ":", "\\:")
	description = strings.ReplaceAll(description, "\t", " ")
	description = strings.ReplaceAll(description, "\n", " ")
	description = strings.ReplaceAll(description, ":", "\\:")
	return value + ":" + description
}

func bashValueCompletion(argument ArgumentSpec) string {
	switch argument.Hint {
	case FileHint:
		return `COMPREPLY=( $(compgen -f -- "$cur") )`
	case DirectoryHint:
		return `COMPREPLY=( $(compgen -d -- "$cur") )`
	default:
		return fmt.Sprintf(`COMPREPLY=( $(compgen -W %s -- "$cur") )`, shellSingleQuote(strings.Join(argument.Choices, " ")))
	}
}

func zshValueCompletion(argument ArgumentSpec) string {
	switch argument.Hint {
	case FileHint:
		return "_files"
	case DirectoryHint:
		return "_files -/"
	default:
		if len(argument.Choices) == 0 {
			return "_message 'value'"
		}
		quoted := make([]string, len(argument.Choices))
		for i, choice := range argument.Choices {
			quoted[i] = shellSingleQuote(choice)
		}
		return "compadd -- " + strings.Join(quoted, " ")
	}
}

func fishValueCompletion(argument ArgumentSpec) string {
	switch argument.Hint {
	case FileHint:
		return `__fish_complete_path "$current"`
	case DirectoryHint:
		return `__fish_complete_directories "$current"`
	default:
		if len(argument.Choices) == 0 {
			return "true"
		}
		parts := make([]string, len(argument.Choices))
		for i, choice := range argument.Choices {
			parts[i] = shellSingleQuote(choice)
		}
		return "printf '%s\\n' " + strings.Join(parts, " ")
	}
}

func joinCommandPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

func shellIdentifier(value string) string {
	var out strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	return out.String()
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
