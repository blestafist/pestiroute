# Development Tooling Adoption

This plan covers tools for **building PestiRoute with OpenCode**, not the gateway's client-facing MCP support or its backend Connectors. Stages refer to [ROADMAP.md](ROADMAP.md); a milestone name is a decision checkpoint, not an installation date. Add a tool only when the named work needs it and verify it against the installed OpenCode version first. Keep project configuration small; developer-specific credentials and optional tools belong in personal configuration.

| When / trigger | Tool or practice | What to do and acceptance signal |
| --- | --- | --- |
| M3 protected operations and acceptance | Existing `AGENTS.md`, project agents, atomic task cards, Context7 and official gopls MCP | See [CURRENT](CURRENT.md) and [TASKS](TASKS.md#m3--access-accounts-and-accounting) for the active task and current blocker. Use the [LOCAL-M3 procedure](LOCAL-M3.md), pinned SQLite/YAML documentation and existing checks; assign one READY card per worker. |
| M0 scaffold, then ongoing | Go toolchain and CI, built-in OpenCode file/shell tools | Pin Go in the scaffold; establish `gofmt`, vet, tests, race checks, and build in the task/CI. These checks provide evidence before introducing another code-navigation MCP. `CURRENT.md` records commands only after they work. |
| When PR work becomes routine | Git CLI and GitHub integration already available to the developer | Use Git for scoped local diffs/commits and GitHub integration for PR review/CI where useful. Keep credentials out of the project config. No additional Git MCP is needed just to edit files or open a PR. |
| M2 onward, only if navigation remains a measured bottleneck | Serena (candidate code-navigation MCP) | Evaluate only if targeted gopls and source searches leave a concrete gap. Check supported Go indexing, OpenCode v2 connection/tool exposure, context overhead, and additional useful references. Keep it personal/disabled by default; no installation now. |
| When shell output repeatedly wastes context | Personal RTK plugin or a small output filter | A local RTK plugin is loaded on the inspected machine (see inventory below). Compare raw and filtered results on test failures, review diffs, and streaming diagnostics before project adoption. Verify exit status and decisive error context; plugin discovery alone does not prove those properties. |
| M2–M4, when contract and protocol checks recur | Small project-local commands or skills (candidate) | Extract a repeatable checklist only after it has been used successfully on real tasks, for example contract conformance or fixture review. Prefer links to the owning docs over copied rules. Add no blanket skills package now; Ponytail guidance already lives in the worker agent. |
| M3 protected operations | Optional SQLite CLI and deterministic file-backed diagnostics | Use `sqlite3` for local schema, integrity and reference checks on synthetic databases; Go driver/API calls remain required evidence. No database MCP is required. Verify CLI/embedded-driver SQLite versions separately and use `.backup` or an offline copy after a clean shutdown, never raw active-WAL copying. See [LOCAL-M3](LOCAL-M3.md). |
| M4–M5, if protocol research grows beyond targeted docs/search | Provider-specific documentation or trace helpers (candidates) | First use versioned fixtures and official protocol docs. Evaluate a helper on one named compatibility investigation and retain it only if it improves reproducibility without placing request bodies or secrets in logs. |
| M5–M6, if connector count and cross-package navigation justify it | Graphify or a simple Go dependency graph (candidates) | Generate a graph from actual Go imports/contract boundaries and compare it to targeted search. Adopt only if it detects useful dependency drift; it must not become a second source of architectural truth. External Connector IPC is a **product** milestone in M6, not an OpenCode plugin requirement. |
| If an admin UI enters the roadmap | Browser/Playwright checks (candidate) | Introduce only with an actual UI acceptance scenario and deterministic local test target. A browser MCP brings no value to the current documentation/backend foundation. |

## M3 Tooling Gate

This is a recommendation from the repository's M3 scope and checked-in configuration, not a new machine-local installation or connection check. `.opencode/opencode.json` declares Context7 and `gopls mcp`; the older observations below remain dated evidence. No agent/model/server permissions or personal plugin settings change in this planning commit.

| Need | Existing tool / minimal addition | Decision |
| --- | --- | --- |
| Navigate runtime/service references | gopls plus targeted `rg` and source reads | Keep; query affected symbols instead of indexing/loading the whole milestone. |
| Validate SQLite driver and YAML APIs | Context7 or official versioned docs | Keep; M3-002 pins dependencies and prepares the cache/CI before offline checks. No new docs MCP. |
| Inspect local schema, migrations and history | Optional `sqlite3` executable; real-file Go integration tests | The CLI is convenient, not an agent dependency. Check its version separately from embedded SQLite. Use synthetic files, not production secrets. |
| Prove admission/reconciliation/restart | Existing Go race detector, gated fixtures and subprocess tests | These are acceptance evidence. A database MCP, benchmark framework or fault-injection plugin is unnecessary for these bounded scenarios. |
| Check architecture | Existing production Core import guards and targeted references | No Serena or Graphify installation now. SQLite/YAML stay outside production Core. |
| Manage commits and CI | Git and the developer's existing GitHub integration | Keep; no additional Git MCP. |
| Browser/provider OAuth | Scripted auth in M3 | Leave browser/Playwright disabled for this milestone; real flows are M5.1. |

Optional CLI checks for a synthetic M3 database:

```sh
sqlite3 --version
sqlite3 -readonly /path/to/synthetic-gateway.db '.schema'
sqlite3 -readonly /path/to/synthetic-gateway.db 'PRAGMA integrity_check; PRAGMA foreign_key_check;'
```

Expected integrity result is `ok`; the reference check returns no violations.
[SQLite CLI](https://www.sqlite.org/cli.html) and [PRAGMA reference](https://www.sqlite.org/pragma.html)
own syntax/behavior; the driver's [official package docs](https://pkg.go.dev/modernc.org/sqlite)
own its connection configuration. `sqlite3` is optional: [LOCAL-M3](LOCAL-M3.md)
uses the CLI for a consistent backup when available; the master key is retained
separately.

Do not add a broad skills bundle or change the existing RTK/Ponytail setup for planning. First verify a real repeated gap; keep optional output filters personal and preserve decisive failures/exit codes. The practical context control is one selected task card, exact linked sections and fresh worker/reviewer sessions.

## OpenCode v2 Gate

- Current `.opencode/opencode.json` uses the native v2 `mcp.servers` shape; the five `.opencode/agents/*.md` definitions use native `permissions` rules and supported modes. `.opencode/commands/next.md` uses the v2 project command path, positional `$1`, and current-session execution with the `orchestrator` agent. The command is a prompt workflow, not a hard scheduler or a durable unattended job.
- OpenCode V2 itself does not provide built-in LSP tools. Go semantic navigation and diagnostics here come from the separate official `gopls mcp` server, not a built-in `lsp` action.
- Before enabling any candidate plugin/hook, test its **v2 plugin API** and the command/agent permissions on the installed version. A working v1 hook is not evidence of v2 compatibility. In particular, do not migrate an RTK output hook by copying its v1 file; validate its new entrypoint and output behavior on a disposable task first.
- The earlier OpenCode v2.0.18 registry check exposed `next`, all five project agents, and `context7`. The historical inventory and new gopls check are separate below; plugins were not rechecked. See [CURRENT](CURRENT.md) for actual capabilities. This tooling inspection did not execute `/next` or validate an unattended task cycle. The initial planning card at that historical check was M2-001. The current executable design card is [M3-001](tasks/M3-001.md); dependent cards need actual-result and readiness review.

## gopls MCP for M2

The official Go language server supplies semantic references and package relationships for M2 shared-binding changes. It supplements source reads and repository checks, not the contract or acceptance evidence.

### Verified Check — 2026-10-01

Commands and MCP calls from this checkout:

| Level | Actual observation |
| --- | --- |
| Installed | `opencode --version` → `opencode v2.0.20`; `gopls version` → `golang.org/x/tools/gopls v0.23.0` |
| Configured | Existing, uncommitted `.opencode/opencode.json` entry: local server `gopls`, command `["gopls", "mcp"]`. Inspected personal `~/.config/opencode/opencode.jsonc` has no gopls entry. This update preserves that existing project change; it does not install or duplicate a server. |
| Connected | `opencode mcp list` reports gopls, Context7 and DuckDuckGo connected; Playwright disabled. This alone is not a successful tool call. |
| Verified by call | `go_search({query: "AttemptScope"})` found the shared type in `internal/core/dispatch.go`. `go_symbol_references({file: "/home/pestix/code/pestiroute/internal/core/dispatch.go", symbol: "AttemptScope"})` returned 18 locations, including the declaration, gateway composition and core tests. `go_diagnostics({files: ["/home/pestix/code/pestiroute/internal/core/dispatch.go"]})` returned `No diagnostics.` |

Diagnostics check workspace parse/build errors and additionally lint active files; this call covered the affected `internal/core` file/package but is not a test, vet, or race run. Other catalog operations below were discovered, not invoked during this check. Availability remains machine/session-specific.

| Useful read-only operation | OpenCode permission action | Purpose |
| --- | --- | --- |
| `go_workspace` | `gopls_go_workspace` | Workspace/module overview |
| `go_search` | `gopls_go_search` | Targeted fuzzy symbol search |
| `go_file_context` | `gopls_go_file_context` | A file's cross-file dependencies |
| `go_package_api` | `gopls_go_package_api` | API summary for selected Go package paths |
| `go_symbol_references` | `gopls_go_symbol_references` | References to a type, function, field or method |
| `go_diagnostics` | `gopls_go_diagnostics` | Workspace diagnostics plus active-file linting |

Short scenario: before changing `AttemptScope`, locate it with `go_search`, obtain `go_symbol_references` using its absolute file path and symbol name, then read the affected declarations/callers/tests. Query file context or a package API only where relationships remain unclear. After substantial Go changes, request diagnostics with the affected absolute file paths and run the card's required checks. For small edits, skip unnecessary MCP calls; when unavailable, use `rg`, source reads and ordinary Go checks.

Under default Code Mode, discover these tools and call `tools.gopls.go_symbol_references(...)` / `tools.gopls.go_diagnostics(...)` through `execute`. Reviewer explicitly allows `execute` and only the six read-only gopls actions above; nested permissions still apply. Its blanket deny continues to block `edit`, `subagent`, `gopls_go_rename_symbol` and unlisted MCP actions, including `gopls_go_vulncheck`. Worker/planner inherit the base MCP allowance and need no additional permissions; orchestrator delegates rather than querying source. No blanket MCP allow is added.

If another developer already configures gopls personally, reuse it rather than automatically adding a project entry. Check configured state, connection, and an actual call separately before claiming verification.

## Historical Inspected Inventory — 2026-09-29

Commands run from this checkout: `opencode --version` → **v2.0.18**, `opencode mcp list`, and `opencode plugin list`.

The following observations are preserved as history, not the current M2 inventory or a new plugin verification.

| Item | Observed state | Scope / action now |
| --- | --- | --- |
| Context7 MCP | Connected; a documentation query also succeeded | The only server declared in `.opencode/opencode.json`. Already available; no installation needed here. |
| DuckDuckGo MCP | Connected | Available from the developer environment, not declared by this repository. Optional research tool; keep personal. |
| Playwright MCP | Disabled | Configured in the developer environment; leave disabled for backend M1 work. |
| RTK plugin | Listed as `rtk`, local source `~/.config/opencode/plugins/rtk/index.ts` | Auto-discovered personal plugin. Source includes a V2 `setup` hook and resolves an external `rtk` binary. This inspection does not establish rewrite efficacy, exit-code preservation or package provenance/version. |
| Project plugins | No `plugins` entry or `.opencode/plugins/` directory | No new project plugin is required for M1. |

Browser and OpenCode tools exposed by the session harness are not evidence that a separate browser/OpenCode MCP was configured. Agents, `/next`, and skills are also distinct from installed plugins. This inventory is a machine-local observation, not a required dependency list for other contributors.

## Connecting Tools in OpenCode V2

Run connection checks from the project directory so project configuration participates:

```sh
opencode --version
gopls version
opencode mcp list
opencode plugin list
```

**Context7:** already configured here. On another project, add it locally with:

```sh
opencode mcp add context7 --url https://mcp.context7.com/mcp
opencode mcp list
```

For personal availability in every project, use `opencode mcp add context7 --global --url https://mcp.context7.com/mcp`. If the state is `needs authentication`, open `/mcps`, select the server and sign in. Only a connected state establishes availability; configuration alone does not.

**Other MCP servers:** use `opencode mcp add <name> --url <official-streamable-http-url>` for a remote server, or `opencode mcp add <name> -- <executable> <arguments>` for a local stdio server. Add `--global` before `--` for personal tools. Substitute a verified server command/URL; DuckDuckGo and Playwright do not need duplicate entries on the inspected machine. Use `/mcps` to inspect/connect/disconnect existing servers. For persistent disablement set `disabled: true` on that server in its owning config.

Manual V2 configuration uses `mcp.servers`, for example the project's existing entry:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "servers": {
      "context7": {
        "type": "remote",
        "url": "https://mcp.context7.com/mcp"
      }
    }
  }
}
```

Merge entries into the existing file; preserve `default_agent` and unrelated settings. Global configuration is `~/.config/opencode/opencode.json(c)` (under `$XDG_CONFIG_HOME` when set); project configuration here is `.opencode/opencode.json`. A project server object replaces the same-named global object in full. Remote OAuth credentials live outside configuration; use `{env:NAME}` for credentials only when a server requires header/environment-based authentication.

**Plugins:** for a verified V2-compatible published package, `opencode plugin add <package>@<version>` installs a global package plugin; verify with `opencode plugin list`. For a project, add the package/version or local path to the `plugins` array in its config. Paths resolve relative to that config file. Files/package directories under `.opencode/plugins/` or global `~/.config/opencode/plugins/` are auto-discovered; the inspected RTK uses the latter, so it is already connected. To reproduce that RTK setup elsewhere, obtain the same reviewed V2 plugin and its required binary first; this local inventory does not identify a published package to install. No speculative RTK package command is implied.

Keep Serena, Graphify, database MCPs and additional output hooks conditional on the triggers above; do not install Serena or Graphify now. **For M3 planning: keep the project configuration unchanged. Use gopls, Context7, built-in file/shell/Git tools and the Go checks; no additional MCP/plugin installation is required.**

References: [OpenCode v2 migration](https://opencode.ai/v2/docs/migrate-v1/), [commands](https://opencode.ai/v2/docs/commands/), [agents](https://opencode.ai/v2/docs/agents/), [MCP servers](https://opencode.ai/v2/docs/mcp-servers/), [configuration](https://opencode.ai/v2/docs/config/), [tools](https://opencode.ai/v2/docs/tools/), [permissions](https://opencode.ai/v2/docs/permissions/), [V2 plugins](https://opencode.ai/v2/docs/build/plugins/). The historical 2026-10-01 check used V2 MCP/tool/permission documentation and Context7's V2 documentation query; it changed worker guidance and reviewer permissions, not server/model settings. The M3 planning update changes documentation only.
