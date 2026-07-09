# Installing jindo-mcp

`jindo-mcp` is a single self-contained Go binary (standard library only) that
speaks the Model Context Protocol over stdio. Build it once, then register it
with your MCP host (Claude Code, Codex CLI, or Antigravity/Gemini `agy`).

## Quick start (one command)

```bash
make install      # build + register with every detected host (Claude/Codex/agy)
# or:  ./install.sh
```

`install.sh` builds the binary, reports which sub-agent CLIs
(`claude`/`codex`/`agy`) are on `PATH`, registers the `jindo` server with each
MCP host it finds (idempotent), and runs an MCP handshake smoke test. Preview
without changing any config: `DRY_RUN=1 ./install.sh`. Other targets:
`make build`, `make test`, `make smoke`, `make clean`.

The rest of this document is the manual per-host setup (what the script
automates) and runtime notes.

## 1. Build

From the repo root:

```bash
go build -o jindo-mcp ./cmd/jindo-mcp
```

This produces `./jindo-mcp` with no runtime dependencies. Cross-compile with the
usual `GOOS`/`GOARCH`. Use an absolute path to the binary in the configs below
(shown as `/abs/path/to/jindo-mcp`).

## 2. Prerequisites — the sub-agent CLIs

jindo does not generate code itself; it **routes each task to an agent CLI** and
runs it headless. Install and authenticate the ones you want jindo to use, and
make sure they are on `PATH`:

| agent id | CLI | role |
|----------|-----|------|
| `claude` | Claude Code (`claude`) | author / reviewer |
| `codex`  | Codex CLI (`codex`)    | author / reviewer |
| `agy`    | Antigravity/Gemini (`agy`) | author / reviewer |

Because jindo spawns these as sub-processes, the host must allow subprocess
execution (see the per-host sandbox notes).

## 3. Register with your MCP host

### Claude Code
Project-scoped — create `.mcp.json` at the repo root:

```json
{
  "mcpServers": {
    "jindo": { "type": "stdio", "command": "${CLAUDE_PROJECT_DIR:-.}/jindo-mcp", "args": [] }
  }
}
```

or user-scoped: `claude mcp add jindo -- /abs/path/to/jindo-mcp`.
Then restart Claude Code or run `/mcp` to connect.

### Codex CLI
```bash
codex mcp add jindo -- /abs/path/to/jindo-mcp
```
`codex mcp add` does not set timeouts, so add them (and, optionally, the
LLM-assess env) to the block in `~/.codex/config.toml`:

```toml
[mcp_servers.jindo]
command = "/abs/path/to/jindo-mcp"
startup_timeout_sec = 30.0
tool_timeout_sec    = 1800.0

[mcp_servers.jindo.env]
JINDO_LLM_ASSESS = "1"   # optional: enable the LLM difficulty-assessment fallback
```

**Sandbox:** jindo's `dispatch` spawns sub-agent processes, which Codex's default
`workspace-write` sandbox cancels. Run Codex with
`--dangerously-bypass-approvals-and-sandbox` (or a profile whose
`sandbox_mode="danger-full-access"` and `approval_policy="never"`). See
[docs/jindo-mcp.md](docs/jindo-mcp.md) and [docs/codex-install.md](docs/codex-install.md)
for the full walkthrough (profile, sub-agent isolation, async/long-task usage).

### Antigravity / Gemini CLI (`agy`)
Register under `mcpServers` in agy's MCP config
(`~/.gemini/config/mcp_config.json`, or `~/.gemini/settings.json` depending on
your agy version):

```json
{
  "mcpServers": {
    "jindo": { "command": "/abs/path/to/jindo-mcp", "args": [] }
  }
}
```

> Confirm the exact key/file for your `agy` build (`agy` hosts MCP servers via
> `~/.gemini` config); the shape above is the standard `mcpServers` object.

## 4. Verify

After registering, list tools in the host — you should see **13**:
`dispatch`, `dispatch_async`, `dispatch_multi`, `job_status`,
`plan`, `plan_next`, `plan_record`, `plan_revise`, `plan_status`,
`memory`, `agents`, `compact`, `calibrate`.

Host-independent stdio smoke test:

```bash
printf '%s\n%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' | ./jindo-mcp
```

You should get an `initialize` result (`serverInfo.name = "jindo-mcp"`) followed
by a `tools/list` result listing the 13 tools.

## 5. Runtime notes

- jindo anchors its `.jindo` shared-memory store to the current working
  directory, or to `CLAUDE_PROJECT_DIR` when that env var is set. Leave it unset
  so jindo follows the host into whatever project it is launched from.
- `JINDO_LLM_ASSESS=1` enables an agy-backed difficulty re-judgement for
  borderline tasks (off by default; falls back to the deterministic scorer).
- Multi-step work: use `plan` then drive one step at a time with
  `plan_next` → `dispatch` → `plan_record` (see [docs/jindo-mcp.md](docs/jindo-mcp.md)).
