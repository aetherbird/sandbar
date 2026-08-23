package agent

import (
	"encoding/json"
	"html"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/sashabaranov/go-openai"
)

// textToolCallSeq is a process-wide counter that ensures text-parsed tool-call
// IDs are globally unique. Without this, IDs like "file_read_0" collide as DB
// primary keys across different assistant messages, causing INSERT failures.
var textToolCallSeq uint64

// dsmlSep is the fullwidth vertical line character used in DeepSeek DSML format.
const dsmlSep = "\uff5c"

var (
	xmlToolCall  = regexp.MustCompile(`<function_call>\s*(\{.*?\})\s*</function_call>`)
	xmlToolCalls = regexp.MustCompile(`<tool_calls>\s*(\{.*?\})\s*</tool_calls>`)
	toolIdentRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// toolNameAliases maps DSML/generic tool names to Sandbar's actual tool names.
var toolNameAliases = map[string]string{
	"read":         "file_read",
	"read_file":    "file_read",
	"bash":         "shell_exec",
	"execute":      "shell_exec",
	"execute_bash": "shell_exec",
	"run":          "shell_exec",
	"write":        "file_write",
	"write_file":   "file_write",
	"search":       "search_files",
	"grep":         "search_files",
	"find":         "search_files",
	"list_dir":     "search_files",
	"list_files":   "search_files",
	"web_search":   "web_search",
	"web_fetch":    "web_fetch",
	"fetch":        "web_fetch",
	"shell_exec":   "shell_exec",
	"file_read":    "file_read",
	"file_write":   "file_write",
	"file_append":  "file_append",
	"file_patch":   "file_patch",
	"git_status":   "git_status",
	"git_diff":     "git_diff",
	"git_add":      "git_add",
	"git_commit":   "git_commit",
	"search_files": "search_files",
	"todo":         "todo",
	"todo_write":   "todo",
	"diff":         "diff",
	"patch":        "file_patch",
}

// registeredTextToolArgs is the subset of Sandbar's registry contract that a
// text-embedded tool call may address. Keeping an argument allowlist prevents
// wrapper metadata such as "tool" and "arguments" (or invented internal
// fields such as "workspace") from crossing into normal tool execution.
// Native structured calls remain the preferred path and are unaffected.
var registeredTextToolArgs = map[string]map[string]bool{
	"file_read":      {"path": true, "max_bytes": true},
	"file_write":     {"path": true, "content": true, "expected_sha256": true},
	"file_append":    {"path": true, "content": true, "expected_sha256": true},
	"file_patch":     {"path": true, "old_str": true, "new_str": true, "expected_sha256": true},
	"shell_exec":     {"command": true, "timeout_seconds": true, "async": true},
	"job":            {"action": true, "job_id": true, "max_bytes": true, "timeout_seconds": true},
	"git":            {"action": true, "repo_path": true, "staged": true, "paths": true, "message": true},
	"web_search":     {"query": true},
	"search_files":   {"pattern": true, "path": true, "target": true, "file_glob": true, "limit": true},
	"web_fetch":      {"url": true, "max_chars": true},
	"todo":           {"action": true, "items": true},
	"delegate_task":  {"goal": true, "context": true},
	"resume_task":    {"task_id": true},
	"image_generate": {"prompt": true},
	"vision_analyze": {"image_path": true},
}

func resolveToolName(name string) string {
	if canonical, ok := toolNameAliases[name]; ok {
		return canonical
	}
	return name
}

func parseTextToolCalls(content string) []openai.ToolCall {
	if content == "" {
		return nil
	}
	if tcs := parseDSML(content); len(tcs) > 0 {
		return tcs
	}
	if tcs := parseFunctionEquals(content); len(tcs) > 0 {
		return tcs
	}
	if tcs := parseFunctionCallsInvoke(content); len(tcs) > 0 {
		return tcs
	}
	if tcs := parseXMLToolCalls(content); len(tcs) > 0 {
		return tcs
	}
	return nil
}

// parseFunctionCallsInvoke handles the XML-like dialect observed from Claude
// Haiku through OpenRouter:
//
//	<function_calls>
//	<invoke name="bash">
//	<parameter name="tool">bash</parameter>
//	<parameter name="arguments">
//	<parameter name="command">cat ...</parameter>
//	</invoke>
//	</function_calls>
//
// The outer "arguments" parameter is sometimes not closed, so encoding/xml
// cannot parse the model output. This scanner deliberately accepts only the
// exact wrappers above, known tool names, identifier-shaped parameter names,
// and parameters declared by the corresponding Sandbar tool. Parameter text
// is XML/HTML-unescaped before being encoded as a JSON argument object.
func parseFunctionCallsInvoke(content string) []openai.ToolCall {
	const (
		wrapperStart = "<function_calls>"
		wrapperEnd   = "</function_calls>"
		invokeStart  = "<invoke name=\""
		invokeEnd    = "</invoke>"
		paramStart   = "<parameter name=\""
		paramEnd     = "</parameter>"
	)

	var tcs []openai.ToolCall
	callSeq := 0
	remaining := content
	for {
		wrapperAt := strings.Index(remaining, wrapperStart)
		if wrapperAt < 0 {
			break
		}
		wrapperBody := remaining[wrapperAt+len(wrapperStart):]
		wrapperClose := strings.Index(wrapperBody, wrapperEnd)
		if wrapperClose < 0 {
			break
		}
		block := wrapperBody[:wrapperClose]
		remaining = wrapperBody[wrapperClose+len(wrapperEnd):]

		for {
			invokeAt := strings.Index(block, invokeStart)
			if invokeAt < 0 {
				break
			}
			afterName := block[invokeAt+len(invokeStart):]
			nameEnd := strings.IndexByte(afterName, '"')
			if nameEnd < 0 {
				break
			}
			rawName := strings.TrimSpace(afterName[:nameEnd])
			afterQuote := afterName[nameEnd+1:]
			tagEnd := strings.IndexByte(afterQuote, '>')
			if tagEnd < 0 {
				break
			}
			// Only whitespace may occur between the name quote and closing >.
			// This keeps the accepted dialect narrow and predictable.
			if strings.TrimSpace(afterQuote[:tagEnd]) != "" {
				block = afterQuote[tagEnd+1:]
				continue
			}
			invokeBodyAndRest := afterQuote[tagEnd+1:]
			invokeClose := strings.Index(invokeBodyAndRest, invokeEnd)
			if invokeClose < 0 {
				break
			}
			invokeBody := invokeBodyAndRest[:invokeClose]
			block = invokeBodyAndRest[invokeClose+len(invokeEnd):]

			if !toolIdentRE.MatchString(rawName) {
				continue
			}
			toolName := resolveToolName(rawName)
			allowedArgs, registered := registeredTextToolArgs[toolName]
			if !registered {
				continue
			}

			// Require the observed wrapper metadata and make it agree with the
			// invoke attribute after alias normalization. This prevents a stray
			// prose-like <invoke> block from becoming executable.
			metadataTool, ok := quotedParameterValue(invokeBody, "tool")
			if !ok {
				continue
			}
			metadataTool = strings.TrimSpace(html.UnescapeString(metadataTool))
			if !toolIdentRE.MatchString(metadataTool) || resolveToolName(metadataTool) != toolName {
				continue
			}

			argumentsAt := strings.Index(invokeBody, paramStart+"arguments\">")
			if argumentsAt < 0 {
				continue
			}
			argumentsBody := invokeBody[argumentsAt+len(paramStart+"arguments\">"):]
			params := make(map[string]interface{})
			for {
				paramAt := strings.Index(argumentsBody, paramStart)
				if paramAt < 0 {
					break
				}
				afterParamName := argumentsBody[paramAt+len(paramStart):]
				paramNameEnd := strings.IndexByte(afterParamName, '"')
				if paramNameEnd < 0 {
					break
				}
				paramName := strings.TrimSpace(afterParamName[:paramNameEnd])
				afterParamQuote := afterParamName[paramNameEnd+1:]
				paramTagEnd := strings.IndexByte(afterParamQuote, '>')
				if paramTagEnd < 0 {
					break
				}
				if strings.TrimSpace(afterParamQuote[:paramTagEnd]) != "" {
					argumentsBody = afterParamQuote[paramTagEnd+1:]
					continue
				}
				valueAndRest := afterParamQuote[paramTagEnd+1:]
				valueEnd := strings.Index(valueAndRest, paramEnd)
				if valueEnd < 0 {
					break
				}
				argumentsBody = valueAndRest[valueEnd+len(paramEnd):]
				if !toolIdentRE.MatchString(paramName) || !allowedArgs[paramName] {
					continue
				}
				value := strings.TrimSpace(html.UnescapeString(valueAndRest[:valueEnd]))
				params[paramName] = coerceTextToolArgument(paramName, value)
			}

			if len(params) == 0 {
				continue
			}
			argsJSON, err := json.Marshal(params)
			if err != nil {
				continue
			}
			tcs = append(tcs, openai.ToolCall{
				ID:   uniqueTextCallID(toolName, callSeq),
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      toolName,
					Arguments: string(argsJSON),
				},
			})
			callSeq++
		}
	}
	return tcs
}

func coerceTextToolArgument(name, value string) interface{} {
	switch name {
	case "async", "staged":
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
	case "max_bytes", "max_chars", "limit":
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	case "timeout_seconds":
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed
		}
	}
	return value
}

func quotedParameterValue(body, name string) (string, bool) {
	start := `<parameter name="` + name + `">`
	at := strings.Index(body, start)
	if at < 0 {
		return "", false
	}
	value := body[at+len(start):]
	end := strings.Index(value, "</parameter>")
	if end < 0 {
		return "", false
	}
	return value[:end], true
}

// parseDSML handles DeepSeek DSML variants:
//
//	<DSML｜invoke name="tool">...params...</DSML｜invoke>
//	<｜invoke name="tool">...params...</｜invoke>
func parseDSML(content string) []openai.ToolCall {
	type pattern struct{ invokeStart, invokeEnd, paramTag, paramEnd string }
	patterns := []pattern{
		{"<DSML" + dsmlSep + "invoke name=\"", "</DSML" + dsmlSep + "invoke>",
			"<DSML" + dsmlSep + "parameter name=\"", "</DSML" + dsmlSep + "parameter>"},
		{"<" + dsmlSep + "invoke name=\"", "</" + dsmlSep + "invoke>",
			"<" + dsmlSep + "parameter name=\"", "</" + dsmlSep + "parameter>"},
	}

	for _, pat := range patterns {
		if tcs := parseDSMLPattern(content, pat); len(tcs) > 0 {
			return tcs
		}
	}
	return nil
}

// parseFunctionEquals handles the nested-XML tool-call format used by some
// open-weights models:
//
//	<tool_call>
//	<function=file_read>
//	<parameter=path>/some/file.go</parameter>
//	</function>
//	</tool_call>
//
// The <tool_call> wrapper is optional. Multiple calls may appear consecutively.
// The format uses <function=NAME> and <parameter=KEY>VALUE</parameter> — no
// quotes, name/key directly after '='.
func parseFunctionEquals(content string) []openai.ToolCall {
	const (
		funcStart  = "<function="
		funcEnd    = "</function>"
		paramStart = "<parameter="
		paramEnd   = "</parameter>"
	)

	var tcs []openai.ToolCall
	callSeq := 0

	blocks := strings.Split(content, funcStart)
	for _, block := range blocks[1:] {
		// Tool name is everything up to the first '>'.
		nameEnd := strings.Index(block, ">")
		if nameEnd < 0 {
			continue
		}
		toolName := resolveToolName(strings.TrimSpace(block[:nameEnd]))

		// Body is up to funcEnd (or rest of block if no closing tag).
		body := block
		if endBlock := strings.Index(block, funcEnd); endBlock >= 0 {
			body = block[:endBlock]
		}

		// Extract <parameter=KEY>VALUE</parameter> pairs.
		params := make(map[string]string)
		searchFrom := 0
		for {
			idx := strings.Index(body[searchFrom:], paramStart)
			if idx < 0 {
				break
			}
			idx += searchFrom + len(paramStart)

			// Parameter key is up to the first '>'.
			keyEnd := strings.Index(body[idx:], ">")
			if keyEnd < 0 {
				break
			}
			paramKey := body[idx : idx+keyEnd]

			valStart := idx + keyEnd + 1
			valEnd := strings.Index(body[valStart:], paramEnd)
			if valEnd < 0 {
				break
			}
			paramVal := body[valStart : valStart+valEnd]
			params[paramKey] = strings.TrimSpace(paramVal)
			searchFrom = valStart + valEnd + len(paramEnd)
		}

		// Skip blocks with no parameters (likely a false positive from
		// a stray "<function=" in regular text).
		if len(params) == 0 {
			continue
		}

		argsJSON, _ := json.Marshal(params)
		callID := uniqueTextCallID(toolName, callSeq)
		callSeq++
		tcs = append(tcs, openai.ToolCall{
			ID:   callID,
			Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{
				Name:      toolName,
				Arguments: string(argsJSON),
			},
		})
	}
	return tcs
}

func parseDSMLPattern(content string, pat struct{ invokeStart, invokeEnd, paramTag, paramEnd string }) []openai.ToolCall {
	var tcs []openai.ToolCall
	callSeq := 0
	blocks := strings.Split(content, pat.invokeStart)
	for _, block := range blocks[1:] {
		endQuote := strings.Index(block, "\"")
		if endQuote < 0 {
			continue
		}
		toolName := resolveToolName(block[:endQuote])

		endBlock := strings.Index(block, pat.invokeEnd)
		body := block
		if endBlock >= 0 {
			body = block[:endBlock]
		}

		params := make(map[string]string)
		searchFrom := 0
		for {
			idx := strings.Index(body[searchFrom:], pat.paramTag)
			if idx < 0 {
				break
			}
			idx += searchFrom + len(pat.paramTag)
			nameEnd := strings.Index(body[idx:], "\"")
			if nameEnd < 0 {
				break
			}
			paramName := body[idx : idx+nameEnd]
			valStart := strings.Index(body[idx+nameEnd:], ">")
			if valStart < 0 {
				break
			}
			valStart += idx + nameEnd + 1
			valEnd := strings.Index(body[valStart:], pat.paramEnd)
			if valEnd < 0 {
				break
			}
			paramVal := body[valStart : valStart+valEnd]
			params[paramName] = strings.TrimSpace(paramVal)
			searchFrom = valStart + valEnd + len(pat.paramEnd)
		}

		argsJSON, _ := json.Marshal(params)
		callID := uniqueTextCallID(toolName, callSeq)
		callSeq++
		tcs = append(tcs, openai.ToolCall{
			ID:   callID,
			Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{
				Name:      toolName,
				Arguments: string(argsJSON),
			},
		})
	}
	return tcs
}

func parseXMLToolCalls(content string) []openai.ToolCall {
	var tcs []openai.ToolCall
	callSeq := 0
	for _, pat := range []*regexp.Regexp{xmlToolCall, xmlToolCalls} {
		for _, m := range pat.FindAllStringSubmatch(content, -1) {
			if len(m) < 2 {
				continue
			}
			var tc struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(m[1]), &tc); err != nil {
				continue
			}
			argsJSON, _ := json.Marshal(tc.Arguments)
			tc.Name = resolveToolName(tc.Name)
			callID := uniqueTextCallID(tc.Name, callSeq)
			callSeq++
			tcs = append(tcs, openai.ToolCall{
				ID:   callID,
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      tc.Name,
					Arguments: string(argsJSON),
				},
			})
		}
		if len(tcs) > 0 {
			break
		}
	}
	return tcs
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// uniqueTextCallID generates a globally-unique synthetic tool-call ID for
// text-parsed (non-native) tool calls. The localSeq distinguishes multiple
// calls within the same parse; the atomic counter prevents DB primary-key
// collisions across different assistant messages.
func uniqueTextCallID(toolName string, localSeq int) string {
	n := atomic.AddUint64(&textToolCallSeq, 1)
	return toolName + "_" + itoa(localSeq) + "_t" + itoa(int(n))
}
