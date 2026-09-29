# Development Tooling Adoption

This plan covers tools for **building PestiRoute with OpenCode**, not the gateway's client-facing MCP support or its backend Connectors. Stages refer to [ROADMAP.md](ROADMAP.md); a milestone name is a decision checkpoint, not an installation date. Add a tool only when the named work needs it and verify it against the installed OpenCode version first. Keep project configuration small; developer-specific credentials and optional tools belong in personal configuration.

| When / trigger | Tool or practice | What to do and acceptance signal |
| --- | --- | --- |
| Now, M1 execution | Existing `AGENTS.md`, project agents, atomic task cards, and Context7 remote MCP | Keep Context7 as the only project MCP. Use it for library API documentation when needed; inspect project sources first. Verify the connection with `opencode mcp list` on the developer machine. The Planner → Worker → Reviewer flow and `/next n` use the existing agents; each worker receives exactly one READY card. |
| M0 scaffold, then ongoing | Go toolchain and CI, built-in OpenCode file/shell tools | Pin Go in the scaffold; establish `gofmt`, vet, tests, race checks, and build in the task/CI. These checks provide evidence before introducing another code-navigation MCP. `CURRENT.md` records commands only after they work. |
| When PR work becomes routine | Git CLI and GitHub integration already available to the developer | Use Git for scoped local diffs/commits and GitHub integration for PR review/CI where useful. Keep credentials out of the project config. No additional Git MCP is needed just to edit files or open a PR. |
| M1–M2, only if code navigation becomes a measured bottleneck | Serena (candidate code-navigation MCP) | Try it on one Go task where repeated manual symbol/call-site searches cost time. Check supported Go indexing, OpenCode v2 connection and tool exposure, context overhead, and whether it finds references the built-in search misses. Keep it personal/disabled by default until it earns its place. |
| When shell output repeatedly wastes context | Personal RTK plugin or a small output filter | A local RTK plugin is loaded on the inspected machine (see inventory below). Compare raw and filtered results on test failures, review diffs, and streaming diagnostics before project adoption. Verify exit status and decisive error context; plugin discovery alone does not prove those properties. |
| M2–M4, when contract and protocol checks recur | Small project-local commands or skills (candidate) | Extract a repeatable checklist only after it has been used successfully on real tasks, for example contract conformance or fixture review. Prefer links to the owning docs over copied rules. Add no blanket skills package now; Ponytail guidance already lives in the worker agent. |
| M3, when persistence and concurrent accounting exist | SQLite CLI and targeted diagnostics | Inspect schema and migration behavior with local commands and deterministic fixtures. Add a database MCP only if a specific repeated investigation cannot be handled clearly by CLI/tests; never expose production credentials by default. |
| M4–M5, if protocol research grows beyond targeted docs/search | Provider-specific documentation or trace helpers (candidates) | First use versioned fixtures and official protocol docs. Evaluate a helper on one named compatibility investigation and retain it only if it improves reproducibility without placing request bodies or secrets in logs. |
| M5–M6, if connector count and cross-package navigation justify it | Graphify or a simple Go dependency graph (candidates) | Generate a graph from actual Go imports/contract boundaries and compare it to targeted search. Adopt only if it detects useful dependency drift; it must not become a second source of architectural truth. External Connector IPC is a **product** milestone in M6, not an OpenCode plugin requirement. |
| If an admin UI enters the roadmap | Browser/Playwright checks (candidate) | Introduce only with an actual UI acceptance scenario and deterministic local test target. A browser MCP brings no value to the current documentation/backend foundation. |

## OpenCode v2 Gate

- Current `.opencode/opencode.json` uses the native v2 `mcp.servers` shape; the five `.opencode/agents/*.md` definitions use native `permissions` rules and supported modes. `.opencode/commands/next.md` uses the v2 project command path, positional `$1`, and current-session execution with the `orchestrator` agent. The command is a prompt workflow, not a hard scheduler or a durable unattended job.
- Before enabling any candidate plugin/hook, test its **v2 plugin API** and the command/agent permissions on the installed version. A working v1 hook is not evidence of v2 compatibility. In particular, do not migrate an RTK output hook by copying its v1 file; validate its new entrypoint and output behavior on a disposable task first.
- The earlier OpenCode v2.0.18 registry check exposed `next`, all five project agents, and `context7`. The current connection/plugin check is recorded below. Go source and `./scripts/check.sh` now exist and M0 checks have passed; see [CURRENT](CURRENT.md) for actual capabilities. This tooling inspection did not execute `/next` or validate an unattended task cycle. `/next 1` now starts with the bounded M1-004 planning card; subsequent cards need dependency and readiness review.

## Inspected Inventory — 2026-09-29

Commands run from this checkout: `opencode --version` → **v2.0.18**, `opencode mcp list`, and `opencode plugin list`.

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

Keep Serena, Graphify, database MCPs and additional output hooks conditional on the triggers above. **For M1 now: use Context7, built-in file/shell tools and the Go checks; no additional MCP/plugin installation is required.**

References: [OpenCode v2 migration](https://opencode.ai/v2/docs/migrate-v1/), [commands](https://opencode.ai/v2/docs/commands/), [agents](https://opencode.ai/v2/docs/agents/), [MCP servers](https://opencode.ai/v2/docs/mcp-servers/), [configuration](https://opencode.ai/v2/docs/config/), [plugin loading](https://opencode.ai/v2/docs/plugins/), [V2 plugin API](https://opencode.ai/v2/docs/build/plugins/). Connection/loading instructions were checked against V2 documentation and Context7's V2 documentation index; no configuration was changed during this inspection.
