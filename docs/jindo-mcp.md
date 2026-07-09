# jindo-mcp — Go MCP Server

## Overview

`jindo-mcp` is the jindo orchestrator core ported to Go and exposed as a
Model Context Protocol (MCP) server. It produces a **single static binary**
with no runtime or dependency pollution: the build requires only the Go
standard library, and the resulting binary carries everything it needs
(routing policy and agent config are embedded at compile time via `go:embed`).

The server speaks **JSON-RPC 2.0 over stdin/stdout** using a newline-delimited
line transport — the same stdio transport all major MCP hosts use for local
servers. No network port, no daemon, no external process manager needed.

## Build

From the repository root:

```bash
go build -o jindo-mcp ./cmd/jindo-mcp
```

This produces a self-contained binary `jindo-mcp` with no runtime dependencies
beyond the OS. Cross-compilation works as usual with `GOOS`/`GOARCH`.

Verify the full module builds clean (stdlib only, zero external deps):

```bash
go build ./...
go test ./...
```

## Registering with an MCP client

`jindo-mcp` is a **stdio server**: the MCP host spawns it as a child process and
communicates over stdin/stdout.

Use a project-scoped `.mcp.json` at the repo root to register it automatically
for Claude Code (this file is created locally and is not committed to the repo):

```json
{
  "mcpServers": {
    "jindo": {
      "type": "stdio",
      "command": "${CLAUDE_PROJECT_DIR:-.}/jindo-mcp",
      "args": []
    }
  }
}
```

`${CLAUDE_PROJECT_DIR:-.}` resolves to the repo root regardless of which
machine or checkout path it's cloned into, so the config is portable — but the
binary itself is **not** committed (see `.gitignore`: `/jindo-mcp`). **You must
build it once after cloning** (see Build above) before Claude Code can connect
to the `jindo` MCP server; a missing binary at that path is the most common
reason `jindo` shows as disconnected. After building, restart Claude Code (or
run `/mcp` to reconnect) to pick up the server.

For a non-Claude-Code MCP host, or a binary kept outside the repo, use an
absolute path instead:

```json
{
  "mcpServers": {
    "jindo": {
      "command": "/absolute/path/to/jindo-mcp",
      "args": []
    }
  }
}
```

The server speaks JSON-RPC 2.0 over stdin/stdout; no additional flags are
required.

## Tools

The server exposes seven tools (defined in `internal/mcp`):

### `dispatch`

Route a coding task to the appropriate agent and model, then execute it.

Input:

| Field      | Type    | Required | Description                                                              |
|------------|---------|----------|---------------------------------------------------------------------------|
| `task`     | string  | yes      | The task description to route and run                                    |
| `agent`    | string  | no       | Override the agent (claude / codex / agy)                                |
| `priority` | string  | no       | Routing priority hint: `cost`, `quality`, or `latency`; reweights intra-tier agent selection |
| `review`   | boolean | no       | Opt-in cross-model peer review of the result. Default `false` (no review). |

Returns a JSON object with fields: `agent`, `model`, `difficulty`, `result`,
`key` (the shared-memory key under which the result was stored).

#### Cross-model peer review (`review: true`)

When `review` is set, the author's result goes through a best-effort
cross-model review stage (`Orchestrator.DispatchWithReview` in
`internal/orchestrator`) after the normal dispatch:

1. A reviewer is chosen via profile matching over the same difficulty tier,
   **excluding the author's own agent** (`routing.SelectReviewer`), so review is
   always cross-model.
2. The reviewer runs with the same per-CLI headless contract as the author, but
   in **read-only mode** — it never edits/writes files — and emits a
   `verdict` of `approved`, `changes_requested`, or `rejected` plus a list of
   findings (`severity`: `critical` / `major` / `minor` / `info`).
3. If any finding is `critical`, the author gets **exactly one** revision
   round: it re-runs with the findings appended to the task, followed by
   **one** re-review of the revised result. There is no further recursion.
4. If the re-review still has a critical finding, the dispatch's `status` is
   `review_failed` (the revised result is still returned). Otherwise the
   dispatch's status reflects the (possibly revised) result as usual.
5. Reviewer failures — no cross-model reviewer available, adapter error, or
   an unparseable review response — are **best-effort**: they never fail the
   dispatch; they degrade to an errored review record and the author's result
   is returned unchanged.

`review` defaults to `false`; a review-off dispatch behaves exactly as before
review existed.

### `dispatch_async` / `job_status`

For tasks that could exceed the MCP host's per-tool-call timeout, `dispatch`
has a background counterpart. `dispatch_async` takes the same
`{task, agent?, priority?, review?}` shape as `dispatch`, submits it to an
in-process job manager (`internal/jobs`), and returns immediately with
`{job_id, status: "running"}` — it does not wait for the result.

**Polling contract:** after calling `dispatch_async`, the caller MUST poll
`job_status` with the returned `job_id` until the status is `"done"` or
`"error"`. A `"running"` status is not a result and must not be treated as
one — this is stated in the `dispatch_async` tool description itself so an
MCP client cannot miss it.

`job_status` input:

| Field      | Type    | Required | Description                                                  |
|------------|---------|----------|----------------------------------------------------------------|
| `job_id`   | string  | yes      | The id returned by `dispatch_async`                            |
| `wait_sec` | integer | no       | Long-poll duration in seconds. Default `25`, capped at `30`.  |

`job_status` **long-polls**: the server blocks for up to `wait_sec` seconds
waiting for the job to reach a terminal state before responding, rather than
returning `"running"` instantly, so a client can poll in a loop without
hammering the server. An unknown `job_id` is an invalid-params error.

Response shape:
- `status: "running"` — no other fields; keep polling.
- `status: "done"` — includes `result`, the same payload `dispatch` would
  have returned synchronously.
- `status: "error"` — includes `error`, the failure message.

**Persistence:** only terminal jobs (`done`/`error`) are persisted, one JSON
file per job at `<mem root>/jobs/<id>.json` (i.e. `.jindo/jobs/<id>.json` by
default) — a still-`running` job has no meaning across a server restart, so
it is not written to disk. Persistence and load-back are best-effort
(`internal/jobs.Manager`): a disk failure never fails the job itself.

### `dispatch.log`

Every dispatch appends one JSON line to `<memory root>/dispatch.log`
(`internal/orchestrator.dispatchLogEntry`), in addition to the existing
routing/memory/status fields:

| Field         | Description                                                                 |
|---------------|-------------------------------------------------------------------------------|
| `duration_ms` | Wall-clock latency of the author adapter run alone (excludes routing, memory I/O, and, on a revision round, reviewer time). |
| `memory_used` | Shared-memory keys the agent reports it actually read, from its `memory_used` response field. Omitted if empty. |
| `review`      | Present only when `review: true` ran for this dispatch (`reviewer_agent`, `reviewer_model`, `verdict`, `findings` severity counts, `revision_rounds`, `final_status`, `errored`). Omitted (not present) for a review-off dispatch, so its log line stays byte-identical to before review existed. |

### `memory`

Read the shared memory store. All agents in a jindo session write their results
to a file-locked JSON store; this tool exposes that store to the MCP client.

Input:

| Field | Type   | Required | Description                                          |
|-------|--------|----------|------------------------------------------------------|
| `key` | string | no       | A specific memory key (e.g. `task:1`); omit for all  |

Returns the entry for the requested key (`{key, found, value}`), or the full
store when no key is given.

### `agents`

List all agents and their per-difficulty-tier model assignments.

No input required. Returns the routing table as a JSON object.

### `compact`

Trigger memory compaction to bound the working set: drops superseded/expired
entries, folds the cold tail into a digest, and keeps only the last N notes.

Input:

| Field         | Type    | Required | Description                                  |
|---------------|---------|----------|-----------------------------------------------|
| `max_entries` | integer | no       | Max live entries to retain (defaults apply)  |
| `max_notes`   | integer | no       | Max recent notes to retain (defaults apply)  |

### `calibrate`

Aggregate `dispatch.log` (`internal/calibrate`) into a routing calibration
report: status distribution per tier/model, signal match frequencies,
near-threshold dispatch count, per-model author-run latency, cross-model
review outcomes, and advisory-only threshold/weight suggestions. It only
reads the log; it never mutates routing config.

Input:

| Field  | Type   | Required | Description                                                        |
|--------|--------|----------|----------------------------------------------------------------------|
| `path` | string | no       | Path to the dispatch.log JSONL file. Defaults to `.jindo/dispatch.log` relative to the server's working directory. |

Returns the aggregated report, which includes:
- **status by tier / status by model** — outcome counts
- **signal match frequency** — how often each routing signal fired
- **latency by model** — count/avg/min/max `duration_ms` per model (author
  adapter run only; dispatches missing `duration_ms` are excluded from the
  distribution, not counted as 0ms)
- **review** — reviewed count, errored count, verdict distribution
  (`approved` / `changes_requested` / `rejected`), and per-author-model
  outcomes (`reviewed`, `review_failed`, `changes_requested`), for dispatches
  where `review: true` ran
- **suggestions** — advisory-only strings (e.g. a tier's non-ok rate, a
  never-matched signal, a high near-threshold rate, or a high
  changes_requested rate for an author model); nothing here is ever applied
  automatically

## Architecture

```
cmd/jindo-mcp/           entry point — wires real collaborators, starts Serve()
internal/
  meta/                 module version constant
  memory/               file-locked JSON shared-memory store
  agent/                CLI adapter (claude / codex / agy)
  tmux/                 persistent tmux session manager
  routing/              deterministic multi-signal task scorer + model router
  orchestrator/         wires router + tmux + adapters + memory into Dispatch()
  mcp/                  JSON-RPC 2.0 server: initialize / tools/list / tools/call
```

`routing` and the agent config are loaded from JSON files embedded at build time
(`go:embed`), so the binary is fully self-contained after `go build`.

## Headless multi-agent design

### Orchestrator — lean and stateless

The orchestrator never inlines shared-memory content into its own process.
Instead, it grants each headless agent direct file access to the bounded
memory root via a system prompt and CLI flags (`--add-dir .jindo` for claude/agy;
trusted cwd for codex), and the **agent itself** reads memory before working.
This keeps orchestrator token usage flat and scales to many concurrent agents
without memory bloat.

### Per-CLI headless commands

Each agent CLI is invoked with a model flag and a task prompt (`-p`):

- **claude:** `claude --model <id> --add-dir .jindo -p <task>`
- **codex:** `codex exec -m <id> --skip-git-repo-check <task>`
- **agy:** `agy --model <id> --add-dir .jindo -p <task>` (agy is the Gemini gateway)

The `--add-dir` flag (claude/agy) gives the agent access to the `.jindo` directory containing prior
context; codex relies on a trusted working directory. The `-p` flag signals
headless operation: the agent reads memory, does the work, and outputs JSON.

### Response contract

The system prompt (built by `agentproto.BuildSystemPrompt`) instructs each agent to:
1. Read the shared memory (the bounded `.jindo` directory)
2. Perform the requested work
3. End output with exactly one JSON block:

```json
{
  "status": "ok|error",
  "result": "<deliverable>",
  "summary": "<what was done>",
  "memory_updates": [
    {"key": "<id>", "note": "<context>", "value": <any>}
  ]
}
```

The orchestrator's `ParseResponse` scans agent stdout for the last balanced
top-level JSON object, unmarshals it, and applies memory updates. If no valid
JSON is found, it falls back to `{status: "unparsed", result: <stdout>}`,
so malformed output never crashes the orchestrator.

### Memory concurrency — atomic, agent-partitioned

The store (`memory.json`) is protected by `syscall.Flock` advisory locking:
- **Lock lifetime:** automatically released when the process dies (no hanging locks)
- **Key allocation:** collision-free agent-partitioned keys `task:<agent>:<n>` allocated under
  the lock; each agent has its own counter partition, so concurrent orchestrators
  and agents never collide
- **Ownership partition:** an agent writes only its own keys; reads are shared
  across all agents
- **Append-only notes:** agents append to `_notes` array; existing entries are
  never mutated
- **Idempotent upsert:** writing the same key twice idempotently replaces the
  prior value (no duplicates)

### Compaction — deterministic with optional LLM summarizer

When the store grows large, `MaybeCompact` (triggered by a configurable entry-count
threshold) applies deterministic compaction rules atomically under the lock:

1. **Supersede by key:** keep the newest-ts entry per unique key (identity op if
   keys are truly unique)
2. **TTL drop:** remove COMPLETED entries (with non-null `result`) older than the
   configured TTL window
3. **Cap:** if live entries exceed `MaxEntries`, fold the oldest (cold-tail) entries
   into a single `_digest` entry; keep the newest `MaxEntries` live
4. **Note trim:** keep only the last-N notes

The cold-tail is summarized by an optional `Summarize` callback (mockable seam,
off in the deterministic gate). If summarization fails, the deterministic text is
used directly, so a flaky summarizer never loses data.

The `compact` MCP tool exposes this; the orchestrator can trigger it on demand, and
concurrent agents see the compacted store on their next memory read.

### Exposed tools

The MCP server now provides **seven tools**:
- `dispatch`: route + run a task, store result in shared memory (optionally with cross-model peer review)
- `dispatch_async`: submit a task to run in the background, return a `job_id` immediately
- `job_status`: long-poll a `job_id` for the terminal (`done`/`error`) result
- `memory`: read a specific key or the full store
- `agents`: list agent/model routing table
- `compact`: run compaction with specified options (MaxEntries, TTLSeconds, etc.)
- `calibrate`: aggregate dispatch.log into a routing calibration report

## Legacy Python implementation

The original Python implementation under `jindo/` is **retained and untouched**.
Its test suite continues to run under `pytest` and covers the Python
orchestration logic independently. The Go MCP server is the new
environment-independent entry point; it reimplements the same routing logic in
pure Go rather than wrapping the Python code.
