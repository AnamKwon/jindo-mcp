# JINDO — Joint Intelligence Network for Distributed Orchestration

![Cute Jindo dog mascot orchestrating multiple coding-agent windows](assets/jindo-hero.png)

**JINDO** is a **J**oint **I**ntelligence **N**etwork for **D**istributed **O**rchestration:
it makes several coding-agent LLMs (Anthropic **Claude**, OpenAI **Codex**, Google
**Gemini**/`agy`) work as one. A host agent hands JINDO a task; JINDO routes it to
the right model(s), runs them headless, can have other models review the result
when requested, and returns it — sharing context across agents through a
file-locked JSON store.

The name maps to the design:
- **Joint Intelligence** — multiple models collaborate on one task: a single author,
  cross-model peer review, or a multi-model fan-out that is synthesized into one answer.
- **Network** — the pool of interchangeable agent CLIs (claude/codex/agy), detected at
  startup and routed to by capability, difficulty, and reasoning effort.
- **Distributed Orchestration** — a lean, stateless-per-task orchestrator that moves
  work and records outcomes; each sub-agent runs isolated, in its own process, from only
  the host-provided task plus bounded shared memory.

## Overview

JINDO distributes coding (and, via propose/answer mode, non-coding) tasks across
multiple agents with capability- and difficulty-aware routing. The host decides
*what* and *which model*; JINDO executes it, with optional peer review and
verification gates. Its primary
surface is a single-binary **Go MCP server** (`cmd/jindo-mcp`) that any MCP host
(Claude Code, Codex, `agy`) can register — see [INSTALL.md](INSTALL.md).

![JINDO architecture: the MCP host drives a plan / plan_next / dispatch / plan_record loop against JINDO's MCP tool layer, which dispatches through a router to agent CLIs; results return/store directly by default, with review/verify as a dashed side branch entered only when the host passes review=true or verify commands, before shared file-locked state](assets/jindo-architecture.svg)

The host remains the driver of that loop: `plan` returns structured steps and
establishes active plan state, then the host iterates
`plan_next` -> dispatch the returned step (optionally using the suggested model,
and optionally passing `review=true`/`verify` commands to gate it)
-> `plan_record` the outcome, optionally calling `plan_revise` to adapt the
remaining steps. JINDO never runs the loop unattended.

Key capabilities:
- **Difficulty routing + host override** — a deterministic scorer (with an optional
  LLM-assess fallback) picks a tier→agent→model, or the host pins `model`/`agent`/`effort`.
- **Reasoning-effort routing** — per-tier effort (low/medium/high…) applied per CLI.
- **Multi-model collaboration** — `dispatch(review=true)` fans out cross-model peer
  review (with a security checklist) and one bounded revision; `dispatch_multi` fans a
  task to several models and optionally has a judge synthesize the candidates.
- **Host-driven step loop** — `plan` decomposes a goal into steps and persists
  active plan state, but does not execute them; the host drives the loop
  itself via `plan_next` -> `dispatch` -> `plan_record` -> optional `plan_revise`,
  one adaptive step at a time.
- **Objective verify gate** — when verify commands are supplied, run allowlisted
  test/build/lint (+security scanners) commands and gate on the result, with bounded
  auto-revision on failure.
- **Self-improvement** — `calibrate` aggregates the dispatch audit log and can apply
  conservative routing tuning to a runtime overrides file.
- **Availability-aware** — agent CLIs are detected at startup; routing only uses the
  ones installed (with fallback), and the `agents` tool reports availability.
- **Async** — `dispatch_async` + `job_status` for long tasks beyond the MCP tool timeout.

> A legacy Python implementation (the sections below, `pip install`) is retained and
> untouched; the Go MCP server is the current, dependency-free entry point.

## Architecture

### memory.py — SharedMemory
File-locked JSON store at `<memory-root>/memory.json`. All agents read and write through the same lock (`memory.lock`), so mutations are atomic and safe across processes. Stores task entries (task text, agent/model/difficulty routing decision, result) and an audit trail of notes. Entry keys follow the form `task:<n>`.

### adapters.py — AgentAdapter & CLI bindings
Abstract AgentAdapter interface with concrete subclasses (ClaudeAdapter, CodexAdapter, AgyAdapter) wrapping the three agent CLIs. Each adapter's `run(task, model)` method constructs `[cli, -m model, task]` and executes it via subprocess, capturing stdout. The `_exec` method is the only subprocess seam, so tests can mock it and verify CLI invocation without launching real processes.

### tmux_manager.py — TmuxManager
Manages a single persistent tmux session with one window per agent (claude, codex, agy). The `_tmux` method is the only tmux seam; tests mock it to verify exact tmux commands. `ensure_session()` creates the session and per-agent windows once (idempotent). `dispatch(agent, command)` sends a command to an agent's window so the human can observe its execution. The session survives across multiple task dispatches.

### router.py — ModelRouter & LLM-primary routing with deterministic fallback
`ModelRouter.select(task, agent=None)` uses a hybrid two-tier strategy:
- **Tier 1 (LLM-primary)**: Optionally consults a small CLI router model (e.g. Claude Haiku) for a structured per-signal assessment when `llm_routing.enabled` is true (disabled by default for determinism).
- **Tier 2 (deterministic fallback)**: On any LLM failure/timeout/None, or when disabled, applies a cost-free multi-signal scorer (four signals: security, constraints, scope, ambiguity, from `jindo/config/routing_policy.json`; total ≥ 6.0 → hard, ≥ 1.0 → standard, else trivial).
- **Ambiguous band**: Deterministic totals within width=1.0 of any threshold qualify for LLM nudging; totals outside are fixed.

Returns: `{agent, model, difficulty, scores, reason, source ('llm'|'deterministic'), confidence (float|None)}`. If no agent override is given, uses the per-difficulty default from `jindo/config/models.json`. For LLM assessment details, see [`docs/llm_routing.md`](docs/llm_routing.md); for the deterministic scorer, see [`docs/routing_policy.md`](docs/routing_policy.md).

### orchestrator.py — Orchestrator
Wires together router + tmux + adapters + memory. `dispatch(task, agent=None)` routes a task (via the router), records the intent and result in shared memory under a `task:<n>` key, sends a visual command to tmux, and runs the adapter. Task entries are authored by "orchestrator" on write (before execution) and re-authored by the executing agent (after execution) so downstream agents see who produced each result.

### cli.py — Typer CLI
Three commands:
- `dispatch TASK [--agent NAME] [--session jindo] [--memory-root .jindo]` — route and run a task, print result as JSON
- `memory [--memory-root .jindo]` — print shared-memory contents (entries + notes) as JSON
- `agents [--config-path PATH]` — print agent/model routing config as JSON

## Model selection

Routing is deterministic and two-stage:

1. **Scoring stage** (no LLM): the four signals (security, constraints, scope, ambiguity) from `jindo/config/routing_policy.json` score the task description deterministically. Outside the ambiguous band (width=1.0 around each threshold), the tier is final.

2. **Hybrid stage** (optional): totals within the ambiguous band may consult an LLM "thinker" seam (``ModelRouter._llm_assess``), which is mockable and returns None by default, leaving the deterministic tier untouched. Production may override to consult a small model on close calls.

The tier is then resolved to an agent+model via `jindo/config/models.json`:
- **trivial** (fast/cheap): agy (Gemini 3.5 Flash)
- **standard** (balanced): claude (Sonnet 5)
- **hard** (capable): codex (GPT-5.5) — leads the highest-stakes tier as the strongest agentic-coding stack (GPT-5.3-Codex, the codex standard-tier model, is ≈85% Verified)

Rationale and benchmark data are in [`docs/model_policy.md`](docs/model_policy.md); scoring rules and signal weights in [`docs/routing_policy.md`](docs/routing_policy.md).

## Developer mode (dev_eval)

Enable optional A/B evaluation of the routed model against an alternative on the same task. Disabled by default (`dev_eval.enabled=false` in `jindo/config/routing_policy.json`). To enable: set `dev_eval.enabled=true`, choose `alternative_strategy` ('other_agent'|'other_tier'|'configured'), and configure `evaluator.weights` (objective/judge balance). On each dispatch, runs the routed model and one alternative in parallel, evaluates both outputs with a combined evaluator (objective seam: runs/tests/lint; judge seam: LLM rubric, weighted), and records ts/task/task_class/signals/chosen_model/alt_model/chosen_score/alt_score/winner to `<memory_root>/dev_eval/eval_history.jsonl`. Accumulated history feeds the router as an optional gated prior (only when dev_eval on + history present + no explicit agent override) that can override model selection when a task-class win-rate favors another model. Default off preserves prior behavior. Note: enabling doubles model calls per dispatch; mock seams keep tests deterministic.

## Model availability probe

`jindo/model_probe.py` provides best-effort per-agent model discovery via a mockable seam. The `probe_agent_models(agent, probe_seam)` function shells out to each agent's CLI (e.g. `claude models`) and parses the output tolerantly, returning a sorted list of available model IDs or `[]` on any CLI failure. The `reconcile(config, detected_by_agent)` function compares configured models (from `jindo/config/models.json`) against detected availability per agent, returning three lists: available (in both), missing (configured but not found), and extra (found but not configured). Real probing only runs on-demand when a caller passes the `default_probe_seam`; tests remain deterministic by providing mock probe data.

## Model profiles

Per-model capability benchmarks, strengths, and weaknesses are documented in [`docs/model_profiles.md`](docs/model_profiles.md), backed by [`jindo/config/model_profiles.json`](jindo/config/model_profiles.json). Each model profile includes official and reported SWE-bench Verified scores, pricing, and a normalized `capability_score` (0..1 scale, higher = more capable). The profiles feed both the capability-routing decision logic and internal consistency checks; see the doc for rationale and data sources.

## Capability-driven routing

When `capability_routing.enabled=true` in `jindo/config/routing_policy.json` (default `false`), the router uses a score-driven right-sizing strategy: it matches task difficulty to model capability and picks the cheapest model that clears the difficulty bar, falling back to the most capable model if none clears it. The strategy is defined by `jindo/capability.py:pick_model_by_capability`, which consults per-model `capability_score` values and maps each difficulty tier (trivial/standard/hard) to a minimum required capability (0.55/0.75/0.90 respectively). Selection precedence (never overridden): explicit `--agent` argument → capability score override → history prior → default tier-based selection. Default off preserves the existing deterministic two-stage routing behavior.

## Install & run

```bash
pip install -e .
```

Dispatch a task:
```bash
jindo dispatch "refactor the auth module" --memory-root .jindo
```

Query shared memory:
```bash
jindo memory --memory-root .jindo
```

List agent and model config:
```bash
jindo agents
```

Run a fully-mocked end-to-end demo (real Orchestrator, no subprocess/tmux/LLM):
```bash
python examples/demo.py
```

## Testing

```bash
pytest -q
```

The test suite exercises the real Orchestrator, SharedMemory, ModelRouter, and TmuxManager with mocks only at the subprocess boundaries (`_exec`, `_tmux`). This ensures the orchestration logic is sound while the test gate remains deterministic (no LLM calls, no tmux server required).

## Go MCP server

The single-binary, stdlib-only MCP server at `cmd/jindo-mcp` is JINDO's primary
entry point. It speaks JSON-RPC 2.0 over stdin/stdout — no runtime, no external
dependencies; the routing policy and agent config are embedded at compile time.

- **Build:** `go build -o jindo-mcp ./cmd/jindo-mcp` (or `make install` — see [INSTALL.md](INSTALL.md)).
- **13 tools:**
  - execution — `dispatch`, `dispatch_async`, `job_status`, `dispatch_multi`
  - planning/step loop — `plan`, `plan_next`, `plan_record`, `plan_revise`, `plan_status`
  - memory/routing — `memory`, `agents`, `compact`, `calibrate`

See [docs/jindo-mcp.md](docs/jindo-mcp.md) for the full tool reference, MCP client registration, and headless multi-agent design; [INSTALL.md](INSTALL.md) for one-command setup across Claude Code / Codex / `agy`.

## Cross-agent context sharing

All agents read and write the same shared memory at a configurable root path (default `.jindo`). When agent A completes a task, it stores the result in `memory.json`. When agent B is dispatched later, it can read all prior entries (including A's output) via the `Orchestrator.context()` method, providing full visibility across agents in the same orchestration session.
