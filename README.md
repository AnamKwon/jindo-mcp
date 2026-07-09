# jindo — Tmux-persistent multi-agent orchestrator

jindo coordinates three real coding-agent CLIs (claude, codex, agy) in a persistent tmux session, routing tasks by difficulty and sharing context via file-locked JSON memory.

## Overview

jindo distributes coding tasks across multiple agents with smart model routing. Each agent (Anthropic Claude, OpenAI Codex, Google Gemini) runs in its own persistent tmux window. Task context flows between agents via a shared, file-locked memory store. Model selection is automatic and deterministic: a heuristic classifier reads the task description, assigns a difficulty tier (trivial / standard / hard), and the config file selects the best agent and model for that tier.

The design is inspired by Sakana AI's jindo orchestration pattern.

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

A single-binary, stdlib-only MCP server entry point is available at `cmd/jindo-mcp`. It ports the jindo orchestration core to Go and exposes it over JSON-RPC 2.0 on stdin/stdout — no runtime, no external dependencies.

- **Build:** `go build -o jindo-mcp ./cmd/jindo-mcp`
- **Zero third-party deps:** stdlib only; the routing policy and agent config are embedded at compile time.
- **Four tools:** `dispatch` (route + run a task), `memory` (read shared memory), `agents` (list routing table), `compact` (trigger memory compaction).

See [docs/jindo-mcp.md](docs/jindo-mcp.md) for full build instructions, MCP client registration, tool reference, and headless multi-agent design details.

## Cross-agent context sharing

All agents read and write the same shared memory at a configurable root path (default `.jindo`). When agent A completes a task, it stores the result in `memory.json`. When agent B is dispatched later, it can read all prior entries (including A's output) via the `Orchestrator.context()` method, providing full visibility across agents in the same orchestration session.
