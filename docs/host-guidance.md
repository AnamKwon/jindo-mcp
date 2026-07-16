## jindo MCP — delegate code work to the multi-agent orchestrator

If the jindo MCP tools (`dispatch`, `dispatch_async`, `job_status`, `plan`,
`plan_next`, `plan_record`, `plan_revise`, `dispatch_multi`, …) are available in
this session, prefer routing code generation / implementation / refactoring
through jindo instead of doing all the work yourself. (If the tools are absent,
ignore this section and work normally.)

- **Short tasks:** classify the request, call `route_capability`, inspect its
  evidence packet, then call
  `dispatch(task, model, selection_reason, effort, verify, review)`. Pin the
  host-chosen model and provide repository-specific verification. The built-in
  keyword scorer is only a no-capability fallback.
- **Target directory:** if the task must create or modify files in a specific
  directory (e.g. a scratch project under `/tmp/...`), pass `workdir` on the
  dispatch. The sub-agent is anchored there (its process cwd + write access) and
  `verify` commands run there. Omit `workdir` only when the work belongs in the
  host's current directory — otherwise the sub-agent falls back to that cwd and a
  requested absolute path may not be honored.
- **Long tasks:** `dispatch_async` → then poll `job_status(wait_sec=25)` until
  status is `done` or `error`. `running` is not a result — keep polling.
- **Multi-step work:** don't cram a whole plan into one dispatch. Use the
  step-state loop: `plan(goal)` → `plan_next` → classify that step's capability
  → `route_capability(capability=...)` → choose model(s) for that concrete step
  → dispatch with the same capability, explicit `selection_reason`, and
  `verify=step.suggested_verify, review=true`
  → `plan_record(step_id, "done"|"failed")` → optional `plan_revise` → repeat
  until `plan_next` reports no steps remain. `step.suggested_model` is a useful
  tier fallback, not a command to ignore stronger task-shape evidence. Decide the
  next step only after seeing the last one's result.
- **Single vs multi model — you decide.** Routine/clear work → single `dispatch`.
  Ambiguous, high-stakes, or answers-may-diverge work (design choices, hard bugs,
  tradeoffs) → `dispatch_multi(task, models:[...])` (read-only propose) and
  synthesize the candidates yourself, or pass `synthesis:"judge"` to have jindo
  synthesize one answer.
- **Implementation is the work; review is verification.** For "improve/fix/
  implement/refactor" the files must actually change — running only a review and
  leaving code unchanged is not done. Use jindo's built-in cross-model review via
  `dispatch(review=true)`; don't run a separate review MCP.
- **Do not benchmark every request.** Calibration narrows the candidate set;
  the current repository's tests decide whether this request may stop on the
  selected model. A passing cheap model is a valid answer even for a difficult
  prompt. A failing or unreviewed model is not.
- **Classify capability before difficulty.** Record six axes before choosing:
  `domain` (coding, mathematics, biology, ...), coding `language` when
  applicable, natural `prompt_language`, `task_type`, consequence `risk`, and
  `oracle` quality. Difficulty
  alone is not a capability profile: a model may be strong at Go concurrency and
  weak at Rust concurrency, or strong at mathematics and weak at biology. Call
  MCP `route_capability` for the evidence-bounded recommendation, then pass the
  same `capability` object to execution. The runtime-embedded source is
  `internal/routing/config/capability_policy.json`.
- **The host owns selection.** `route_capability` returns `candidate_evidence`
  and `host_selection`; it does not select a model. Candidate order is a
  benchmark prior, not an execution rule. For `dispatch`/`dispatch_async`, the
  host must provide an exact `model` and a concise `selection_reason`. For
  `dispatch_multi`/`dispatch_multi_async`, it must provide the exact `models`
  set and explain why comparison is warranted. Jindo records whether the chosen
  models were inside the benchmark candidate set but permits a reasoned override.
- **Evidence-guided host loop:** inspect the actual code path or question before
  choosing. Identify the invariant, ambiguity, change radius, context size,
  consequence of error, available oracle, and iteration budget. Then call
  `route_capability` and compare each candidate's directly observed strengths,
  cautions, repeatability, review defects, latency class, and operational
  failures against this request. Choose one model only when evidence is directly
  relevant, the work is bounded/reversible, and a strong oracle can reject a bad
  result. Choose multiple provider-diverse models when evidence is tied or
  unstable, the task is ambiguous/high-stakes, candidates cover different
  failure modes, or the oracle is weak. Record the choice, evidence used,
  uncertainty, and verify/review plan in `selection_reason`.
- **Benchmarks narrow hypotheses; they do not make rules.** The same small model
  can win a difficult cell and fail a nearby one: Flash Low repeatedly led
  several Python/Rust/SQL cells and corrected Go test generation, but review
  exposed unseeded Go-test gaps; Luna passed selected exact-numerical work but
  was 1/3 on Python contract-test generation; Haiku's results changed across
  languages, prompt languages, and fresh generations. Conversely, Opus had the
  cleaner Go-test review but exceeded the fresh-run timeout. The host must use
  these observations as task-specific tradeoffs, never as `difficulty -> size`
  or `task keyword -> model` mappings.
- **Acceptance remains gate-based.** Pass the strongest safe repository-owned
  verification commands and set `review=true` for every non-trivial edit.
  Accept only when the objective gate passes and
  `review_status.gate_passed==true`. On failure, reassess the failure mode and
  choose again from the evidence packet; do not blindly advance to the next
  listed model. If no candidate can satisfy the gate, report the concrete
  failure instead of looping or claiming success.
- **Language and subject boundary:** direct-CLI calibration now covers multiple
  exact cells across Go, Python, Rust, JavaScript, Java, SQL, and C++, including
  debugging, numerical, security, multi-file-refactor, and test-generation
  purposes. Test authorship is a separate capability from implementation and
  review: route by the language plus failure mode, then execute the generated
  tests against the correct implementation and a mutation set. Do not
  transfer one cell's winner to unrelated work in the same language, to shell or
  Swift, or to noncoding subjects. An exact unmeasured cell is a
  calibration request, not evidence that the largest model wins. Until that cell
  has repeated results, use the router's provider-diverse `parallel_compare`
  candidates and retain every answer for scoring.
- **Prompt-language overrides are exact, not global.** The paired Java fixture
  kept Spark first in English and Korean, but the paired SQL cohort changed from
  English Haiku 3/3 to Korean Haiku 1/3 while Korean Spark passed 3/3. A later
  English-only repetition put Flash Low first on the generic SQL route. Set
  `prompt_language` on the capability. The router uses the Korean SQL override
  only for that exact cell;
  it does not generalize “Korean means Spark” to other work.
- **Execution gates are mandatory on calibrated code routes.** A coding
  `dispatch` is rejected unless it includes objective `verify` commands and
  `review:true`. Explicit model pins do not bypass these gates. If a candidate
  fails either gate, diagnose the failed invariant and make a new host selection
  from the evidence packet with the same gates; the routing decision itself is
  not a test result.
- **HLE-like work:** classify the subject and reasoning form separately (for
  example `biology + short_answer`, `mathematics + formal_proof`, or `physics +
  exact_calculation`). Run the provider-diverse frontier candidates independently.
  Exact dataset answers, symbolic/numeric checks, and item-level rubrics outrank a
  judge model. Store accuracy by subject, prompt language, reasoning form, and
  difficulty; only promote a subject-specific default after repeated items show a
  stable advantage. Do not collapse all HLE questions into one global score.
  The current image-free multiple-choice evidence has 20 items per subject for
  Terra and Flash Medium, but only one repeat, so all six measured subjects stay
  `parallel_compare`: Terra leads mathematics (7/20) and CS theory (8/20), Flash
  Medium leads biology (8/20), chemistry (5/20), and physics (6/20), and history/
  humanities ties at 8/20. Terra completed all 12 batches with valid JSON; Flash
  Medium had two timeouts and two answer-set omissions. Treat those transport
  failures as routing evidence, not missing data to hide.
- **Reviewer models are a separate capability.** A 20-case Python/Rust labeled
  benchmark with three repeats measured defect recall, precision, known-good
  false positives, and repeatability. The evidence-ordered, provider-diverse pool
  is Terra → Gemini 3.5 Flash Medium → Fable. All three were perfect on this set;
  Spark, 5.4-mini, Haiku, Opus and Flash Low each missed at least one defect label.
  `dispatch(review=true)` excludes the author. Require every requested review to
  complete and the aggregate gate to pass; for a missing oracle also require a
  human-checkable gate. This ranking applies only to the seeded contracts, and
  author implementation scores never substitute for reviewer scores.
- **Calibration campaign:** `python3 bench/matrix.py` summarizes the required
  language-semantics, software-purpose, HLE subject/reasoning, multilingual, and
  reviewer-defect campaigns. `python3 bench/matrix.py --validate` checks the
  matrix contract. A capability cell needs at least 20 items and 3 repeats before
  promotion; otherwise retain a provider-diverse tie set. Unrun cells are a work
  queue, not evidence that they already have a winner.
- **Adaptive calibration:** for a new code cell, use
  `python3 bench/adaptive_run.py --tasks ... --models ... --output-dir ...`.
  It screens once without reviewers, retains provider-diverse objective passes,
  repeats only those candidates, and spends review tokens only on stable
  finalists. This is a calibration workflow, not something to run for every
  ordinary routed request.
- **Verification selection:** use commands already owned by the repository and
  make the risk observable. Typical safe examples are `go test ./...`,
  `go test -race ./...`, `pytest -q`, `npm test`, `npm run typecheck`, `cargo test`,
  and focused package/test paths. Do not invent a green but irrelevant command
  such as `go version`. If no adequate oracle exists, use provider-diverse
  comparison and require cross-model review; do not claim objective verification.
- **Provider inventory fallback** (availability map, not a routing rule):

  | tier | claude | codex | agy (display name) | effort |
  |---|---|---|---|---|
  | simple / mechanical | `claude-haiku-4-5` | `gpt-5.6-luna` | `Gemini 3.5 Flash (Low)` | `low` |
  | normal implementation | `claude-sonnet-5` | `gpt-5.3-codex-spark` | `Gemini 3.1 Pro (Low)` | `medium` |
  | hard / security / high-leverage | `claude-fable-5` | `gpt-5.5` | `Gemini 3.1 Pro (High)` | `high` |

  Use this table only when no capability evidence exists. The host must still
  justify its choice from the actual task. Effort represents reasoning depth,
  independently of model size: a bounded but subtle task may use a small model
  at `high`. codex has no `max` effort (clamps to `xhigh`); agy encodes effort
  in the model name so `effort` is ignored for it.
- **Sandbox:** jindo's `dispatch` spawns sub-agent processes. If your host
  sandboxes subprocess spawning (e.g. Codex's default `workspace-write`), run it
  with a full-access profile / elevated permissions or dispatch will be cancelled.
