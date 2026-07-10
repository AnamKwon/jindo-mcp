## jindo MCP — delegate code work to the multi-agent orchestrator

If the jindo MCP tools (`dispatch`, `dispatch_async`, `job_status`, `plan`,
`plan_next`, `plan_record`, `plan_revise`, `dispatch_multi`, …) are available in
this session, prefer routing code generation / implementation / refactoring
through jindo instead of doing all the work yourself. (If the tools are absent,
ignore this section and work normally.)

- **Short tasks:** call `dispatch(task, model, effort)`. Read the task, judge its
  difficulty yourself, and pin `model` + `effort` (menu below). jindo's built-in
  keyword scorer is only a fallback and misroutes (security keywords over-promote
  trivial work; keyword-free but genuinely hard work gets under-routed), so
  specify them.
- **Target directory:** if the task must create or modify files in a specific
  directory (e.g. a scratch project under `/tmp/...`), pass `workdir` on the
  dispatch. The sub-agent is anchored there (its process cwd + write access) and
  `verify` commands run there. Omit `workdir` only when the work belongs in the
  host's current directory — otherwise the sub-agent falls back to that cwd and a
  requested absolute path may not be honored.
- **Long tasks:** `dispatch_async` → then poll `job_status(wait_sec=25)` until
  status is `done` or `error`. `running` is not a result — keep polling.
- **Multi-step work:** don't cram a whole plan into one dispatch. Use the
  step-state loop: `plan(goal)` → `plan_next` → dispatch the returned step with
  `dispatch(task=step.prompt, model=step.suggested_model, verify=step.suggested_verify, review=true)`
  → `plan_record(step_id, "done"|"failed")` → optional `plan_revise` → repeat
  until `plan_next` reports no steps remain. Decide the next step only after
  seeing the last one's result.
- **Single vs multi model — you decide.** Routine/clear work → single `dispatch`.
  Ambiguous, high-stakes, or answers-may-diverge work (design choices, hard bugs,
  tradeoffs) → `dispatch_multi(task, models:[...])` (read-only propose) and
  synthesize the candidates yourself, or pass `synthesis:"judge"` to have jindo
  synthesize one answer.
- **Implementation is the work; review is verification.** For "improve/fix/
  implement/refactor" the files must actually change — running only a review and
  leaving code unchanged is not done. Use jindo's built-in cross-model review via
  `dispatch(review=true)`; don't run a separate review MCP.
- **Model / effort menu** (pin by the difficulty you judge):

  | tier | claude | codex | agy (display name) | effort |
  |---|---|---|---|---|
  | simple / mechanical | `claude-haiku-4-5` | `gpt-5.4-mini` | `Gemini 3.5 Flash (Low)` | `low` |
  | normal implementation | `claude-sonnet-5` | `gpt-5.3-codex-spark` | `Gemini 3.1 Pro (Low)` | `medium` |
  | hard / security / high-leverage | `claude-opus-4-8` | `gpt-5.5` | `Gemini 3.1 Pro (High)` | `high` |

  Borderline (concurrency, generics, algorithm design, security, cross-cutting
  refactors, subtle invariants) → lean **hard**; under-routing costs more than
  over-routing. codex has no `max` effort (clamps to `xhigh`); agy encodes effort
  in the model name so `effort` is ignored for it.
- **Sandbox:** jindo's `dispatch` spawns sub-agent processes. If your host
  sandboxes subprocess spawning (e.g. Codex's default `workspace-write`), run it
  with a full-access profile / elevated permissions or dispatch will be cancelled.
