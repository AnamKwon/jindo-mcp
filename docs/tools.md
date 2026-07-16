# jindo-mcp — Tool Catalog (auto-checked)

This is the **authoritative, test-verified** list of every tool the
`jindo-mcp` server registers via `tools/list`. It is kept in sync with the code
by `TestToolsDocInSync` in `internal/mcp/mcp_test.go`, which compares the tool
names in this table against `mcp.ToolCatalog()` in **both** directions: adding,
removing, or renaming a tool in the code without updating this file fails the
test. The descriptions below are each tool's first sentence; the full
descriptions live in the tool definitions in `internal/mcp/mcp.go`.

| tool | description (first sentence) |
|------|------------------------------|
| `route_capability` | Return host decision support without running a model: exact priors, analogous evidence with transfer warnings, the eligible policy catalog, task signals, evidence gaps, oracle checks, and reviewer policy. |
| `dispatch` | Run one explicitly selected model. |
| `dispatch_async` | Dispatch a coding task in the background and return immediately with a job_id (does not wait for the result). |
| `dispatch_multi` | Fan a task out to multiple models concurrently in read-only "propose" mode: each model returns its OWN complete candidate solution. |
| `dispatch_multi_async` | Async variant of dispatch_multi: fan a task out to multiple models concurrently in read-only "propose" mode and return immediately with a job_id instead of waiting for every candidate to finish. |
| `job_status` | Poll the status of an async dispatch job. |
| `plan` | Decompose a goal into an ordered step plan via a capable model AND establish it as the active, step-gated plan state. |
| `plan_next` | Return the next runnable step of the active plan plus the count of not-yet-done steps remaining. |
| `plan_record` | Record the outcome of a plan step: set its status to "done" or "failed" with an optional note. |
| `plan_revise` | Adapt the remaining active plan: append new steps, update fields of existing steps by id, and remove steps by id. |
| `plan_status` | Return the full active plan state, or `{plan: null}` when no plan is active. |
| `plan_gate` | The autonomous loop's stop gate: runs the active plan's integration verify_cmds AND a read-only goal-met judge to decide whether the loop may terminate. |
| `memory` | Read jindo shared memory: one key's value, or `{entries, insights}` when key is omitted. |
| `agents` | List the agent → difficulty → model routing table, plus a per-agent map reporting whether each agent's CLI is installed. |
| `models_refresh` | Probe each installed agent CLI for its actually-available models and report new models as unmeasured assessment requests without inferring a tier from their names. |
| `compact` | Trigger memory compaction to bound the working set. |
| `calibrate` | Aggregate dispatch.log into a routing calibration report. |
