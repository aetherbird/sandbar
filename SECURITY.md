# Security Policy

## Reporting a vulnerability

Please open a **private security advisory** on the GitHub repository
("Security" → "Report a vulnerability") rather than filing a public issue.
Include reproduction steps, affected versions, and impact. Reports are
acknowledged and triaged as they arrive; fixes ship in a patch release once
validated.

## Scope

In scope: the Sandbar CLI, its tool implementations, approval policy
enforcement, the workspace jail, the SQLite store, and anything else shipped
in this repository.

Out of scope: your API keys and provider credentials, the behavior of
upstream LLM providers, and compromise of the machine you run Sandbar on.

## The security model, in brief

There is no software sandbox for `shell_exec` — the agent runs with the
literal OS privileges of the user who launched it. What Sandbar provides is
policy and hygiene, and it fails closed:

- **Tiered approvals.** Every tool is classified `read`, `write`, or `exec`.
  The approval mode (`yolo` / `write` / `always-ask`) plus per-tool and
  per-tier rules decide allow/prompt/deny. When policy says "prompt" and no
  interactive handler exists (headless runs), the call is **denied**, never
  silently allowed.
- **Workspace jail.** File operations and dynamic shell commands resolve to
  the configured workspace root; traversal attempts and escapes are rejected.
  This is convenience hygiene, not a security boundary.
- **SSH guards.** Remote shell execution rejects option-injection-shaped
  hosts, applies the command blocklist remotely, and supports an exact host
  allowlist.
- **Local-only data.** Nothing leaves your machine except traffic to the
  providers you configure; there is no telemetry.

If you need isolation from the agent's full privileges, run the binary inside
a container or OS-level sandbox of your choosing.
