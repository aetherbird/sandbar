# Sandbar — Design

Sandbar is a standalone terminal AI coding-agent harness in Go: a streaming
REPL, a tiered-approval tool system, and a SQLite thread store, all in one
local binary with no server component.

---

## 1. Vision

A lightweight, single-user agentic harness for local and cloud language
models. It runs entirely in your terminal: the agent reasons, plans, and
executes tools (file, shell, git, search, web) on your behalf, with every
conversation persisted locally in SQLite. Not a platform, not a service, not
multi-user.

---

## 2. Goals

- Chat with any OpenAI-compatible model endpoint — OpenRouter cloud, local
  Ollama, Gemini free-tier, OpenAI direct.
- Provider-flexible native tool-calling: emit `tools` array for any model that
  advertises `supports_tools: true`.
- Tiered-approval tool execution (files, shell, git, web search).
- Streaming responses and reasoning paths in real-time.
- Single-binary deployment with minimal dependencies.
- Scriptable: a stable newline-delimited JSON event stream (`--json`).

## 3. Non-Goals

- Multi-user authentication or RBAC.
- Plugin marketplace or third-party extensions.
- Advanced RAG / vector database.
- Voice input.
- A server component, web UI, or remote mode.

---

## 4. Architecture

Sandbar is a **single local binary**. There is no server to deploy, no daemon,
and no network listener: the CLI runs the agent engine in-process, opens the
SQLite database directly, and talks to LLM providers over HTTPS from your
machine.

```
┌────────────────────────────────────────────┐
│              sandbar (CLI/TUI)             │
│                                            │
│  ┌──────────────────────────────────────┐  │
│  │      cmd/sandbar (bubbletea REPL)       │  │
│  └──────────────┬───────────────────────┘  │
│                 │ Backend interface        │
│  ┌──────────────▼───────────────────────┐  │
│  │      backend.LocalBackend            │  │
│  │  - Agent Engine (internal/agent)     │  │
│  │  - LLM Client    (internal/llm)      │  │
│  │  - Tool Router    (internal/tools)   │  │
│  │  - SQLite Memory  (internal/memory)  │  │
│  └──────────────────────────────────────┘  │
└──────────────────┬─────────────────────────┘
                   │ HTTPS (OpenAI-compatible)
                   ▼
            LLM providers
```

**The Backend seam:** `internal/backend` defines the `Backend` interface — the
stable contract between the CLI front end and the agent runtime. One
implementation ships, `LocalBackend`, which runs the agent in-process against
the local store and config. Optional capability interfaces (`ModelsProvider`,
`MessageQueuer`, `TodoLister`, `PlanDecider`) are type-asserted by callers
that need them, keeping the base interface small.

---

## 5. Tech Stack

| Layer | Choice | Rationale |
|---|---|---|
| Language | Go 1.25+ | Single binary, fast, easy deployment, great concurrency |
| TUI Framework | `github.com/charmbracelet/bubbletea` | Rich inline REPL experience in the terminal |
| LLM Client | `github.com/sashabaranov/go-openai` | Mature, OpenAI-compatible, supports tools + streaming |
| Database | SQLite (`modernc.org/sqlite`) | Pure-Go CGO-free, zero-config, portable; enforced WAL mode. **Critical:** this choice is what enables the `GOOS=… GOARCH=… go build` cross-compile matrix — do NOT swap in `mattn/go-sqlite3`, which is CGO and breaks static cross-compile. |
| Tokenizer | `github.com/pkoukk/tiktoken-go` + offline loader | Real BPE accounting with an embedded vocabulary; never HTTP-fetches encodings at runtime. Falls back to a chars÷4 heuristic only if tokenizer init fails. |
| Config | YAML (`gopkg.in/yaml.v3`) | Human-readable model profiles |
| Git Ops | CLI Native (`git` binary wrapper) | Avoid go-git overhead; exploit robust local Git CLI via `os/exec` |
| Content search | ripgrep when on PATH, pure-Go walker otherwise | `rg` is a soft dependency; a built-in fallback reproduces its observable behavior so the tool never hard-fails without it |
| Web search | Brave Search API with DuckDuckGo HTML fallback | Brave key required for primary; fallback needs no API key |

---

## 6. Directory Structure

```
sandbar/
├── cmd/sandbar/                 # Terminal UI (the only binary)
│   ├── main.go
│   └── commands.go              # slash-command registry + dispatch
├── internal/
│   ├── agent/           # Core reasoning loop (ordered safe concurrency)
│   │   └── agent.go
│   ├── backend/         # Backend seam; LocalBackend implementation
│   │   └── backend.go
│   ├── llm/             # OpenAI-compatible client, <think> parser,
│   │   │                #   stream events, token accounting
│   │   ├── client.go
│   │   ├── models.go
│   │   ├── stream.go
│   │   └── tokens.go
│   ├── tools/           # Tool implementations + approval policy
│   │   ├── registry.go
│   │   ├── approval.go
│   │   ├── file.go / file_mutation.go / patch.go
│   │   ├── shell.go / jobs.go / ssh.go
│   │   ├── git.go / gitdispatch.go
│   │   ├── search.go / websearch.go / webfetch.go
│   │   ├── delegate.go / todo.go
│   │   ├── imagegen.go / vision.go
│   │   └── sysproc_unix.go / sysproc_windows.go  # platform shims
│   ├── memory/          # Chat history, threads, FTS5 search (WAL)
│   │   ├── store.go
│   │   ├── thread.go
│   │   ├── search.go
│   │   ├── compression.go
│   │   └── plan.go
│   ├── persona/         # System prompts, skills discovery
│   │   └── persona.go
│   ├── config/          # YAML loader, path resolution, zero-config boot
│   │   ├── config.go
│   │   ├── resolve.go
│   │   ├── zerocfg.go
│   │   └── client.go
│   ├── cliadmin/        # `sandbar admin` subcommands (doctor, config…)
│   ├── cliui/           # Shared CLI rendering (wrap, markdown)
│   └── ui/theme/        # Theme palettes
├── migrations/          # SQLite schema (embedded)
├── tests/fixtures/      # Integration test fixtures
├── docs/                # This document
├── README.md
├── LICENSE
├── config.yaml.example  # Commented configuration template
├── system-prompt.md     # Default persona prompt
├── go.mod / go.sum
└── Makefile
```

---

## 7. Core Modules

### 7.1 LLM Client (`internal/llm`)

- **Provider abstraction:** Each provider has a `base_url` and optional API key.
- **Model resolution:** Aliases are provider-qualified (`provider/rest/of/alias`). When the first `/`-delimited segment matches a provider name, the remainder resolves within that provider only; otherwise all providers are searched in config order and the first match wins. `ListModels` returns every alias provider-qualified and sorted.
- **Tool support:** If `supports_tools: true` in config, send native `tools` array; otherwise the agent falls back to parsing tool calls from assistant text. `model_defaults` inherit per provider; model-level values win.
- **Streaming & Reasoning Parsing:** Use SSE (`stream: true`). The stream analyzer extracts reasoning streams enclosed in `<think>...</think>` blocks and separates them from final answer tokens.
- **Context window:** Enforce per-model `context_length` with a real BPE tokenizer (tiktoken, offline-embedded vocabulary; chars÷4 only as a last-resort fallback). Reserve a 20% headroom buffer below `context_length` for the model's response.
  - When over budget, turn-start compression summarizes older context with an auxiliary LLM call (configurable target ratio, min/max summary tokens, secret redaction). The summary is persisted to SQLite and reused across turns. Group-aware boundary alignment ensures assistant `tool_calls` are never split from their `tool` results, and the latest user turn is always preserved.
  - Mid-loop compression runs only after every tool result in the current assistant call group is complete. It first prunes oversized old tool outputs; if still over budget, it replaces completed current-turn work with one transient active-turn checkpoint while preserving the current user request. Raw SQLite history is retained.
  - Fallback truncation drops whole semantic groups, never an assistant/tool-result fragment. If even the system prompt plus current user request cannot fit, the turn stops with an explicit unsafe-payload error. Compression triggers, summaries, usage, fallbacks, and errors are all observable; silent history loss is forbidden.

### 7.2 Agent Engine (`internal/agent`)

The agent runs a **ReAct loop**:

```
User Message
    │
    ▼
[Agent Loop]
    │
    ├──► Build prompt (system + history + user + tool schemas)
    │
    ├──► Stream LLM response & reasoning blocks (<think>...</think>)
    │
    ├──► If response contains tool_calls:
    │        Run an all-independent batch concurrently (bounded)
    │        Otherwise execute the whole batch sequentially
    │        Append every result in provider/source order
    │        Loop again
    │
    └──► If response is final text:
             Stream to user
             Save to memory
```

**Ordered Safe-Concurrency Strategy (Critical Guard):**
Each tool explicitly opts into parallel execution through registry metadata.
Sandbar runs a batch concurrently only when every call is read-only or isolated;
concurrency is bounded to eight workers. A batch containing shell, Git, todo,
file mutation, image generation, an unknown tool, or any other unannotated call
runs entirely in strict provider order. Results from a concurrent batch are
collected by input index and emitted and persisted in provider order, so the
OpenAI tool-call/result transcript remains deterministic and valid. This keeps
dependent cascades such as `[file_write]` followed by `[shell_exec]` race-free
while allowing independent reads, searches, fetches, vision calls, and
delegations to overlap.

**Iteration policy:** `max_turns: 0` permits unlimited distinct tool work; a
positive value is an explicit operator cap. Independently, Sandbar warns on
the third consecutive identical tool-call group and stops after the sixth if
the model does not change course. This semantic no-progress guard prevents a
runaway loop without imposing an arbitrary limit on long agentic tasks.

Tool arguments are validated against each tool's top-level required fields
before dispatch. Action-dependent contracts such as `todo` also validate their
item shape and semantics in the handler, returning an actionable error instead
of treating a malformed mutation as a successful no-op.

**Subagents:** `delegate_task` runs a self-contained briefing in a fresh agent
instance with its own (optionally different) model, and `resume_task`
continues an interrupted delegation by its durable task ID. Independent
delegations sent in one batch run concurrently. Subagent registries share the
parent's job supervisor so background work is torn down with the thread.

### 7.3 Tool System (`internal/tools`)

All tools are defined with JSON Schema and registered in a `Registry` (a
`Tool` carries Name, Description, Schema, a `ParallelSafe` flag, approval
`Metadata`, and an `Execute` func).

**Registry.Execute is the single policy and dispatch choke point.** It
resolves effective arguments (including per-argument approval-tier
escalation, e.g. `git add` counts as write even though `git status` is read),
enforces required fields and schema validation, evaluates approval policy,
optionally prompts, and only then invokes the handler. Plan mode is enforced
here too: write/exec-tier calls are denied at dispatch while a plan is being
drafted.

**SHA-256 write preconditions:** every mutating file tool (`file_write`,
`file_append`, `file_patch`) requires `expected_sha256` — the digest the model
observed when it last read the file, or the `ExpectedFileAbsent` sentinel for
creation. The mutation applies atomically only if the file's current digest
still matches; otherwise the call fails with a conflict error telling the model
to re-read and retry. `file_read` always returns the full-content SHA-256 even
when display bytes are capped, so the precondition is always obtainable. This
makes lost-update races between the model's view and disk impossible to commit
silently.

**Tool Result Round-Trip (OpenAI-compatible):**
Tool results return to the model as a `role: tool` message carrying the
original `tool_call_id` emitted by the assistant turn. The agent loop appends
results in the exact order tools were called.

**Output Truncation Policy:**
- Hard cap: 32 KiB per tool result (UTF-8 bytes).
- Over-cap output is truncated and suffixed with: `\n\n[...truncated N bytes; full output not shown to model...]`
- `file_read` additionally honors a soft per-call `max_bytes` arg (default 32 KiB, max 256 KiB) — caller can request more for explicit large reads.
- `shell_exec` truncates stdout and stderr **independently** at 16 KiB each, so a chatty stderr can't crowd out useful stdout.
- Binary / non-UTF-8 output from `file_read` and `shell_exec` is detected (invalid UTF-8 sequences) and replaced with `[binary output suppressed: N bytes]` rather than corrupting the JSON stream.

**Toolset (15 tools):**

| Tool | Key inputs | Output | Tier |
|---|---|---|---|
| `file_read` | `path`, `max_bytes?` | Contents + full-content SHA-256 | read |
| `file_write` | `path`, `content`, `expected_sha256` | Success / conflict | write |
| `file_append` | `path`, `content`, `expected_sha256` | Success / conflict | write |
| `file_patch` | `path`, `old_str`, `new_str`, `expected_sha256` | Success / conflict | write |
| `shell_exec` | `command`, `host?`, `timeout_seconds?`, `async?` | stdout + stderr + exit code | exec |
| `job` | `action`, `job_id?`, `max_bytes?`, `timeout_seconds?` | Managed background-job control | exec |
| `git` | `action` (status/diff/add/commit), `repo_path?`, `staged?`, `paths?`, `message?` | Git text output | read → exec by action |
| `web_search` | `query` | List of results | read |
| `search_files` | `pattern`, `path?`, `target?`, `file_glob?`, `limit?` | Regex content / filename matches | read |
| `web_fetch` | `url`, `max_chars?` | Extracted readable text | read |
| `todo` | `action`, `items?` | Durable per-thread task list | metadata (plan-exempt) |
| `delegate_task` | `goal`, `context?` | Sub-agent summary | exec |
| `resume_task` | `task_id` | Resumed sub-agent summary | exec |
| `image_generate` | `prompt` | Generated image (jailed output path) | exec |
| `vision_analyze` | `image_path` | Provider vision analysis | exec (external send) |

File tools are jailed to the workspace root directory; `git add` requires an
explicit non-empty `paths` array (no blanket `git add .`); `git commit` fails
fast if nothing is staged.

**Blocked shell commands (default):** `rm -rf /`, `chmod 777`; the list is
configurable. Entries are matched against the **command word** of each
pipeline/chain segment (tokenized, honoring quotes), never as a raw substring
of the command line: a single-word entry blocks that command wherever it
appears as a command; a multi-word entry (e.g. `rm -rf /`) matches a segment
whose leading tokens equal the entry, so it blocks a recursive delete of root
but not `rm -rf /tmp/x`. This avoids the false-positive class where a `sudo`
entry used to block `which sudo`, paths containing "sudo", etc. Dynamic shell
commands that escape the mapped workspace are also rejected (environment
injection, path hacks).

**Remote shell execution:** `shell_exec` accepts an optional `host` argument.
When set, the harness owns the ssh transport and POSIX quoting: the command is
single-quote-wrapped as `bash -c '<command>'` and passed to `ssh` as a single
argv element (no local shell layer), so the model passes a plain command and
never composes nested ssh/python/shell quoting. The blocklist applies to the
remote command exactly as it does locally. Background remote jobs (`host` +
`async:true`) are rejected. The `tools.ssh` config bounds the connect phase
(`connect_timeout`, default 5s), keeps ssh non-interactive (`batch_mode`,
default true), and can restrict targets via `allowed_hosts` (empty = any
host). Hosts beginning with `-` are rejected (ssh option-injection guard).

### 7.4 Memory (`internal/memory`)

- **Thread model:** Each conversation is a `Thread` with UUID, title, creation
  time, workspace, and message list. Messages carry a per-thread monotonic
  `seq`; assistant tool calls live in a separate `tool_calls` table keyed by
  the provider's call ID. Schema is committed in `migrations/` and applied in
  order on open; later migrations add compressions, subagent tasks, per-thread
  todo plans, and plan-mode state.
- **Storage:** SQLite (`modernc.org/sqlite`), opened with
  `journal_mode=WAL; synchronous=NORMAL; foreign_keys=ON; busy_timeout=5000`
  and connection-local pragmas repeated on write connections.
- **Full-text search:** message content is indexed into an FTS5 virtual table
  (`messages_fts`) kept in sync via triggers; `/search` runs bm25-ranked
  MATCH queries with user input sanitized for FTS5 syntax.
- **Auto-title:** After the first user message + assistant response, a
  background goroutine asks the LLM for a 5-word title; the UPDATE is
  conditional (`WHERE title IS NULL`) to avoid clobbering a manual rename.
- **Fork:** `/fork` copies a thread (messages, tool calls with remapped IDs
  and result references, plans) into a new thread ID inside one transaction,
  rolling back completely if any step fails. The source thread is untouched.

### 7.5 Persona (`internal/persona`)

- **System prompt template:** Configurable in `config.yaml` — preferred form is `persona.system_prompt_file` pointing at a Markdown file (source of truth: `system-prompt.md`; the file wins over any inline `system_prompt`).
- **Dynamic injection:** Persona injects an environment block (workspace, git-repo, platform, UTC date at day resolution for prefix-cache friendliness, model) and workspace project context (`.sandbar.md`, `AGENTS.md`) into the system prompt before each request. The prompt is model-agnostic — no per-family guidance is injected.
- **Default fallback persona** (when neither is set): "You are Sandbar, a precise agentic assistant. You help the user get real work done: inspect, build, repair, explain, document, and carry the task forward…" (compact form; see `internal/config/config.go`).
- **Skills:** `.sandbar/skills/<name>/SKILL.md` in the workspace is advertised by name + `description:` line in a `# Skills` prompt section (capped at 20); the model reads the full SKILL.md with `file_read` only when a task matches. No restart or config change needed to add or edit a skill.
- **Reasoning effort:** `--effort low|medium|high` (CLI) or `/effort` (session) sets the per-turn OpenAI-style `reasoning_effort`; empty means provider default. Not persisted — each turn may differ.
- **Plan mode:** `--plan` / `/plan` runs a turn read-only: a PLAN MODE directive is appended to the system prompt, and write/exec-tier tool calls are denied at dispatch (after per-argument tier resolution, so e.g. `git add` is blocked while `git status` passes). The resulting plan is stored durably and can be approved or rejected from the session picker. Context files are trusted as the operator's own instructions; no heuristic injection scan is applied to them.

---

## 8. Event Schema

Every turn is surfaced as a stream of `llm.StreamEvent` values. In `--json`
mode these are emitted as newline-delimited JSON (JSONL) — one event object
per line — which is the stable, machine-readable contract for scripting and
benchmarking. The `type` field is the discriminator; remaining fields are
populated per type and omitted when empty.

```go
type StreamEvent struct {
    Type    string `json:"type"` // thread, user_message, thinking, token,
                                 // tool_call, tool_result, approval_required,
                                 // error, done, usage, auxiliary_usage,
                                 // compression_start, compression_end,
                                 // compression_error, subagent_*
    Content string `json:"content,omitempty"`

    ToolCallID string          `json:"tool_call_id,omitempty"`
    ToolName   string          `json:"tool_name,omitempty"`
    Arguments  json.RawMessage `json:"arguments,omitempty"`

    TaskID     string `json:"task_id,omitempty"`     // subagent lifecycle
    TaskGoal   string `json:"task_goal,omitempty"`
    TaskStatus string `json:"task_status,omitempty"`

    Approval *ApprovalEvent `json:"approval,omitempty"` // interactive approval request

    PromptTokens     int    `json:"prompt_tokens,omitempty"`
    CompletionTokens int    `json:"completion_tokens,omitempty"`
    TotalTokens      int    `json:"total_tokens,omitempty"`
    UsagePurpose     string `json:"usage_purpose,omitempty"` // main | compression | title | subagent

    Compression *CompressionEvent `json:"compression,omitempty"`

    ThreadID string `json:"thread_id,omitempty"` // on "thread" and "done"
}
```

**Example JSONL stream:**

```json
{"type":"thread","thread_id":"0b2f…"}
{"type":"user_message","content":"Read main.go and summarize it"}
{"type":"thinking","content":"Locating main.go in the workspace…"}
{"type":"tool_call","tool_call_id":"call_1","tool_name":"file_read","arguments":{"path":"main.go"}}
{"type":"tool_result","tool_call_id":"call_1","tool_name":"file_read","content":"package main…"}
{"type":"token","content":"This"}
{"type":"token","content":" file"}
{"type":"usage","prompt_tokens":1834,"completion_tokens":96,"total_tokens":1930,"usage_purpose":"main"}
{"type":"done","thread_id":"0b2f…"}
```

---

## 9. CLI Specification

```
sandbar [flags] [message]

Flags:
  --model                 Model alias (default: client.yaml default_model, else first available)
  --workspace             Workspace directory (default: current directory)
  --config                Config path (default: search path, see §11)
  --thread                Resume thread by ID (--resume is an alias)
  --json                  Emit newline-delimited JSON events (scripting/benchmarking)
  --summarize-context     Summarize a JSON message batch from stdin without
                          running an agent turn (requires --json and --model)
  --plan                  Plan mode: read-only investigation; present a plan
                          instead of changing anything
  --effort                Reasoning effort for this run: low, medium, or high
  --tools                 Restrict this run to these tools, comma-separated;
                          the rest are not advertised to the model
  --disable-subagents     Omit delegate_task and resume_task entirely
  --theme / --list-themes CLI theme override / list theme IDs
  --color                 Color output: auto, always, or never

Admin subcommands (before flag parsing):
  sandbar doctor | config | completion …
```

**Modes:**
- **Interactive TUI:** `sandbar` (no args) → launches an inline Bubble Tea REPL (no alt-screen): the transcript streams into native terminal scrollback above a persistent input block with a live status bar (model, context gauge, session timer). Slash commands (with autocomplete):
  `/model`, `/effort`, `/plan`, `/theme`, `/sessions`, `/resume`, `/new`, `/delete`, `/title`, `/fork`, `/compress`, `/undo`, `/search`, `/clear`, `/noformat`, `/redraw`, `/help`, `/quit`.
  - `! <command>` is a shell escape — runs the command in the workspace and shows its output, cancelable with Ctrl+C.
  - `@path` mentions expand the referenced file's content into the message before sending.
  - Renders raw thinking tokens inside a distinct, low-contrast italic section located directly above the final response.
- **One-shot Standalone:** `sandbar [message]` → prints streaming response to stdout; add `--json` for the event stream.
- **Shell Pipeline Integration:** standard Unix piping:
  ```bash
  cat source_code.go | sandbar "Summarize this logic block"
  ```
  Stdin detection uses `os.Stdin.Stat()` (no cgo/isatty dependency). When piped, the full stdin buffer is appended to the user message (separator: two newlines + a fenced code block).

**Themes:** palettes include light, dark, monochrome, Catppuccin Latte/Mocha,
Tokyo Night (and variants), Rosé Pine (and variants), Gruvbox, Dracula, One
Dark, and more; selection precedence is `--theme` > `SANDBAR_THEME` >
`client.yaml` preference > system light/dark. `NO_COLOR` and `--color never`
disable color.

---

## 10. Configuration

### Resolution

Config is located by precedence — the working directory is deliberately
**not** searched, so running sandbar from a random folder can't silently load
a stray `config.yaml`:

1. explicit `--config <path>`
2. `$SANDBAR_CONFIG`
3. first existing fixed location:
   - `$XDG_CONFIG_HOME/sandbar/config.yaml`
   - `~/.config/sandbar/config.yaml`
   - `/etc/sandbar/config.yaml`

**Zero-config boot:** with no config file at all, a single OpenAI provider is
synthesized from the environment when `OPENAI_API_KEY` is set (`OPENAI_BASE_URL`
and `OPENAI_MODEL` override the endpoint and alias; defaults
`https://api.openai.com/v1` and `gpt-4o-mini`). On first zero-config boot the
commented `config.yaml.example` template is copied to the user config location
(never overwriting an existing file).

The SQLite database path resolves from the config's `database` value: absolute
paths are used as-is; relative values land under the user data dir
(`$XDG_DATA_HOME` or `~/.local/share/sandbar`), never next to the config file.

A separate per-user client config — `~/.config/sandbar/client.yaml` — holds
TUI preferences only (`default_model`, `theme`, `color_mode`, `font_size`) and
is auto-created with commented defaults on first run.

### Schema

```yaml
workspace: "./workspace"   # file ops are jailed to this directory
database:  "./sandbar.db"  # SQLite (pure-Go modernc.org/sqlite);
                           #   relative paths resolve under ~/.local/share/sandbar

persona:
  name: "Sandbar"
  # Persona prompt lives in its own Markdown file next to the config
  # (source of truth: system-prompt.md; relative paths resolve against
  # the config's directory, and the file wins over inline system_prompt).
  system_prompt_file: "system-prompt.md"

# ============================================================
# Providers — each block is an OpenAI-compatible API endpoint
# ============================================================
providers:
  # --- Cloud / OpenRouter ---
  - name: openrouter-direct
    base_url: "https://openrouter.ai/api/v1"
    api_key: "${OPENROUTER_API_KEY}"
    models:
      google/gemini-3.1-flash-lite:
        context_length: 262144
    model_defaults:
      supports_tools: true

  # --- Local Ollama ---
  - name: local-ollama
    base_url: "http://localhost:11434/v1"
    api_key: ""
    models:
      gemma3:27b:            { context_length: 262144 }
      qwen3:32b:             { context_length: 262144 }
    model_defaults:
      supports_tools: true

# ============================================================
# Tool configuration
# ============================================================
tools:
  # Approval modes, from least to most restrictive:
  # yolo = allow all; write = prompt for exec; always-ask = prompt for write+exec.
  # Missing tool metadata is classified as exec. Exact per-tool rules override
  # tier rules, and both override the selected mode.
  approval:
    mode: "yolo"
    rules: {}               # e.g. {shell_exec: prompt, image_generate: deny}
    tiers: {}               # e.g. {write: prompt, exec: deny}
  shell:
    timeout: "30s"
    blocked_commands:
      - "rm -rf /"
      - "chmod 777"
    jobs:
      max_jobs: 128
      max_running: 16
      output_bytes: 65536
      retention: "30m"
      termination_grace: "750ms"
  ssh:
    connect_timeout: 5s   # ssh -o ConnectTimeout for the host argument
    batch_mode: true      # ssh -o BatchMode=yes (no interactive prompts)
    allowed_hosts: []     # empty = any host; else exact allowlist
  web_search:
    engine: "brave"     # primary; falls back to DuckDuckGo HTML scrape if
    brave_api_key: ""   #   the Brave key is empty/unreachable

compression:
  enabled: true
  threshold: 0.80       # fraction of context that triggers compression
  target_ratio: 0.20
  min_summary_tokens: 1000
  max_summary_tokens: 12000
  model: ""             # empty = use current model; set a fast alias instead

subagent:
  model: ""             # empty = default sub-agent model; set a fast alias
  max_turns: 30         # cap on delegated sub-agent turns; 0 = unlimited
```

Environment variables referenced as `${VAR}` in YAML values are interpolated
at load. `SANDBAR_CONFIG` (config path) and `SANDBAR_THEME` (theme override)
are the harness's own env knobs.

---

## 11. Security Model

- **Absolute System Permissions Doctrine (No Sandbox Illusion):**
  There is no secure software sandbox for `shell_exec` inside Sandbar. Path hygiene, the workspace jail, and the blocked-command list exist purely as accidental-mitigation safeguards. The agent runs with the literal, absolute OS privileges of the user who launched it.
  - Use in untrusted contexts **must** be wrapped in a dedicated containerized namespace (e.g., Docker, gVisor, rootless Podman) or OS sandboxing.
- **Tiered approvals:** every tool carries an access tier — `read`, `write`, or `exec` — resolvable per argument (so `git add` escalates while `git status` stays read). The approval mode (`yolo` / `write` / `always-ask`) derives a default allow/prompt decision per tier; exact per-tool `rules` and per-tier `tiers` entries override the mode. Approving a `delegate_task`/`resume_task` grants the delegated run as one broad boundary: mode-derived child prompts are not repeated, while explicit deny/prompt rules remain in force inside subagents.
- **Fail-closed approvals:** when policy says "prompt" but no interactive approval handler exists (headless/scripted runs), the call is **denied**, never silently allowed (`ErrApprovalUnavailable`).
- **Plan mode:** read-only turns — write/exec-tier dispatch is refused while planning.
- **Workspace jail:** all file path targets resolve via deep absolute evaluation. Path conversions verify that resolved paths do not escape the designated workspace directory (blocking path traversals like `..`). Image generation writes and vision reads are jailed the same way.
- **Shell timeout:** 30 seconds default, configurable. Hard OS kill signal sent on timeout expiration.
- **SSH guards:** remote `shell_exec` rejects hosts beginning with `-` (option injection), enforces the command blocklist remotely, and can be pinned to an exact host allowlist.
- **Local-only data:** threads, messages, and summaries live in the local SQLite database under `~/.local/share/sandbar`. Nothing is telemetry'd anywhere; the only outbound traffic is to the LLM/search providers you configure.

---

## 12. Distribution

Sandbar is a single static binary. There is no server component, no
service to install, and no deployment topology: run the binary on your
machine, point it at providers, work.

- Build: `make build` from the repo root (CGO_ENABLED=0, stripped).
- Supported platforms via platform shims (`sysproc_unix.go` /
  `sysproc_windows.go`): Linux, macOS, Windows, FreeBSD — each on amd64 and
  arm64. The pure-Go SQLite driver is what keeps the full cross-compile
  matrix a plain `GOOS=… GOARCH=… go build` with no C toolchain.
- Everything else the harness needs at runtime (token vocabulary, search
  fallback) is embedded or pure Go: `rg` is optional, tiktoken never phones
  home.

---

## 13. Finalized Decisions

1. **CLI stdin piping:** one-shot mode detects a pipe via `os.Stdin.Stat()` and appends stdin to the message.
2. **Thinking rendering:** raw `<think>` chunks are parsed, streamed, and rendered in a distinct low-contrast section.
3. **Git operations:** native `git` CLI via `os/exec` (no go-git); explicit staging, no blanket `git add .`.
4. **Parallel execution:** only all-independent batches via explicit tool metadata, max eight workers; result emission and persistence always follow provider order.
5. **SQLite:** WAL + `synchronous=NORMAL` + `foreign_keys=ON`; schema in `migrations/`, applied in order.
6. **Tool results:** OpenAI `role: tool` round-trip; 32 KiB hard cap per result (16 KiB each for shell stdout/stderr); binary output suppressed.
7. **SHA-256 write preconditions:** mutating file tools require the digest observed at last read; conflicts fail loudly.
8. **Token accounting:** real BPE tokenizer (offline vocabulary); 20% headroom; group-safe compression; silent history loss forbidden.
9. **Web search:** Brave API primary, DuckDuckGo HTML fallback.
10. **Content search:** ripgrep when present, pure-Go walker otherwise.
11. **SQLite driver:** `modernc.org/sqlite` only — CGO drivers break static cross-compile.

---

## Lineage

Sandbar is a public fork of an in-house harness that also carried a web SPA,
a multi-host server deployment, and a remote CLI mode. This fork removes all
of that: what remains is the local CLI, its tool system, and the design
decisions documented here.
