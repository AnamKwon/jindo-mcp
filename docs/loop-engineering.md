# Loop Engineering Roadmap

This document synthesizes "loop engineering" — the practice of building agent
harnesses as stacked, self-correcting loops rather than single prompt calls —
and grounds it against what actually exists in this repository today:
`jindo` (the Go routing/dispatch/memory/calibration service) and the
`loop-run` plugin (the loop-engine MCP + `.claude` agents/skills that drive
step-by-step autonomous coding runs).

Sources synthesized: LangChain, "The Art of Loop Engineering"; plantis.ai,
"LoopCraft: The Art of Stacking Loops."

## 1. The 4-layer "stacked loops" frame

Both articles converge on the same idea from different angles: a mature
agent system is not one loop, it is four loops stacked on top of each other,
each operating at a different timescale and feeding the layer above it.

### L1 — Agent loop
The innermost loop: a model repeatedly calls tools, observes results, and
decides whether to call another tool or stop, until the task is done. This is
the "ReAct"-style loop most people mean when they say "agent." It runs
per-task, in seconds to minutes, and its unit of feedback is a single tool
result.

### L2 — Verification loop
Wraps L1: the agent's output (or an intermediate step's output) is scored
against a rubric or grader — a test suite, a linter, an LLM judge, an
acceptance command. A failing score is turned into feedback and fed back into
another L1 pass, rather than being accepted as final. This is what turns "the
model produced *an* answer" into "the model produced *a checked* answer." L2
is what makes L1 trustworthy enough to run unattended.

### L3 — Event-driven loop
Wraps L1+L2 in time: instead of a human typing a prompt to start each run,
an external event — a schedule (cron), a webhook, a new message in a channel,
a file change — triggers a background run of the L1/L2 pair. This is what
turns the harness from "something I invoke" into "something that runs
without me."

### L4 — Hill-climbing loop
The outermost, slowest loop, and where both articles say the compounding
advantage actually lives: production traces from L1–L3 (what tasks ran, how
they were routed, which verifications failed and why, what a human corrected)
are fed into an analysis agent whose job is to improve the harness itself —
its prompts, its tool set, its graders, its routing/model-selection policy —
not to solve one more task. L4 is a loop *over* the other three loops; each
of its iterations changes how L1–L3 behave on every subsequent run. Without
L4, a harness's quality is fixed at design time; with it, the harness gets
better every time it is used.

Layers stack strictly downward in dependency: L4 has nothing to analyze
without L3's volume of runs or at least L1/L2's traces; L3 has nothing
worth triggering unattended without L2 to keep it honest; L2 has nothing to
grade without L1 producing an answer to grade.

## 2. Cross-cutting principles

- **Write loops, not prompts.** The unit of engineering effort is the
  control flow around the model (what triggers it, what checks it, what
  feeds back), not the wording of a single instruction.
- **Remove the human bottleneck.** Every point where a loop currently
  pauses for a person to approve, judge, or restart is a candidate for L2/L3
  automation; the goal is unattended operation with the human moved to
  reviewing outcomes, not gating each step.
- **Active memory management, not append-only.** Memory should be
  extracted, transformed, and committed (superseded entries dropped, cold
  history folded into a digest) — an ever-growing log degrades every
  downstream read and eventually poisons the context window.
- **Descend for reliability, ascend for leverage.** Route routine work to
  cheaper/smaller/more deterministic execution paths (descend) and reserve
  expensive, high-context, high-reasoning paths (ascend) for the work that
  actually needs them; this is a routing decision, not a one-size model
  choice.
- **Provider-agnostic routing.** Which model/agent executes a task should
  be a resolved decision (based on task signals and difficulty), not a
  hardcoded choice, so the harness can add/swap models without rewriting
  call sites.
- **Observability-first.** Every dispatch, verification, and routing
  decision should leave a structured, replayable trace before anything
  else is built on top of it — L4 (and debugging) is impossible without it.
- **No silent degradation, no silent truncation.** When a loop hits a
  limit (budget, size, retries, a truncated log/context), it must surface
  that fact in the trace/result rather than quietly returning a partial or
  degraded answer as if it were complete.

## 3. Gap analysis

### 3a. jindo

**L1 — Agent loop: present.**
`internal/orchestrator.Dispatch` / `DispatchWithReview` (`internal/orchestrator/orchestrator.go`)
run a single agent invocation end to end via `runAuthor`, exposed over MCP as
the `dispatch` tool (`internal/mcp/mcp.go`, tool `"dispatch"`). Task routing
into a tier + concrete agent/model is deterministic:
`routing.ScoreTask` (`internal/routing/routing.go`) scores a task against
weighted signal patterns into a tier (`trivial`/`standard`/`hard`), and
`routing.Select` resolves that tier to an `agent`+`model` pair (honoring an
explicit `agent` override, or intra-tier profile-based selection, or the
configured default). `dispatch_async` + `job_status`
(`internal/mcp/mcp.go`) add a fire-and-poll variant with an explicit polling
contract for long tasks, so L1 doesn't block the MCP call.

**L2 — Verification loop: present, opt-in.**
`Dispatch(..., review:true)` / `runReview` (`internal/orchestrator/orchestrator.go`)
route the author's result to a second agent selected via
`routing.SelectReviewer` (cross-model by construction: it excludes the
author's own agent — `internal/routing/routing.go`), and on a critical
finding trigger one revision round (`reviewOnce`/`runReview`). This is
opt-in per-call (`review` bool on the `dispatch`/`dispatch_async` MCP tools),
not automatic — L1 can run without ever being checked.

**L3 — Event-driven loop: absent.**
There is no schedule/webhook/channel trigger anywhere in jindo; every
dispatch is invoked synchronously by an MCP caller (`dispatch`,
`dispatch_async`+`job_status`). `dispatch_async` moves work off the calling
thread but is still caller-initiated, not event-initiated.

**L4 — Hill-climbing loop: DONE (jindo F1, this loop).**
Every dispatch appends one line to `dispatch.log`
(`internal/orchestrator.appendDispatchLog`, `dispatchLogFile` constant,
`internal/orchestrator/orchestrator.go`), a JSONL audit trail carrying the
routing rationale, tier, model, status, duration, and review outcome — this
is the raw material L4 needs. `internal/calibrate.Load` +
`Report.buildSuggestions` (`internal/calibrate/calibrate.go`) read that log
and aggregate it into a report: per-tier non-ok rates, signals that never
fired, near-threshold dispatch rates, per-model review changes-requested
rates — then turn breaches of `failureRateThreshold`/`minSample` into
human-readable suggestion strings. As built in this loop, the previously
open loop now closes end to end:

- **Runtime override layer.** `routing.ApplyOverrides(path)`
  (`internal/routing/routing.go`) overlays a small, writable subset of
  routing tunables — `thresholds[tier]`, `signal_weights[name]` (onto an
  existing signal's `Weight`; unknown names ignored), `assess_bands[name]` —
  from a JSON file (`.jindo/routing_overrides.json`) onto the `go:embed`
  compiled defaults built at `init()`. An absent file is a no-op (defaults
  untouched, fully backward compatible); malformed JSON returns an error
  with the defaults left fully intact (the overlay is built on a clone and
  committed only after a successful parse). It is applied once at
  `jindo-mcp` startup (`cmd/jindo-mcp/main.go`, calling
  `routing.ApplyOverrides(".jindo/routing_overrides.json")`).
  `routing.Thresholds()` is the accessor that exposes the live (overlaid)
  baseline to callers such as `calibrate`.
- **Findings feedback (b2).** The calibrate report now aggregates recurring
  review findings: `Report.ReviewFindingTotals` sums Critical/Major/Minor/Info
  findings across all reviewed dispatches, and a new per-author-model
  `CriticalMajor` counter feeds `buildSuggestions`, which emits an advisory
  string when a model's critical/major finding rate exceeds
  `failureRateThreshold` over at least `minSample` reviewed dispatches.
- **Gated apply (b3).** The MCP `calibrate` tool (`internal/mcp/mcp.go`,
  `callCalibrate`) gained an `apply` boolean (default `false`) and an
  `overrides_path` string (default `.jindo/routing_overrides.json`).
  `apply:false` remains report-only and byte-identical to the pre-apply
  behavior — nothing is written. `apply:true` calls
  `Report.DeriveOverrides()` (`internal/calibrate/calibrate.go`), which for
  each gated tier (`standard`, `hard`) checks its non-ok outcome rate against
  `failureRateThreshold`/`minSample` and, if flagged, nudges that tier's
  gating threshold UP by a fixed, conservative `thresholdNudge` (0.5) from
  the LIVE baseline (`routing.Thresholds()`) — everything else
  (never-matched signals, near-threshold rate, review findings) stays
  advisory-only in `Suggestions`, never a mechanical delta. The result is
  marshaled (`Overrides.Marshal`) and written to `overrides_path`, the exact
  file `routing.ApplyOverrides` reads back — closing the
  read-traces → tune-harness loop: `dispatch.log` → `calibrate` (report) →
  `calibrate(apply:true)` (derive + write overrides) →
  `routing.ApplyOverrides` (consumed at next startup).

Known limitation, stated deliberately rather than hidden: the nudge is
monotonic/one-way (`DeriveOverrides` only ever raises a flagged tier's
threshold, never lowers one), so repeated `calibrate(apply:true)` passes
accumulate rather than settle to an equilibrium on their own. This is why
`apply` defaults to `false` and remains an opt-in, human-triggered action
rather than an automatic post-dispatch hook.

Related standing mechanisms that support the loops above: memory compaction
(`internal/memory.MaybeCompact`, `internal/memory/compaction.go`) runs a
cheap threshold check after each dispatch and only performs a full
`Compact` (drop superseded/expired entries, fold a cold tail into a digest,
cap notes) when a limit is actually exceeded — the "active memory
management" principle applied to jindo's own shared memory store, also
exposed manually via the `compact` MCP tool.

### 3b. loop-run plugin (loop-engine MCP + `.claude` agents/skills)

**L1 — Agent loop: present, explicitly staged.**
The `loop-run` SKILL (`~/.claude/skills/loop-run/SKILL.md`) defines
CLARIFY → PLAN → STEP loop → STOP as the orchestrator's stages. Each STEP
dispatches to a role-matched, difficulty-routed executor —
`trivial`→`loop-explorer` (Haiku), `standard`→`loop-worker` (Sonnet, this
agent), `hard`→`loop-worker-deep` (Opus) — inside an isolated git worktree
(`loop_worktree_create`/`loop_worktree_prepare`), which is L1 run per-step
rather than per-whole-task.

**L2 — Verification loop: present, and it is the load-bearing gate.**
Every step carries its own `accept` criteria (allowlisted, shell-free), and
`loop_run_verify(..., kind:"round")` runs that step's `accept`; the
`loop-verifier` role then judges goal-fit on top of the raw pass/fail. A
step is `done` only if both the accept command and the verifier agree
(SKILL.md, "STEP VERIFY (the gate)"); a fail feeds `loop_record_result`,
which drives retry with a changed approach up to `max_attempts` before
auto-escalating to `blocked`. An `integration` kind of `loop_run_verify`
plus a `goal_met` judgment gates the whole run's STOP condition. This is a
materially stronger L2 than jindo's opt-in review: it is mandatory, and it
gates merge (`loop_worktree_merge` only runs after a passing step).

**L3 — Event-driven loop: partial.**
Nothing here fires on a webhook/channel event, but the separate `loop`
skill lets a prompt/slash-command (including `/loop-run` itself) be run on a
recurring interval, and SKILL.md notes it explicitly: "for unattended
multi-hour runs, the user can wrap this with `/loop` so it re-invokes and
`resume` continues the same loop_id." That is a schedule-style trigger
layered on top of L1/L2, but it is opt-in tooling bolted on from outside,
not a first-class trigger inside the loop-engine MCP itself (no webhook/queue
listener).

**L4 — Hill-climbing loop: absent (observe-only).**
`loop_observe` records every stage (find_work/task_result/verify/
diagnostics/merge/replan/stop) into an `events.jsonl`-style trace under
`.claude/loop/` (confirmed present in this worktree:
`.claude/loop/state/loop-0001.json`), and `loop_replan` re-plans the
*remaining task list* every round based on what a step just revealed — but
that re-planning only prunes/refines/adds *tasks within the current run*.
Nothing reads accumulated traces *across* runs to change the harness itself:
the `loop-planner`/`loop-worker`/`loop-verifier` prompts, the
`accept`-command conventions, the difficulty-routing thresholds, or the
worktree/merge mechanics are all fixed by the SKILL.md and agent
definitions and are never rewritten by the loop. This is the same gap as
jindo's calibrate was before this loop: rich per-run trace, no agent that
consumes traces across runs to improve the harness. Unlike jindo's F1, this
side of the gap is DESIGN-ONLY as of this loop — see roadmap item P1 below;
it was intentionally not implemented here because the loop-engine MCP server
described by this gap analysis is the same server that was running this
loop, so modifying it live was out of scope.

**Summary:** L1 and L2 are the strongest layers in both systems — jindo's
deterministic scorer/router plus opt-in cross-model review, and the plugin's
mandatory per-step accept+verifier gate. L3 exists only as bolt-on/opt-in
scheduling in the plugin and not at all in jindo. L4 used to be the clear gap
in both: jindo's `calibrate` computed real suggestions from real trace data
but was contractually suggest-only, and the plugin's `loop_observe` trace is
rich but nothing reads it across runs. As of this loop, jindo's side is
closed (F1: `calibrate(apply:true)` → `routing.ApplyOverrides`, see 3a
above); the plugin's side remains open — its hill-climbing meta-loop is
still only a proposal (P1, design-only, not implemented this loop because
the loop-engine MCP server in question was the one running this loop).

## 4. Prioritized roadmap

### Plugin (loop-run / loop-engine)

- **P1 — Hill-climbing meta-loop (DESIGN-ONLY, not implemented this loop).**
  Add a stage/agent that periodically reads accumulated
  `.claude/loop/state/*.json` + trace events across multiple completed loops
  and proposes concrete harness edits (planner prompt wording,
  `accept`-command patterns that keep failing, difficulty routing) —
  closing the one gap both source articles call out as where the compounding
  advantage lives. This is the TypeScript/loop-engine-MCP analogue of
  jindo's F1 (below), which this loop DID implement; P1 stayed design-only
  because the loop-engine MCP server this gap analysis describes is the same
  server driving this loop, so live-modifying it while it was in use was out
  of scope for this run.
- **P2 — LLM rubric grader for non-command steps.** Today `accept` must be
  an allowlisted shell command; steps whose "done" is qualitative (docs,
  design judgment) have no first-class L2 gate. An LLM-judge grader
  extends verification coverage to that class of task.
- **P3 — Descend/ascend tier escalation mid-run.** A step stuck at
  `loop-worker` after repeated `failed` results currently only escalates to
  `blocked`; auto-promoting to `loop-worker-deep` (ascend) before declaring
  blocked would save runs that just needed more reasoning budget.
- **P4 — Active memory-maintenance loop.** `.claude/loop/state/*.json`
  grows per loop with no compaction analogous to jindo's `MaybeCompact`;
  long-running or frequently-resumed loops will eventually pay a read/context
  cost proportional to history rather than to open work.
- **P5 — Event triggers + no-silent-truncation.** Give loop-engine a native
  trigger surface (webhook/schedule) instead of relying on the external
  `/loop` wrapper, and make `loop_observe`/verifier explicitly flag when a
  trace, diff, or log was truncated rather than silently showing a partial
  view.

### jindo

- **F1 (THIS LOOP) — calibrate becomes hill-climbing. DONE.**
  `internal/calibrate`'s suggestions now have a runtime override layer that
  `routing.ScoreTask` consults on top of the embedded `go:embed` policy:
  `routing.ApplyOverrides(".jindo/routing_overrides.json")` overlays
  `thresholds`/`signal_weights`/`assess_bands` at `jindo-mcp` startup
  (`cmd/jindo-mcp/main.go`), absent-file is a no-op and malformed input
  leaves defaults intact, and `routing.Thresholds()` exposes the live
  baseline. The calibrate report also aggregates recurring review findings
  (`Report.ReviewFindingTotals`, per-model `CriticalMajor`) into an advisory.
  The MCP `calibrate` tool's new `apply`/`overrides_path` parameters let an
  operator go straight from report to `Report.DeriveOverrides()`'s
  conservative one-step-up threshold nudge to a written overrides file that
  `routing.ApplyOverrides` round-trips — turning L4 from "prints advice"
  into "advice can actually close the loop," gated behind an explicit
  `apply:true` because the nudge is monotonic (threshold-raising only) and
  accumulates across repeated applies.
- **F2 — Rubric-scored review.** `runReview`'s critical-finding trigger is
  currently binary (revise or not); scoring review findings against a
  rubric would let `calibrate`'s per-model changes-requested-rate
  suggestions (already computed in `buildSuggestions`) become more precise
  and actionable.
- **F3 — Tier escalation on repeated `review_failed`.** `dispatch.log`
  already carries enough signal (`Rationale.Tier`, review outcome) to detect
  a task that keeps failing review at its assigned tier; auto-escalating the
  tier/model on retry (rather than requiring a human to read `calibrate` and
  hand-edit thresholds) is the same descend/ascend principle P3 applies to
  the plugin.
- **F4 — Event-driven dispatch.** Add a trigger surface (webhook/schedule)
  that calls `Dispatch`/`DispatchWithReview` directly, so jindo can run
  unattended the way `dispatch_async`+`job_status` already assume a caller
  will poll — completing jindo's L3 rather than leaving it caller-initiated
  only.
