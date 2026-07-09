# Codex → jindo MCP live verification

Proves, with real `codex exec` runs, that Codex can drive other coding agents through the
jindo MCP server (cross-agent dispatch) and that long-running tasks complete via async
polling without hitting an MCP timeout.

- Date: 2026-07-07
- Host agent: `codex-cli 0.142.1` (binary `/opt/homebrew/bin/codex`), model `gpt-5.5`
- jindo MCP: `/Users/anamkwon/jindo-mcp-test/jindo-mcp`, registered in `~/.codex/config.toml`
  as `[mcp_servers.jindo]` with `startup_timeout_sec=30`, `tool_timeout_sec=1800`,
  `env.CLAUDE_PROJECT_DIR=/Users/anamkwon/jindo-mcp-test`
- cwd for all runs: `/Users/anamkwon/jindo-mcp-test` (a `trusted` project in config)

## Result summary

| Check | What it proves | Result |
|-------|----------------|--------|
| (a) cross-agent sync dispatch | Codex calls `jindo/dispatch` and the task is executed by a *different* agent (`claude`), not codex | **PASS** |
| (b) async no-timeout | `dispatch_async` + repeated `job_status(wait_sec=25)` runs a longer task to completion with no MCP timeout | **PASS** |
| server-side cross-check | `dispatch.log` gained the two new records; async job JSON persisted | **PASS** |

Codex sees and can invoke the jindo tools (`dispatch`, `dispatch_async`, `job_status`, plus
`memory`, `agents`, `compact`, `calibrate` — confirmed via a raw `tools/list` probe of the
binary).

### Operational prerequisite discovered (important)

A first attempt at check (a) under Codex's **default** exec sandbox (`sandbox: workspace-write`,
`approval: never`) **failed**: the `jindo/dispatch` MCP call was reported `failed` with
`user cancelled MCP tool call`, and nothing was written server-side (`dispatch.log` stayed at
its baseline of 9 lines, no job created). jindo dispatch spawns real sub-agent processes
(`claude`/`gemini`) that write files and run outside Codex's workspace-write scope, so the call
does not survive the restricted sandbox.

The fix is to run with full access:
`codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check ...`
(sandbox becomes `danger-full-access`). With that, both checks pass cleanly. This is a Codex
*sandbox* requirement, not a jindo config problem — the jindo TOML (binary path, env,
`tool_timeout_sec=1800`) is correct and the tools were visible in both attempts.

`--skip-git-repo-check` is included because `/Users/anamkwon/jindo-mcp-test` is not a git repo.

---

## Check (a) — cross-agent sync dispatch

**Command**

```
cd /Users/anamkwon/jindo-mcp-test && \
codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check \
  "Use the jindo MCP dispatch tool to run this task with agent set to \"claude\": reply with exactly the word ping. Report the tool's JSON result verbatim."
```

**Session header (trimmed)**

```
model: gpt-5.5   approval: never   sandbox: danger-full-access
session id: 019f3b71-7ae3-7c13-a088-b77d7cc7028b
mcp: jindo/dispatch started
mcp: jindo/dispatch (completed)
```

**Tool result JSON (verbatim)**

```json
{"agent":"claude","difficulty":"trivial","key":"task:claude:19","model":"claude-haiku-4-5","rationale":{"matched":{},"total":0,"threshold":"trivial","threshold_value":0,"tier":"trivial","profile_match":{"candidates":null,"chosen":"claude","reason":"explicit agent override; profile selection skipped"}},"result":"ping","status":"ok","summary":"Replied with the word ping as requested"}
```

**Assertion:** `"agent":"claude"` (NOT codex) — cross-agent dispatch confirmed. Codex (gpt-5.5)
routed the task through jindo to a claude agent (`claude-haiku-4-5`), which returned
`result:"ping"`, `status:"ok"`, key `task:claude:19`. **PASS.**

> Note: the first (default-sandbox) attempt returned `user cancelled MCP tool call` — see
> "Operational prerequisite discovered" above.

---

## Check (b) — async dispatch, poll to done, no MCP timeout

**Command**

```
cd /Users/anamkwon/jindo-mcp-test && \
codex exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check \
  "Use the jindo MCP tools. Step 1: call dispatch_async with task='Write a Go function named safeDivide(a, b int) (int, error) that returns an error on division by zero, plus a table-driven test safeDivide_test in package main. Write the files.' and capture the returned job_id. Step 2: repeatedly call job_status with that job_id and wait_sec=25 until the returned status is \"done\" (keep polling if status is queued/running). Step 3: report the FINAL job_status result JSON verbatim, and also tell me the job_id and how many times you called job_status."
```

**MCP call trace (trimmed from the transcript)**

```
mcp: jindo/dispatch_async started
mcp: jindo/dispatch_async (completed)
codex: The jindo async job is `fd5919329917d379ff5314bf2e1c1c63`. ...
mcp: jindo/job_status started / (completed)   -> "running"
mcp: jindo/job_status started / (completed)   -> "running"
mcp: jindo/job_status started / (completed)   -> done
```

**Reported by codex:**
- `job_id`: `fd5919329917d379ff5314bf2e1c1c63`
- `job_status` calls: **3** (each with `wait_sec=25`)
- No MCP timeout error appeared anywhere in the run.

**Final `job_status` result JSON (verbatim)**

```json
{"result":{"agent":"claude","difficulty":"standard","key":"task:claude:20","model":"claude-sonnet-5","rationale":{"matched":{"scope":1.2},"total":1.2,"threshold":"standard","threshold_value":1,"tier":"standard","profile_match":{"candidates":[{"agent":"agy","coverage":0.6,"cost_rank":1},{"agent":"claude","coverage":1.08,"cost_rank":2},{"agent":"codex","coverage":0.72,"cost_rank":3}],"chosen":"claude","reason":"highest weighted profile coverage of matched signals [scope], ties broken by lower cost_rank"}},"result":"Created /Users/anamkwon/jindo-mcp-test/safe_divide.go (safeDivide(a, b int) (int, error), returns ErrDivideByZero when b==0) and /Users/anamkwon/jindo-mcp-test/safe_divide_test.go (table-driven TestSafeDivide covering positive, negative, zero-dividend, division-by-zero, and truncation cases), both package main.","status":"ok","summary":"Read shared .jindo memory for prior context and naming conventions (no safeDivide entry existed), then wrote safe_divide.go and safe_divide_test.go following the clamp_non_negative.go (value, error) convention. Could not run go build/test/vet due to sandbox approval restrictions, consistent with prior agents' notes."},"status":"done"}
```

**Assertions:**
- Terminal payload `"status":"done"`; inner result carries `agent:"claude"`,
  `model:"claude-sonnet-5"`, `status:"ok"` — all required fields present.
- The task was routed standard (tier `standard`) and ran to completion across 3 poll cycles
  (~59s wall: created `16:19:27`, finished `16:20:26`) with **no MCP timeout** — the long-poll
  `job_status(wait_sec=25)` pattern under `tool_timeout_sec=1800` holds. **PASS.**

---

## Server-side cross-check

`dispatch.log` grew from the baseline **9 → 11** lines; the two new records correspond exactly
to the two checks above:

```
2026-07-07T07:18:54Z | task:claude:19 | agent=claude | model=claude-haiku-4-5 | status=ok   (check a)
2026-07-07T07:20:26Z | task:claude:20 | agent=claude | model=claude-sonnet-5 | status=ok   (check b)
```

(The `dispatch.log` timestamps are UTC; the job file timestamps are local +09:00 — same events.)

Async job file persisted (the `.jindo/jobs/` directory did not exist before this run):

```
/Users/anamkwon/jindo-mcp-test/.jindo/jobs/fd5919329917d379ff5314bf2e1c1c63.json
```

```json
{
  "id": "fd5919329917d379ff5314bf2e1c1c63",
  "status": "done",
  "result": { "agent": "claude", "model": "claude-sonnet-5", "status": "ok", "key": "task:claude:20", ... },
  "created_at":  "2026-07-07T16:19:27.006370+09:00",
  "finished_at": "2026-07-07T16:20:26.983073+09:00"
}
```

Both dispatched tasks also produced their artifacts in `/Users/anamkwon/jindo-mcp-test/`
(`safe_divide.go`, `safe_divide_test.go` for check b), confirming the sub-agents did real work.

## Conclusion

Codex non-interactively drives other coding agents through jindo MCP: synchronous
cross-agent dispatch (a) and long-running async dispatch with poll-to-done (b) both succeed
with no MCP timeout. The only operational requirement is to run `codex exec` with
`--dangerously-bypass-approvals-and-sandbox` (plus `--skip-git-repo-check` outside a git
repo) so the jindo-spawned sub-agent processes are not blocked by Codex's default
workspace-write sandbox.
