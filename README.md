# Temper

Turn your AI coding sessions into reviewed, committed skills.

Temper reads the transcripts your coding agents already write to disk — Cursor, Claude Code — and distills them into suggested skills: the conventions you keep repeating, the pitfalls your agent keeps rediscovering, the corrections you keep making. You review each suggestion against its verbatim transcript evidence, accept or reject, and Temper writes the accepted ones into the files your agents actually read: `AGENTS.md`, `CLAUDE.md` (via import), `.cursor/rules/*.mdc`, `SKILL.md`.

**There is no Temper cloud.** Transcripts are parsed locally; prepared, redacted distillation payloads go directly from your machine to the provider you configure: an HTTP endpoint, the official Codex CLI, or the official Claude Code CLI. The review dashboard is served by the binary at `localhost`.

Temper is an independent continuation of [elcruzo/autoskills](https://github.com/elcruzo/autoskills). The full Git history and original MIT copyright notice are preserved; see [NOTICE](NOTICE). This project is not affiliated with or endorsed by the original author.

## Install

```bash
go build -o temper ./cmd/temper   # from source (Go 1.25+)
```

To get the full dashboard UI embedded in the binary, build the frontend first:

```bash
cd web && bun install --frozen-lockfile && bun run build && cd ..
go build -o temper ./cmd/temper
```

## Use

```bash
# 1. configure your LLM (or use env vars)
export ANTHROPIC_API_KEY=sk-ant-...        # or OPENAI_API_KEY / TEMPER_API_KEY

# 2. see what's on this machine
temper status

# 3. distill recent sessions into suggestions
temper scan                            # --project NAME, --since 168h, --max 20, --dry-run

# 4. review: accept / edit / reject — accepted skills are written to your repos
temper review                          # opens http://127.0.0.1:4517

# always-on mode: watches transcript dirs and auto-scans seconds after a session settles
temper install-daemon                  # or run `temper daemon` manually

# maintenance
temper garden                          # LLM pass: propose amend/merge/prune → review inbox
temper verify                          # flag skills referencing missing or recently-changed paths
temper undo sg_xxx                     # revert an accepted skill (removes the artifact)
```

Config lives at `~/.temper/config.json`:

```json
{
  "provider": "http",
  "endpoint": "https://api.anthropic.com/v1",
  "api_key": "sk-ant-...",
  "model": "claude-sonnet-4-5",
  "max_suggestions_per_scan": 10,
  "min_confidence": 0.5,
  "ignore_projects": [],
  "trigger_phrase": "",
  "auto_accept_threshold": 0,
  "section_budget_bytes": 12000,
  "daemon_interval_minutes": 30
}
```

### Migration from AutoSkills

On first stateful command, Temper adopts `~/.autoskills` only when `~/.temper` does not already exist. The directory move is atomic on the same filesystem, `autoskills.db` becomes `temper.db`, and existing config, skills and backups are preserved. Migration refuses to guess when both homes exist or when SQLite `-wal`/`-shm` sidecars indicate an active or uncheckpointed legacy database; stop the old daemon and resolve the named conflict, then retry.

New configuration uses `TEMPER_PROVIDER`, `TEMPER_ENDPOINT`, `TEMPER_API_KEY` and `TEMPER_MODEL`. The corresponding `AUTOSKILLS_*` variables remain supported as lower-priority fallbacks. Existing `autoskills:` managed blocks and `autoskills-*` artifacts remain readable/removable; new writes use `temper:` markers and `temper-*` names. `install-daemon` replaces the legacy systemd or launchd label only after the Temper service starts successfully, and rolls back to the legacy service on activation failure.

`provider` accepts exactly `http`, `codex`, or `claude`. A config without this field keeps the historical `http` behavior.

HTTP uses the existing OpenAI-compatible chat-completions client:

```json
{
  "provider": "http",
  "endpoint": "https://api.openai.com/v1",
  "api_key": "...",
  "model": "..."
}
```

Codex invokes an already authenticated official CLI; Temper does not need an OpenAI API key:

```json
{
  "provider": "codex",
  "model": "..."
}
```

Install `codex`, run its normal login flow yourself, and confirm `codex login status`. Omitting `model` uses the CLI default. Each call is ephemeral, runs in a fresh temporary directory outside the scanned repository, and uses a separate temporary `CODEX_HOME` containing only the existing `auth.json`: a symbolic link when the platform permits it, otherwise a `0600` copy inside the `0700` temporary home on non-Windows systems. Windows requires symbolic-link support because Go file modes do not establish an equivalent user-only ACL there; failure is explicit rather than creating a weak credential copy. Temper never parses, logs or passes this credential in argv, and removes the temporary home after the call. This staging is necessary because changing `CODEX_HOME` otherwise disconnects the CLI from its subscription authentication. The call uses a read-only sandbox, ignores user config and rules, empties MCP configuration, disables project-instruction loading and the active filesystem/tool/plugin features observed in Codex 0.151.0, skips host skill and Git discovery, and captures only the final message. Provider-control, API-key and telemetry environment variables are removed case-insensitively before launch; subscription authentication comes only from the staged `auth.json`. Strict config parsing makes an unsupported isolation flag fail the call instead of silently weakening it. Codex still supplies its own built-in system behavior, administrator-managed configuration and authentication machinery; Temper cannot disable or audit those internals, and future CLI versions can change the meaning or completeness of these controls. The executable resolved as `codex` in `PATH` is a trusted prerequisite: Temper checks availability, not binary signatures or provenance. Codex `exec` offers one prompt stream rather than separate system/user channels, so Temper sends the prepared system and user messages as explicitly delimited sections on stdin.

Claude invokes an already authenticated official Claude Code CLI; Temper does not need an Anthropic API key and never reads Claude credential files:

```json
{
  "provider": "claude",
  "model": "..."
}
```

Install `claude`, complete its normal authentication flow yourself, and confirm `claude auth status`. Omitting `model` uses the CLI default. Each call uses print mode, a fresh temporary directory outside the scanned repository, safe mode, no tools, no slash commands and no session persistence. Temper passes the prepared system message through Claude's supported `--system-prompt-file` option and the prepared user message over stdin, keeping prompt content out of argv. API-key, alternate-cloud-provider and telemetry environment variables are removed before launch so the CLI uses its normal subscription authentication. Safe mode disables user/project customizations including `CLAUDE.md`, skills, plugins, hooks and MCP servers, but Claude documents that administrator-managed policy and built-in behavior still apply; Temper cannot inspect or disable those layers. As for Codex, the `claude` executable found in `PATH` is trusted rather than attested by Temper.

Both CLI providers request a JSON object through the CLI's structured-output option, then the existing distiller still performs strict JSON decoding, closed-field/enum validation and exact-substring evidence verification. Temper never starts a login, falls back to another provider or puts transcript content in shell commands. Apart from staging Codex `auth.json` as described above, it does not access CLI credentials. Missing executables, detectable authentication failures, timeout/cancellation, oversized output, non-zero exits, empty output and invalid structured output are explicit errors. Cancellation kills the whole CLI process group on Unix; Windows uses `taskkill /T /F` with direct-process kill as a fallback; other platforms kill the launched process and bound the wait, so descendant cleanup there remains dependent on the CLI. The isolation statements above describe verified controls of the locally inspected CLI versions, not a sandbox independently implemented or guaranteed by Temper.

Set `trigger_phrase` (e.g. `"temper this"`) to distill only sessions where you typed that phrase. `section_budget_bytes` caps the managed AGENTS.md section — on overflow, the lowest-confidence skills are demoted to on-demand skill files so always-on context never bloats ("Markdown poisoning" defense).

`auto_accept_threshold` is **deprecated and ignored**. It used to write high-confidence suggestions to disk during a scan; a model-authored file with no human in the loop is not a tuning dial, so it was removed. An existing config still loads — a non-zero value only prints a warning on `scan`. Every suggestion stays pending until you accept it in `temper review`.

With `provider: "http"`, any OpenAI-compatible endpoint works: OpenAI (`https://api.openai.com/v1`), Anthropic's compatibility endpoint, a corporate LLM gateway, or local Ollama (`http://localhost:11434/v1`, no key). The endpoint must be `https`, or `http` on loopback for a local model — a remote plaintext URL, or one carrying credentials in the URL, is refused before any request is built.

## How it decides what to suggest

The distiller hunts for five signal types — user corrections, repeated rediscovery, failure→fix sequences, stated conventions, repeatable workflows — and is heavily quality-gated: most sessions produce zero suggestions, every suggestion must carry verbatim transcript evidence (validated by exact substring match), credential-shaped strings are redacted before any LLM call, and suggestions deduplicate against your existing rules files and prior suggestions. A cheap deterministic pre-filter runs first: a session with none of those markers is skipped before any LLM call, so the model is only spent on transcripts that actually contain signal.

Accepted skills are placed deliberately (always-on rule vs path-scoped rule vs on-demand skill). In `AGENTS.md` they live inside a single managed section, grouped so related skills read together instead of accumulating chronologically:

```markdown
<!-- temper:section:begin -->
## Agent skills (temper)

### Conventions
<!-- temper:begin id=sg_xxx group=conventions -->
#### Use pnpm, never npm — preinstall hook redirects
...
<!-- temper:end id=sg_xxx -->

### Commands & workflows
### Pitfalls
<!-- temper:section:end -->
```

Re-accepting updates a block in place, groups stay sorted, and your hand-written content outside the section is never touched. Skill bodies are written for machines: fenced commands, exact paths, tables — not prose.

## What leaves your machine

- Redacted transcript excerpts → your configured HTTP endpoint, Codex CLI, or Claude Code CLI
- Nothing → us. There is no telemetry, no account, no server in v0.1.

Every dynamic byte in a provider request — transcript turns, your `AGENTS.md`/`CLAUDE.md` context, `.cursor/rules`, managed blocks read by `garden`, project metadata — goes through one preparation step that redacts credential shapes and identity data (API keys, bearer tokens, private keys, connection strings, cloud tokens, `.env` assignments, internal URLs, email addresses) and neutralizes control markers, then redacts the assembled payload once more. A transcript is data: it cannot close its own delimiter, choose a placement, set a status, or cause a write. Provider redirects are refused instead of replaying credentials to a destination you did not configure.

Accepted skills are written as Markdown only. A shell fence inside a skill body stays a fence — Temper never emits an executable file.

## Platform support

| Platform | Core (scan/review/garden/verify) | Background service |
|---|---|---|
| macOS | supported (primary dev platform) | `install-daemon` → launchd |
| Linux | supported (binary cross-compiles CGO-free; amd64 + arm64) | `install-daemon` → systemd user unit |
| Windows | local build, vet and Go suite pass (`%USERPROFILE%\.cursor`, `%USERPROFILE%\.claude`); CI coverage is still missing | run `temper daemon` manually / Task Scheduler (native service on the roadmap) |

WSL note: transcripts written by Windows-side tools live under `/mnt/c/Users/<you>/...` — point `CURSOR_DIR`-style overrides there (first-class WSL detection is on the roadmap).

## Repo layout

- `cmd/temper` — CLI (scan / review / status)
- `internal/` — collectors (Cursor, Claude Code), distiller, writers, local API server
- `web/` — review dashboard (Vite + React + SCSS), embedded into the binary

The marketing site and the Mintlify documentation are maintained in separate repositories.

## License

MIT
