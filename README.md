# Sandbar

Sandbar is a standalone terminal AI coding-agent harness in Go: a streaming
REPL backed by any OpenAI-compatible model endpoint — cloud (OpenRouter,
OpenAI, Gemini) or local (Ollama, llama.cpp, vLLM) — with 15 built-in tools
behind tiered approvals, a SQLite thread store with full-text search,
automatic context compression, subagents, plan mode, themes, and a `--json`
event stream for scripting. It boots with zero configuration from
`OPENAI_API_KEY`, ships as a single static binary, and has no server
component and no telemetry: everything runs and stays on your machine.

## Features

- **Streaming REPL** — inline Bubble Tea interface (no alt-screen) with live
  reasoning display, context gauge, and session timer.
- **15 tools, tiered approvals** — file read/write/append/patch, shell (local
  or SSH), background jobs, git, web search, content search, web fetch, todo
  tracking, subagent delegation/resume, image generation, vision analysis.
  Every tool is classified `read`/`write`/`exec`; approve per tier, per tool,
  or per session. Approvals fail closed in headless runs.
- **SHA-256 write preconditions** — mutating file tools require the digest
  observed at last read, so conflicting writes fail loudly instead of
  silently overwriting.
- **Cost rollups** — usage events are priced against an embedded
  models.dev catalog snapshot (fully offline); the status bar and one-shot
  footer show cumulative spend, hidden for unknown or free models.
- **Read schemes** — `file_read` resolves `pr://<n>`, `issue://<n>` (GitHub
  via your `gh` CLI) and `agent://<task-id>` (persisted subagent
  transcripts) before touching the filesystem.
- **SQLite thread store** — every conversation persisted locally with WAL
  journaling, FTS5 full-text search (`/search`), session resume, forking, and
  undo.
- **Context auto-compression** — real BPE token counting (offline-embedded
  vocabulary), summarization with group-aware boundaries that never split
  tool calls from results, and observable fallbacks. No silent history loss.
- **Subagents** — delegate self-contained subtasks (`delegate_task`), resume
  interrupted ones (`resume_task`); independent delegations run concurrently.
- **Plan mode** — `--plan` / `/plan` runs a read-only turn that produces a
  plan you approve before anything changes.
- **Themes** — light/dark/monochrome plus Catppuccin, Tokyo Night, Rosé Pine,
  Gruvbox, Dracula, and more; `NO_COLOR` respected.
- **`--json` scripting mode** — newline-delimited `StreamEvent` stream for
  scripts and benchmark harnesses; pipe stdin in, events out.
- **Workspace jail** — file operations resolve to the configured workspace
  root; path traversal and workspace-escaping shell commands are rejected.
- **Zero-config boot** — `OPENAI_API_KEY` alone is enough to start; a
  commented config template is written for you on first run.
- **Single static binary** — pure-Go SQLite, CGO disabled, cross-compiles to
  linux/darwin/windows/freebsd on amd64/arm64.

## Quick Start

The fastest path needs no config file at all:

```bash
export OPENAI_API_KEY=sk-...
sandbar
```

That synthesizes an OpenAI provider from the environment (`OPENAI_BASE_URL`
and `OPENAI_MODEL` override the endpoint and model alias) and writes a
commented `config.yaml` template to `~/.config/sandbar/config.yaml` for you
to edit later. See `config.yaml.example` for the full annotated schema:

```yaml
workspace: "./workspace"          # file ops are jailed to this directory
database:  "sandbar.db"           # resolves under ~/.local/share/sandbar

providers:
  - name: openrouter-direct
    base_url: "https://openrouter.ai/api/v1"
    api_key: "${OPENROUTER_API_KEY}"
    models:
      google/gemini-3.1-flash-lite:
        context_length: 262144
    model_defaults:
      supports_tools: true

tools:
  approval:
    mode: "yolo"                  # yolo | write | always-ask

compression:
  enabled: true
  threshold: 0.80
```

### Install

**Prebuilt binary** — via the install script:

```bash
curl -fsSL https://raw.githubusercontent.com/aetherbird/sandbar/main/install.sh | bash
```

The script detects your platform, downloads the release archive, verifies it
against the published sha256 checksums, and installs to `~/.local/bin`
(override with `BIN_DIR`; pin a release with `SANDBAR_VERSION=v0.3.0`).
Prebuilt binaries are not published yet — the install script and pinned
versions will work once a goreleaser release ships (see
[docs/RELEASE.md](docs/RELEASE.md)). Build from source or `go install` in
the meantime.

**Build from source** (Go 1.25+):

```bash
git clone https://github.com/aetherbird/sandbar.git
cd sandbar
make build        # produces ./sandbar (static, stripped, version-stamped)
make install      # installs to ~/.local/bin/sandbar
```

`make build` stamps the binary with `git describe --tags --always --dirty`;
run `sandbar version` to see it.

**`go install`** — `go install github.com/aetherbird/sandbar/cmd/sandbar@latest`
installs the binary as `sandbar` (module root is the repo root); the binary
reports the module version of the tag it was installed from — see
`sandbar version`.

**Homebrew / Scoop** — planned after the first release (tap and bucket
generated by goreleaser).

## Configuration

- **Config path** — first of: `--config <path>`, `$SANDBAR_CONFIG`,
  `$XDG_CONFIG_HOME/sandbar/config.yaml`, `~/.config/sandbar/config.yaml`,
  `/etc/sandbar/config.yaml`. The working directory is never searched.
- **Env vars** — `SANDBAR_CONFIG` (config path), `SANDBAR_THEME` (theme
  override); provider keys are interpolated into YAML as `${VAR}` (e.g.
  `${OPENROUTER_API_KEY}`, `${BRAVE_API_KEY}`). `OPENAI_API_KEY` alone boots
  the zero-config default.
- **Client prefs** — `~/.config/sandbar/client.yaml` holds TUI-only
  preferences (`default_model`, `theme`, `color_mode`, `font_size`,
  `show_cost` — opt-in session-cost display), auto-created with
  commented defaults on first run.
- **Data** — the SQLite database lives under `~/.local/share/sandbar/`
  (absolute `database:` values are honored as-is).
- **models.json** — a legacy-style provider registry layered on top of
  config.yaml providers (see `models.json.example`). Sandbar looks for
  `models_json:` in the config, then `models.json` next to the loaded config
  file. Schema: `{"providers": {name: {baseUrl, api, apiKey, compat, models[]}}}`
  with model entries `{id, name, modelId, contextWindow, maxTokens}` and
  `compat` quirks (`supportsDeveloperRole`, `supportsReasoningEffort`,
  `maxTokensField`, `requiresToolResultName`,
  `requiresAssistantAfterToolResult`, `thinkingFormat`, `sendSessionId`).
  Keys resolve as `$ENV`/`${ENV}` (unset → empty), `!command` (shell stdout,
  trimmed), or literal. On a provider-name clash models.json wins (the YAML
  provider is replaced, not an error); JSON providers are appended after the
  YAML ones. Importing a legacy file that lacks `supports_tools` defaults
  imported models to tool support. The zero-config env boot ignores
  models.json. `api: "anthropic-messages"` routes to the native Anthropic
  Messages wire client. Compat quirks currently honored: `maxTokensField`,
  `requiresToolResultName`, and `thinkingFormat` (mapped onto the reasoning
  dialect); `supportsDeveloperRole`, `supportsReasoningEffort`,
  `requiresAssistantAfterToolResult`, and `sendSessionId` are parsed but not
  yet applied.

### Skills & Templates

- **Skills** — on-demand instruction packs discovered from
  `<workspace>/.sandbar/skills`, `.claude/skills`, and `.agents/skills`
  (then `~/.config/sandbar/skills`, `~/.claude/skills`, `~/.agents/skills`;
  earlier scopes shadow later ones by name). Each is a folder with a
  `SKILL.md` carrying a `description:` header; the system prompt advertises
  the list and the model reads the file only when relevant.
- **Prompt templates** — markdown files in `<workspace>/.sandbar/prompts`
  or `~/.config/sandbar/prompts` become slash commands: `/name args`
  expands the body (`$1..$9`, `$@`/`$ARGUMENTS`, `${@}`, `${@:N}`,
  `${@:N:L}`) and submits it as your message. Registered commands win over
  same-named templates.
- **Prompt files** — `SYSTEM.md` replaces the base persona instructions
  (everything else in the prompt still assembles around it),
  `APPEND_SYSTEM.md` appends at the end, and `TITLE_SYSTEM.md` templates
  the session title from the first message (all support `{{cwd}}`,
  `{{date}}`; the title file also `{{message}}`, `{{firstLine}}`). Looked
  up per file in `<workspace>/.sandbar`, `.claude`, `.codex`, `.agents`,
  then `~/.config/sandbar`, `~/.claude`, `~/.codex`, `~/.agents` — first
  existing wins; no ancestor walk.

## Daily Use

| Key | What it does |
|---|---|
| `/model` | Switch model (picker) |
| `/sessions` | List and resume past sessions |
| `/resume [id]` | Resume a session by id or unique prefix (picker without argument) |
| `/new` | Start a fresh thread |
| `/delete` | Delete the current thread (two-step: `/delete confirm`) |
| `/title <text>` | Set the current session's title |
| `/fork` (`/branch`) | Branch the current session |
| `/compress` (`/compact`) | Compress context now |
| `/undo` | Remove the last exchange |
| `/search <query>` | Full-text search past conversations |
| `/clear` | Clear the screen and start fresh |
| `/noformat` | Re-print the last response as raw text |
| `/redraw` | Repaint (recover from render drift) |
| `/effort <level>` | Set reasoning effort: low \| medium \| high \| default |
| `/plan` | Toggle plan mode (read-only turn that produces a plan) |
| `/theme` | Switch theme (picker or id) |
| `/help` (`/?`) | Command reference |
| `/quit` (`/q`, `/exit`) | Exit |
| `! <command>` | Shell escape — run a command in the workspace |
| `@path` | Mention a file; its content is expanded into the message |

**Editing.** `file_read` stamps every line with an 8-hex content hash; paste
those hash-prefixed lines into `file_patch`'s `old_str` to anchor the edit —
stale anchors are rejected with the current hashes instead of silently
patching the wrong lines.

Pipe input for one-shot use:

```bash
cat main.go | sandbar "explain this file"
sandbar --json "list the failing tests" | jq -r 'select(.type=="token") | .content'
```

## Privacy & Security

- **All local.** Threads, messages, and summaries live in SQLite at
  `~/.local/share/sandbar`. There is no telemetry, no crash reporting, and no
  server component. Outbound traffic goes only to the LLM/search providers
  you configure.
- **Fail-closed approvals.** When approval policy requires a prompt but no
  interactive handler exists (headless/scripted runs), the tool call is
  denied — never silently allowed.
- **Workspace jail.** File tools and dynamic shell commands are confined to
  the configured workspace; path traversal and workspace escapes are
  rejected. Note this is convenience hygiene, not a sandbox: the agent runs
  with your full OS privileges. Run it inside a container or OS sandbox if
  the context is untrusted. See `SECURITY.md`.

## Development

```bash
make fmt          # go fmt ./... (CI also enforces gofmt -l is empty)
make test         # go test -race -count=1 -skip TestFullTuiPipeline ./...
make build        # CGO_ENABLED=0 static build
go vet ./...
```

Layout:

```
cmd/sandbar/           REPL entry point (the only binary)
internal/agent/        reasoning loop, subagents, steering
internal/backend/      Backend seam (LocalBackend)
internal/catalog/      embedded models.dev pricing snapshot
internal/cliadmin/     admin subcommands (doctor, config)
internal/cliui/        shared CLI rendering
internal/config/       YAML config, resolution, zero-config boot
internal/llm/          OpenAI-compatible client, stream events, tokens
internal/mcp/          Model Context Protocol clients
internal/memory/       SQLite store, FTS5 search, compression
internal/persona/      system prompt assembly, skills
internal/testutil/     test helpers
internal/tools/        tools, registry, approvals, jobs, ssh
internal/ui/theme/     palettes
migrations/            SQLite schema
tests/fixtures/        test fixtures
docs/DESIGN.md         full design document
go.mod / go.sum        module github.com/aetherbird/sandbar
Makefile               build/test targets
.goreleaser.yaml       release pipeline
install.sh             curl-pipeable installer
config.yaml.example    commented configuration template
models.json.example    legacy-style provider registry example
system-prompt.md       default persona prompt
```

## License

MIT — see [LICENSE](LICENSE).

---

Sandbar is forked from an in-house harness. Inspired by [pi](https://github.com/badlogic/pi-mono),
[opencode](https://opencode.ai), and Claude Code.
