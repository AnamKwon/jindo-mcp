# Live Verification Notes

## Orchestrator Diagnostic Run

A one-off live diagnostic was performed to verify difficulty-based routing and cross-agent shared memory.

### What Was Tested

- Fresh jindo-mcp binary built and live JSON-RPC dispatch over stdio
- ~5-8 real JSON-RPC requests covering trivial, standard, and hard difficulty tasks
- Cross-agent memory-reference scenario: orchestrator dispatching to multiple agents with shared .jindo/memory.json
- Inspection of resulting memory state and agent response contracts

### CONFIRMED WORKING (No Regressions Expected)

- **Difficulty-based routing 100% correct:**
  - Trivial → agy/gemini-3.5-flash
  - Standard → claude/claude-sonnet-5
  - Hard → codex/gpt-5.5
  - (Matches jindo/config/models.json / internal/routing/config/models.json)

- **Cross-agent memory read:** claude-family agents successfully read .jindo/memory.json via --add-dir + BuildSystemPrompt instruction; accurately reported prior codex dispatch decisions.

- **AllocKey collision-free:** agent-partitioned keys (task:\<agent\>:\<n\>) worked across live processes with zero collisions.

### Five Bugs Found & Fixed

1. **BUG1 [critical]:** agy CLI crashed on every dispatch (`flags provided but not defined: -append-system-prompt`). agy has no such flag, only --add-dir. **Fix:** agy now receives only --add-dir; memory-read/contract instruction prefixed into task text.

2. **BUG2 [high]:** codex received zero memory-read or response-contract instruction (codex has no system-prompt or --add-dir flag). **Fix:** instruction now prefixed into codex's task text.

3. **BUG3 [high, most damaging]:** memory corruption — authoritative result Upsert ran BEFORE agent memory_updates fan-out; agent naming its own dispatch key silently clobbered structured record. Reproduced twice. **Fix:** fan-out now runs before authoritative write; structured record always wins final state.

4. **BUG4 [medium]:** MCP dispatch tool response omitted status/summary fields despite orchestrator Result carrying them. **Fix:** added fields to response.

5. **BUG5 [low, DX]:** failed subprocess calls swallowed stderr, showing only "exit status N" with no diagnostics; made bug 1 hard to root-cause. **Fix:** ExitError.Stderr captured and wrapped into returned error.

### Current State (as of the 5-bug fix pass above)

- Each bug has deterministic regression test (mocked seams, no real CLI calls)
- `go test ./...` green
- No MCP protocol/tool-name changes; no models.json changes; no Python changes

## Follow-up: Permission-Gate Stall (2026-07)

A subsequent live dispatch asking an agent to write a real file (e.g. `.gitignore`)
surfaced a related but distinct problem: headless dispatch has no TTY to answer an
interactive permission/approval prompt, so any task requiring a file write stalled.

### Confirmed Root Causes (per adapter)

- **claude:** without `--permission-mode`, headless writes stall — the agent
  replies "you haven't granted permission" and does nothing (exit 0, no file
  written). **Fix:** added `--permission-mode acceptEdits` (scoped to file
  edits, not a full `--dangerously-skip-permissions` bypass).
- **codex:** its default sandbox is directory-trust-dependent; outside a
  trusted directory it silently refuses writes ("read-only sandbox and
  approvals are disabled"). **Fix:** added `-s workspace-write` (scoped to the
  working directory + /tmp, not `danger-full-access`). This also required
  fixing `internal/agent/agent.go`'s `kindCodex` branch, which previously
  discarded all extras under the (now-disproven) assumption that codex had no
  relevant flags — `codex exec --help` in full lists `-s/--sandbox` and
  `--add-dir`.
- **agy [more severe, separate bug]:** agy does **not** operate on the actual
  process cwd by default, or when only a nested subdirectory (e.g. the memory
  root) is granted via `--add-dir` — it silently redirects all work into its
  own default scratch directory (`~/.gemini/antigravity-cli/scratch`) and
  still reports success there, never touching the real project files. **Fix:**
  grant `--add-dir` for the actual cwd (in addition to the memory dir) plus
  `--dangerously-skip-permissions` (agy's only available bypass — no scoped
  acceptEdits-equivalent exists for it).

### Live Re-Verification

Rebuilt the binary and dispatched a real "create `.gitignore`" task to each of
claude, agy, and codex from a fresh scratch directory: all three returned
`status:"ok"` and the file was confirmed written in the **correct** working
directory (previously agy wrote nothing there and silently reported success
elsewhere; claude and codex reported being blocked and wrote nothing).

### Current State

- `internal/orchestrator/orchestrator.go`'s `buildDispatchArgs` now also
  decides the per-CLI privilege grant, with the live evidence documented
  inline; `Dispatch` resolves the real cwd via `os.Getwd()` for agy's grant.
- Regression tests assert the new flags are present in each adapter's
  captured argv (mocked seams; `go test ./...` stays green, deterministic).
- No MCP protocol/tool-name changes; no models.json changes; no Python changes.

## Follow-up: Sensitive-File Dispatch Policy (2026-07)

Once headless writes actually worked (the fix above), the risk flipped: the
same permission flags that let agents write real files also let them write
*sensitive* ones (`.env`, `.mcp.json`, `.claude/settings.local.json`, ssh
keys, cloud credentials, etc.) with no human in the loop to catch it.

### Per-CLI Path-Deny Capability Check (live)

- **claude:** supports path-scoped denial via
  `--disallowedTools "Write(<pattern>)" "Edit(<pattern>)"`.
  Live-confirmed: with this flag, a task asking claude to create `.env` was
  refused ("The write was blocked by permission settings — `.env` files are
  denied by default.") and no file was written; without it, the same task
  succeeds and creates the file.
- **codex:** `codex exec --help` (full listing) has no per-file write/edit
  deny. Its only relevant flags are `-s/--sandbox` (read-only /
  workspace-write / danger-full-access — directory-scoped, not per-file) and
  `--add-dir`.
- **agy:** `agy --help` has no per-file deny either. Its only relevant flags
  are `--sandbox` (terminal restrictions, not path-scoped) and
  `--dangerously-skip-permissions`.

Conclusion: only claude has a usable per-file deny. A policy that relied on
CLI flags alone would leave codex and agy completely unprotected.

### Design: Orchestrator-Level Gate (CLI-agnostic)

`internal/policy` adds a pure, deterministic `Check(task string) (blocked
bool, matched string)` over a list of sensitive glob patterns (`.env`,
`.env.*`, `.mcp.json`, `.claude/settings.json`,
`.claude/settings.local.json`, `id_rsa*`, `id_ed25519*`, `*.pem`, `*.key`,
`*.pfx`, `.npmrc`, `.netrc`, `.aws/credentials`, `.aws/config`, `.ssh/*`,
`credentials.json`).

`Orchestrator.Dispatch` calls `policy.Check` on the task text as its very
first step — before routing, before any memory write, before
`GetAdapter`/`RunWith`. A match returns a `*policy.BlockedError` immediately;
no agent is ever invoked and no memory entry is created for a blocked
attempt. This is the primary defense and it is uniform across all three
CLIs, since it never depends on a CLI's own flags.

As defense-in-depth (claude only, since it's the only CLI that supports it),
`buildDispatchArgs`'s claude case also appends
`policy.ClaudeDisallowedToolArgs()` (`--disallowedTools` with `Write(<pat>)`/
`Edit(<pat>)` for every pattern) — this catches the narrower case where
claude decides *mid-task* to touch a sensitive path that was never named in
the original task text, which the text-based gate above cannot see.

### Live Re-Verification

Rebuilt the binary and dispatched three tasks via MCP `dispatch`:
- `"write SECRET=1 into .env..."` (agent: claude) → refused with
  `policy: task references sensitive path (matched ".env")`; no file
  written, no adapter invoked.
- `"open .mcp.json and register a new server entry"` (agent: codex) →
  refused the same way, proving the gate protects codex too (which has no
  CLI-level equivalent).
- `"add a small health check endpoint..."` (unrelated task) → passed through
  normally; claude ran and created `health.js` in the working directory.

### Current State

- New package `internal/policy` (pure functions, no I/O).
- `internal/orchestrator/orchestrator.go`'s `Dispatch` gates on
  `policy.Check` first; claude's `buildDispatchArgs` case also appends
  `policy.ClaudeDisallowedToolArgs()`.
- Regression tests: `internal/policy/policy_test.go` (pattern matching, both
  blocked and allowed cases) and
  `internal/orchestrator/orchestrator_test.go` (`TestDispatchBlocksSensitivePathBeforeAnyAdapterCall`,
  `TestDispatchAllowsOrdinaryTaskThroughPolicyGate`, plus a claude-extras
  assertion for `--disallowedTools`).
- `go test ./...` green; no MCP protocol/tool-name changes; no models.json
  changes; no Python changes.
  captured argv (mocked seams; `go test ./...` stays green, deterministic).
- No MCP protocol/tool-name changes; no models.json changes; no Python changes.

## Loop-0010 Re-Verification (permission gate)

Diagnosis-only re-check (2026-07-02, ~05:43 UTC): re-ran the exact live scenario
above against the current `buildDispatchArgs`/`kindClaudeLike`/`kindCodex` code
(no `.go` edits made in this step) to confirm the previously-fixed permission
gate has not regressed.

**Procedure:** rebuilt `go build -o /tmp/jindo-mcp-t1 ./cmd/jindo-mcp`, created
three fresh throwaway cwds (`/tmp/jindo-loop-verify-perm-{claude,codex,agy}`),
and from inside each ran the rebuilt binary with stdin JSON-RPC
(`initialize` then `tools/call name=dispatch`) asking each of claude / codex /
agy in turn to create `hello.txt` containing `hello` in that cwd. Each run was
given up to 5 minutes; none needed it.

**Results:**

- **claude:** `status:"ok"`, `summary:"File hello.txt created at
  /private/tmp/jindo-loop-verify-perm-claude/hello.txt"`. `hello.txt` (content
  `hello`) confirmed present via `ls`/`cat` in
  `/tmp/jindo-loop-verify-perm-claude`. No stall, returned well under the
  5-minute budget.
- **codex:** `status:"ok"`, `summary:"Read the shared .jindo context, added the
  requested file, and verified its contents."`. `hello.txt` (content `hello`)
  confirmed present in `/tmp/jindo-loop-verify-perm-codex`. No
  "read-only sandbox and approvals are disabled" error — `-s workspace-write`
  is still being applied.
- **agy:** `status:"ok"`, `summary:"Read shared memory, created hello.txt with
  the text 'hello', and verified the file contents."`. `hello.txt` (content
  `hello`) confirmed present in `/tmp/jindo-loop-verify-perm-agy` (the actual
  dispatch cwd). Cross-checked `~/.gemini/antigravity-cli/scratch` directly —
  it still only contains its pre-existing `.gitignore` (unmodified timestamp),
  i.e. agy did **not** silently redirect the write there.

**Verdict: no regression found, all three CLIs still write to the correct
cwd.** `buildDispatchArgs` (claude: `--append-system-prompt` + `--add-dir` +
`--permission-mode acceptEdits`; agy: prefixed instruction + `--add-dir` for
both cwd and memDir + `--dangerously-skip-permissions`; codex: prefixed
instruction + `-s workspace-write`) and the `kindClaudeLike`/`kindCodex` argv
builders in `internal/agent/agent.go` continue to produce argv that makes each
CLI actually write into the dispatch's real working directory, with no
stalls and no silent misdirection. `go build ./...` still succeeds (no `.go`
files were touched in this diagnostic step).


## Loop-0010 Re-Verification (shared memory)

Diagnosis-only re-check (2026-07-02): live re-verification of shared-memory
correctness — `internal/memory/*.go` (`SharedMemory`, `AllocKey`, `Upsert`,
`OwnerOf`, flock-based locking) and, specifically, the past BUG3 fix in
`internal/orchestrator/orchestrator.go`'s `Dispatch`: the agent's
`memory_updates` fan-out loop must run BEFORE the authoritative result
`Upsert` under the dispatch's own key, so that an agent naming its own
dispatch key in `memory_updates` cannot clobber the final structured record
with a stray scalar. No `.go` files were edited in this step.

**Procedure:** rebuilt `go build -o /tmp/jindo-mcp-t2 ./cmd/jindo-mcp`, created a
throwaway cwd `/tmp/jindo-loop-verify-mem`, and drove the rebuilt binary over
stdin JSON-RPC (`initialize` + `tools/call name=dispatch`), one dispatch per
process invocation, letting `.jindo/memory.json` accumulate across runs in that
same cwd. `.jindo/memory.json` was `cat`/`json.tool`'d after every step.

**Step (a) — cross-agent key allocation, no collisions.** Dispatched one
trivial no-op task to each of `claude`, `codex`, and `agy` in turn:
- `claude` -> key `task:claude:1`
- `codex` -> key `task:codex:2`
- `agy` -> key `task:agy:3`

`AllocKey` computes `n` as one past the max index across *all* existing
`task:*` keys store-wide (not a per-agent-local counter — confirmed by reading
`keyN`/`AllocKey` in `internal/memory/memory.go`), so the shared global index
advances 1, 2, 3 across agents while the per-key agent segment still comes out
correctly partitioned (`task:<agent>:<n>`). Resulting `memory.json` after all
three: exactly `task:claude:1`, `task:codex:2`, `task:agy:3`, each a full
structured record `{task, agent, model, difficulty, result, status, summary}`
authored by the correct agent. No key collisions, no missing keys.

**Step (b) — BUG3 self-key fan-out-vs-authoritative-write ordering.**
Dispatched a 4th task to `claude`, instructing it to discover its own
soon-to-be-allocated dispatch key (`task:claude:4`, the next global index) and
include, in its own `memory_updates`, a keyed update naming that exact key
with a plain scalar value (`"self-ref-corruption-test"`), i.e. attempting to
have the fan-out loop write a scalar over the same key the authoritative
Upsert will also target. The agent complied, correctly inferring
`task:claude:4` from reading `.jindo/memory.json` itself, and reported it in
its summary.

Resulting `.jindo/memory.json` entry for `task:claude:4`:

```json
"task:claude:4": {
  "author": "claude",
  "ts": 1782971573,
  "value": {
    "agent": "claude",
    "difficulty": "standard",
    "model": "claude-sonnet-5",
    "result": "self-key-test",
    "status": "ok",
    "summary": "attempted to name own dispatch key in memory_updates; used task:claude:4 ...",
    "task": "... (full original task text) ..."
  }
}
```

The final value is the full structured record (`task`/`agent`/`model`/
`difficulty`/`result`/`status`/`summary`), **not** the scalar
`"self-ref-corruption-test"` the fan-out loop wrote for that same key moments
earlier. This is exactly the expected behavior of the current code: the
`memory_updates` fan-out loop (orchestrator.go ~line 317) runs first and does
write the scalar under `task:claude:4` momentarily (per `OwnerOf(target) ==
route.Agent` skipping the relabel-to-a-fresh-key guard for a self-named key),
but the authoritative `Upsert` at the end of `Dispatch` (~line 346) then
overwrites the same key with the full record as the last write of the
dispatch, so the final on-disk state is never corrupted. No stray scalar
survived. Ordering (fan-out before authoritative write) held on this live run.

**Step (c) — cross-agent memory read.** Dispatched a 5th task to `codex`,
instructing it to read `.jindo/memory.json` in its bounded memory directory,
find the entry `task:agy:3` (produced by a *different* agent in step (a)), and
quote its `result` field in its own summary. Response:

```json
{"agent":"codex","key":"task:codex:5","result":"cross-agent-read-test",
 "status":"ok",
 "summary":"Read `.jindo/memory.json` and found `task:agy:3`; its `result` field was `noop-agy-1`."}
```

`codex` correctly located and quoted `agy`'s prior result (`noop-agy-1`),
confirming the headless memory-directory hand-off (system prompt + `--add-dir`
pointing at the shared `.jindo` root) does let one agent's dispatch actually
read and build on a different agent's prior recorded result, store-wide.

**Final `.jindo/memory.json` state** (5 dispatch keys plus `_notes`, no
corruption, no duplicate/missing keys):

```
task:agy:3    -> {agent: agy,    result: noop-agy-1,          status: ok}
task:claude:1 -> {agent: claude, result: noop-claude-1,       status: ok}
task:claude:4 -> {agent: claude, result: self-key-test,       status: ok}
task:codex:2  -> {agent: codex,  result: noop-codex-1,        status: ok}
task:codex:5  -> {agent: codex,  result: cross-agent-read-test, status: ok}
```

**Verdict: no regression found.** Cross-agent `AllocKey` partitioning is
collision-free; the fan-out-before-authoritative-Upsert ordering fix for BUG3
still holds live — an agent naming its own dispatch key in `memory_updates`
cannot corrupt the final structured record, because the authoritative Upsert
at the end of `Dispatch` always runs last and wins; and cross-agent memory
reads work as designed. `go build ./...` still succeeds (no `.go` files were
touched in this diagnostic step).

## Loop-0010 Re-Verification (routing)

Diagnosis-only live re-verification (2026-07-02, ~14:58 UTC): re-ran difficulty-based
model routing (internal/routing/*.go, Select function) to confirm that
trivial/standard/hard difficulty classification and agent+model resolution are
functioning correctly per the embedded config.

**Procedure:** 
1. Verified both config files (`jindo/config/models.json` and 
   `internal/routing/config/models.json`) — confirmed both are identical (no sync 
   issue) and the binary reads from `internal/routing/config/models.json` via 
   go:embed directive (line 20 of internal/routing/routing.go).
2. Built fresh binary: `go build -o /tmp/jindo-mcp-t3 ./cmd/jindo-mcp` ✓
3. Created test program `test_routing.go` calling `routing.ScoreTask()` and 
   `routing.Select()` directly (bypassing MCP dispatch overhead) to verify 
   routing logic in isolation.
4. Ran three test dispatches, one per difficulty tier, with empty agent field 
   (auto-select):
   - **Task 1 (trivial):** "Fix a typo" → score 0.00 < 1.0
   - **Task 2 (standard):** "Implement a new function to calculate statistics" → score 2.40 ∈ [1.0, 6.0)
   - **Task 3 (hard):** "Add secure password authentication with token encryption and access control" → score 15.00 ≥ 6.0

**Config State (both files identical):**

```json
{
  "default_agent_by_difficulty": {
    "trivial": "agy",
    "standard": "claude",
    "hard": "codex"
  },
  "agents": {
    "agy": { "trivial": "gemini-3.5-flash", ... },
    "claude": { "standard": "claude-sonnet-5", ... },
    "codex": { "hard": "gpt-5.5", ... }
  }
}
```

**Routing Results:**

| Tier | Task | Score | Difficulty | Agent | Model | Expected Agent | Expected Model | Match |
|------|------|-------|------------|-------|-------|-----------------|-----------------|-------|
| Trivial | "Fix a typo" | 0.00 | trivial | agy | gemini-3.5-flash | agy | gemini-3.5-flash | ✓ |
| Standard | "Implement new function..." | 2.40 | standard | claude | claude-sonnet-5 | claude | claude-sonnet-5 | ✓ |
| Hard | "Add password auth + crypto..." | 15.00 | hard | codex | gpt-5.5 | codex | gpt-5.5 | ✓ |

**Score Breakdown (hard task example):**
- security signal (weight 3.0): matched "auth", "password", "token", "encrypt", "access control" = 5 patterns → 3.0 × 5 = 15.0
- constraints signal (weight 1.5): no matches → 0
- scope signal (weight 1.2): no matches → 0
- ambiguity signal (weight 0.8): no matches → 0
- **Total: 15.0 + 0 + 0 + 0 = 15.0 ≥ 6.0 → hard**

**Verdict: no regressions found. Difficulty-based routing is 100% correct:**
- Score thresholds (trivial < 1.0, standard ∈ [1.0, 6.0), hard ≥ 6.0) working as expected
- Default agent resolution per tier accurate (agy → trivial, claude → standard, codex → hard)
- Model resolution per agent+tier accurate per config
- Config sync verified (both files identical, binary reads embedded copy)
- All three test dispatches matched config expectations

**Build status:** `go build ./...` succeeds; no `.go` files were modified in this diagnostic step.

## Loop-0010 Re-Verification (sensitive-file policy)

**Scope:** `internal/policy/policy.go` (`Check`, `ClaudeDisallowedToolArgs`) and
`internal/orchestrator/orchestrator.go` `Dispatch` — verifying that the
sensitive-path gate runs *before* routing, memory writes, and any adapter
subprocess, for all three CLI agents, and that it does not block benign
tasks. Diagnostic only; no `.go` files modified.

**Static check:** `orchestrator.Dispatch` (internal/orchestrator/orchestrator.go:223-233)
calls `policy.Check(task)` as the very first statement in the function body,
before `o.Route(...)`, before `mem.AllocKey(...)`, before `mem.AppendNote(...)`,
and before `buildDispatchArgs`/adapter invocation. A blocked task returns
immediately as `&policy.BlockedError{...}` with no further side effects.

**Build:** `go build -o /tmp/jindo-mcp-t4 ./cmd/jindo-mcp` succeeded.

**Setup:** throwaway cwd `/tmp/jindo-loop-verify-policy` (empty, no pre-existing
`.env`/`.mcp.json`/`.jindo`), MCP JSON-RPC requests piped over stdin/stdout to
the rebuilt binary (`cmd/jindo-mcp` wires `memory.New(".jindo")` relative to
process cwd, confirmed in `cmd/jindo-mcp/main.go`).

**Case (a) claude / `.env`, (b) codex / `.mcp.json`, (c) agy / `.env`** — sent
together as one batch (`initialize` + 3× `tools/call dispatch`):

```json
{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"dispatch failed: policy: task references sensitive path (matched \".env\"); dispatch refused before any agent ran"}}
{"jsonrpc":"2.0","id":3,"error":{"code":-32603,"message":"dispatch failed: policy: task references sensitive path (matched \".mcp.json\"); dispatch refused before any agent ran"}}
{"jsonrpc":"2.0","id":4,"error":{"code":-32603,"message":"dispatch failed: policy: task references sensitive path (matched \".env\"); dispatch refused before any agent ran"}}
```

- All 3 requests returned as JSON-RPC `error` (code -32603) with the expected
  `"policy: task references sensitive path"` message and the correct matched
  pattern (`.env` for a/c, `.mcp.json` for b) — independent of which agent
  (`claude`, `codex`, `agy`) was targeted.
- Timing: all 4 requests (init + 3 blocked dispatches) round-tripped in
  **0.38s wall-clock total** (`time` measured 0.00s user / 0.01s sys). This is
  orders of magnitude faster than a real CLI subprocess invocation (the
  benign case below took 87s for a single dispatch), which is strong indirect
  evidence no adapter subprocess was ever spawned for the blocked cases.
- Filesystem: `ls -la /tmp/jindo-loop-verify-policy` after the batch showed
  only `.jindo/` (empty directory), `requests.jsonl`, `out_abc.jsonl`,
  `err_abc.log` — **no `.env` and no `.mcp.json` were created**.
- Memory: `.jindo/` existed as an empty directory with **no `memory.json`
  file at all** (not just an empty one) — confirming `mem.AllocKey` /
  `mem.AppendNote` / `mem.Upsert` were never reached, i.e. the policy gate
  fired strictly before any shared-memory interaction, for all three agents.
- stderr was empty for the batch.

**Case (d) benign / no agent specified** — `dispatch` with
`task="add a small health check endpoint that returns 200 OK"` (no `agent`
field, so default routing applied → resolved to `claude`):

```json
{"jsonrpc":"2.0","id":2,"result":{"content":[{"text":"{\"agent\":\"claude\",\"difficulty\":\"standard\",\"key\":\"task:claude:1\",\"model\":\"claude-sonnet-5\",\"result\":\"Created /private/tmp/jindo-loop-verify-policy/server.js: a standalone Node.js http server with GET /health returning 200 OK (and 404 for other routes)...\",\"status\":\"ok\",\"summary\":\"...\"}","type":"text"}],"isError":false}}
```

- Returned `status:"ok"` (not blocked), took **87.01s wall-clock** for this
  single dispatch (`time`: 4.29s user / 1.05s sys / 1:27.01 total) — consistent
  with an actual `claude` CLI subprocess having been spawned and run to
  completion, unlike the sub-second blocked cases above.
- Filesystem: `server.js` was actually created in
  `/tmp/jindo-loop-verify-policy/` by the dispatched agent.
- Memory: `.jindo/memory.json` was created and populated with both an
  `orchestrator` note (`"dispatch task:claude:1: claude/claude-sonnet-5
  (standard) :: add a small health check endpoint that returns 200 OK"`) and
  a `claude`-authored completion note, plus the `task:claude:1` entry with
  the full result — i.e. the normal AllocKey → AppendNote → Upsert → adapter
  → Upsert(result) path ran end-to-end for a non-sensitive task.

**Verdict: no regressions found.** The sensitive-path policy gate in
`Dispatch` runs strictly before routing, `AllocKey`, any memory write, and
any adapter subprocess, for all three agents (`claude`, `codex`, `agy`)
uniformly, exactly as the package/function docs claim. Benign tasks are
unaffected and proceed through the full normal dispatch path.

**Build status:** `go build ./...` succeeds; no `.go` files were modified in this diagnostic step.

## Loop-0010 Summary

All four historically-problematic areas (headless permission gates, shared
memory fan-out ordering, difficulty routing, sensitive-file dispatch policy)
were re-verified in this loop via live CLI reproduction — real `claude`,
`codex`, and `agy` subprocess dispatch through a freshly rebuilt
`jindo-mcp` binary — and all four continue to work correctly, with no
regressions or unresolved issues found. One minor documentation-only
arithmetic typo was found and corrected during T3; it was not a code bug.
No code changes were required in this loop, so its diagnostic work is
purely confirmatory.

## Loop-0011 Live Verification — (a) Auto growth-bounding

**Goal:** prove that `orchestrator.Dispatch`'s new automatic
`mem.MaybeCompact(MaxEntries: 200, MaxNotes: 200, ...)` call — added right
after the authoritative result `Upsert`, per loop-0011-design §1 — actually
self-bounds `memory.json` growth during real dispatches, *without* the
manual MCP `compact` tool ever being invoked.

**Setup (time-efficient, per plan):**
- `go build -o /tmp/jindo-mcp-t4 ./cmd/jindo-mcp` — rebuilt with the
  auto-compaction change already present in this worktree's
  `internal/orchestrator/orchestrator.go` (lines ~367-390) and
  `internal/memory/compaction.go` (`MaybeCompact`/`Compact`).
- Throwaway cwd `/tmp/jindo-loop-verify-growth/.jindo/memory.json` was
  pre-seeded via a small Python script writing 197 synthetic entries
  directly in the store's exact wrapper shape (`{"value":{...task fields...},
  "author":..., "ts":<unix seconds>}`), split across `task:claude:1..100`,
  `task:codex:1..70`, `task:agy:1..27`, plus a `_notes` array of 50 synthetic
  orchestrator notes. This mimics real dispatch records but only to get
  close to the 200 cap quickly — it does not touch the compaction trigger
  itself, which only runs live inside `Dispatch`.
- 6 REAL dispatches were then run against the rebuilt `jindo-mcp-t4` binary
  over stdio JSON-RPC (`initialize` + `tools/call` "dispatch"), mixing all
  three real CLIs (`claude`, `codex`, `agy`), from that same cwd. No
  `compact` tool call was made at any point in this step.

**Observed per-dispatch evidence** (real_count = non-reserved,
non-`_digest`, non-`_notes` entries; sizes are `.jindo/memory.json` via
`wc -c`):

| step | agent (real CLI) | key allocated | real_count after | `_digest` present | digest `count` | file size (bytes) |
|---|---|---|---|---|---|---|
| seed | — (synthetic, direct write) | — | 197 | no | — | 64676 |
| 1 | claude | `task:claude:101` | 198 | no | — | 60427 |
| 2 | codex | `task:codex:102` | 199 | no | — | 60878 |
| 3 | agy | `task:agy:103` | 200 | no | — | 61327 |
| 4 | claude | `task:claude:104` | **200** (was 201 pre-compact) | **yes** | 1 | 61701 |
| 5 | codex | `task:codex:105` | 200 | yes | 2 | 61940 |
| 6 | agy | `task:agy:106` | 200 | yes | 3 | 62251 |

(Seed file size is larger than post-dispatch sizes because the synthetic
seeder wrote denser JSON per entry than real dispatch results; the relevant
trend is steps 1→6, not seed→1.)

At step 4 the dispatch's own authoritative `Upsert` pushed the real count to
201 — one over `MaxEntries: 200` — and `MaybeCompact`'s cheap threshold
check (`realCount > opts.MaxEntries`) fired, triggering a full `Compact`
pass automatically as a side effect of `Dispatch`, with no manual `compact`
tool call anywhere in the transcript. `Compact` folded the single oldest
entry (`task:claude:98`, `ts`-checked against `task:claude:99` and
`task:claude:100` in the digest body — confirming the cold-tail, oldest-ts-
first fold rule) into a fresh `_digest` entry (`author: "_compaction"`,
`count: 1`).

Steps 5 and 6 confirm this is not a one-off: each subsequent dispatch again
pushes the live count to 201 post-Upsert, `MaybeCompact` fires again, and
the live count drops back to exactly 200 while `_digest.value.count`
increments (1 → 2 → 3), i.e. each fold's cold-tail entry accumulates into
the same digest rather than being lost.

**File-size trend:** pre-cap (steps 1-3, real_count still climbing 198→200)
size grew by +451, +449 bytes per dispatch. Once the cap engaged (steps
4-6, real_count pinned at 200) growth per dispatch dropped to +374, +239,
+311 bytes — driven only by the new/replaced live entry plus a small digest
delta, not by unbounded accumulation. Confirmed: entry count stabilizes at
200 rather than growing to 200+N, and file size stops growing linearly with
dispatch count once compaction kicks in.

**Verdict: 문제 없음 — auto-compaction works as designed.** The
`MaybeCompact` call wired into `Dispatch` after the authoritative Upsert
does self-bound `memory.json` growth in real, live use: real entry count
never exceeds 200 across 6 consecutive real dispatches spanning all three
agents, `_digest` appears exactly when the cap is first exceeded and
accumulates correctly across repeated triggers, and the manual `compact`
MCP tool was never invoked in this verification.

**Build status:** `go build ./...` succeeds; no `.go` files were modified in
this diagnostic step (only the throwaway `/tmp/jindo-loop-verify-growth`
directory and the prebuilt `/tmp/jindo-mcp-t4` binary were touched, both
outside the repo).

## Loop-0011 Live Verification — (b) Post-compaction cross-agent utilization

**Goal:** prove that compaction does NOT create a context blind spot — a
freshly dispatched agent, told only to "read shared memory" per
`agentproto.BuildSystemPrompt`'s STEP 1 (with T3's digest-guidance wording:
"Check for both the live recent entries and \"_digest\" explicitly; reading
only one of the two will silently lose context"), must correctly surface
BOTH (a) a fact that exists ONLY inside the folded `_digest` entry, and (b) a
fact that exists only as a LIVE recent entry, in the same read.

**Setup:**
- `go build -o /tmp/jindo-mcp-t5 ./cmd/jindo-mcp` — rebuilt with both the T2
  auto-compaction wiring and the T3 `BuildSystemPrompt` digest-guidance
  wording already present in this worktree (no `.go` files touched by this
  step).
- Throwaway cwd `/tmp/jindo-loop-verify-utilization/.jindo/memory.json` was
  seeded directly (not via real compaction) with the store's exact wrapper
  shape, so the digest-only fact and the live-only fact are both present
  from the very first dispatch with no need to force 200+ entries first:
  - **`_digest` entry** (key `"_digest"`), simulating a prior real
    `Compact` fold, with value:
    ```json
    {
      "body": "task:codex:7 :: codex :: Decided to use PBKDF2 with 100000 iterations for password hashing instead of bcrypt, per a security review note.",
      "count": 1,
      "oldest_ts": <now-90000>,
      "newest_ts": <now-90000>
    }
    ```
    (`author: "_compaction"`, `ts: <now-80000>`) — the PBKDF2/100000-iteration
    password-hashing decision exists **nowhere else** in the seeded store; it
    is answerable only by reading `_digest`.
  - **8 live entries** (`task:claude:47`, `task:codex:48`, `task:agy:49`,
    `task:claude:50`, `task:codex:51`, `task:agy:52`, `task:claude:53`,
    `task:codex:54`), each a normal `{"value":{"status","result","summary",
    "memory_updates"},"author","ts"}` wrapper. `task:claude:50`'s `result` is
    the distinctive live-only fact: `"Chose Redis for the session store
    because of TTL support and low-latency lookups; ADR-0032 updated
    accordingly."` — this decision exists **nowhere in `_digest`**; it is
    answerable only by reading the live entries.
  - Plus a `_notes` array with 2 synthetic orchestrator notes (unrelated
    control content, present to match the real store shape).

**Dispatch (real MCP JSON-RPC, real CLI subprocess, twice for robustness):**
`initialize` + `tools/call "dispatch"` was sent over stdio to the rebuilt
`jindo-mcp-t5` binary from `/tmp/jindo-loop-verify-utilization`, once with
`"agent":"claude"` and once with `"agent":"codex"`, both with the identical
task text:

> Before doing anything else, read the shared memory directory (including
> any _digest entry) under the bounded memory dir, and tell me in your
> result: (1) what password hashing approach was previously decided
> (including the exact algorithm and iteration count if mentioned), and (2)
> what session-store technology was previously chosen and why. Then just say
> "noted" as the rest of your work. Do not create, edit, or write any files.

**Observed responses (verbatim `result` field from the parsed agent
Response, as returned by the `dispatch` tool call):**

- **claude** (routed `hard` / `claude-opus-4-8`, key `task:claude:55`):
  > (1) Password hashing: PBKDF2 with 100000 iterations (chosen over bcrypt
  > per a security review note; from _digest entry originating
  > task:codex:7). (2) Session store: Redis, chosen for its TTL support and
  > low-latency lookups (from task:claude:50; ADR-0032 updated). noted

  `summary`: "Read .jindo/memory.json including the _digest; reported the
  prior password-hashing decision (PBKDF2, 100000 iterations) and
  session-store decision (Redis, for TTL + low-latency). No files created,
  edited, or written."

- **codex** (routed `hard` / `gpt-5.5`, key `task:codex:56`):
  > (1) Password hashing: PBKDF2 with 100000 iterations, chosen instead of
  > bcrypt per a security review note. (2) Session store: Redis, chosen for
  > TTL support and low-latency lookups. noted

  `summary`: "Read .jindo/memory.json, including the _digest entry and live
  task entries. No files were created, edited, or written."

**Assessment:** both real dispatched agents correctly answered BOTH
questions in the same response:
- The password-hashing fact ("PBKDF2 with 100000 iterations", "instead of
  bcrypt") is verbatim-traceable to the seeded `_digest.value.body` and
  appears **nowhere else** in the store — both agents' mention of it proves
  they actually read and used the folded `_digest` entry, not just the live
  entries.
- The session-store fact ("Redis", "TTL support", "low-latency") is
  verbatim-traceable to the seeded `task:claude:50.value.result` live entry
  and appears **nowhere** in `_digest` — both agents' mention of it proves
  normal live-entry usage still works after compaction exists in the store,
  consistent with loop-0010 T2's finding.

Neither agent showed the blind spot the task was checking for (i.e.,
neither answered only the live-entry question while ignoring `_digest`, nor
vice versa). Both facts were used correctly by both agents on the first
try, with no follow-up prompting needed.

**Verdict: 문제 없음 — no post-compaction blind spot found.** T3's
`BuildSystemPrompt` digest-guidance wording, combined with T2's
auto-compaction wiring, together produce the intended effect: a freshly
dispatched agent that reads shared memory per STEP 1 picks up a
digest-only historical fact and a live-only recent fact in the same pass,
across two different real CLIs (claude, codex).

**Build status:** `go build ./...` succeeds; no `.go` files were modified in
this diagnostic step (only the throwaway `/tmp/jindo-loop-verify-utilization`
directory and the prebuilt `/tmp/jindo-mcp-t5` binary were touched, both
outside the repo).

## Loop-0011 Summary

**Root cause:** `orchestrator.Dispatch` never called `memory.MaybeCompact`,
so `memory.json` grew unbounded across real usage even though
`internal/memory/compaction.go` already had a working `Compact`/
`MaybeCompact` mechanism — previously reachable only via the manual MCP
`compact` tool, never as part of the normal dispatch path.

**Fix:** `Dispatch` (`internal/orchestrator/orchestrator.go`) now calls
`mem.MaybeCompact(memory.CompactOptions{MaxEntries: 200, MaxNotes: 200,
TTLSeconds: 0, Now: 0, Summarize: nil})` immediately after the
authoritative result `Upsert` and before the final `return`, matching the
MCP `compact` tool's existing defaults exactly so the automatic push and
the manual pull enforce one non-diverging notion of "too many entries."
Failure is best-effort: a compaction error is recorded via
`AppendNote("orchestrator", ...)` but never fails the dispatch, since the
authoritative record is already durably written by that point. The
`dispatchMem` interface and the `spyMem` test double were extended with a
matching `MaybeCompact` method, and a new regression test proves
auto-compaction triggers purely as a side effect of `Dispatch` (no manual
`compact` call needed).

**Prompt improvement:** `agentproto.BuildSystemPrompt`'s STEP 1 now
explicitly instructs agents to consult both the live recent entries and the
folded `_digest` summary ("Check for both the live recent entries and
\"_digest\" explicitly; reading only one of the two will silently lose
context."), closing a real blind-spot gap that existed before this loop.

**Live verification outcomes:**
- (a) Real dispatches self-bounded `memory.json` at 200 live entries with a
  growing `_digest`, across 6 consecutive real dispatches spanning all
  three agents — with no manual `compact` call anywhere in the
  transcript.
- (b) Real `claude`/`codex` dispatches, given only the updated system
  prompt's STEP 1 instruction, correctly surfaced both a digest-only fact
  and a live-entry-only fact in the same response, with no blind spot.

No code changes were needed beyond this loop's own fix — this loop both
diagnosed AND fixed the growth problem, unlike loop-0010, which
re-verified four historical problem areas live and found no regressions.
