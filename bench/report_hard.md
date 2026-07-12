# jindo benchmark — hard suite (codex-only), two tiers

CODEX-ONLY (claude/agy tokens exhausted). Goal: measure whether JINDO's objective
verify gate + self-heal (auto-revision on verify failure) LIFTS a single agent's
verified success on hard tasks vs raw codex — run at two codex tiers to try to
induce first-try failures.

Suite (5 hard, hermetic, verify-gated tasks): concurrency LRU+TTL under `-race`,
arithmetic expression evaluator, interval merge, a data-race bug fix under
`-race`, topological sort with cycle detection (Korean prompt). Ground truth =
each task's own verify (incl. `-race`) re-run after merge.

## KPI

| model | config | verified pass | avg secs | avg revisions |
|---|---|---|---|---|
| gpt-5.5 (hard tier)     | codex_raw    | 5/5 (100%) | 111.0 | 0 |
| gpt-5.5 (hard tier)     | codex_verify | 5/5 (100%) | 112.7 | 0 |
| gpt-5.4-mini (triv tier)| codex_raw    | 5/5 (100%) |  89.5 | 0 |
| gpt-5.4-mini (triv tier)| codex_verify | 5/5 (100%) |  85.9 | 0 |

## JINDO self-heal lift: +0 percentage points (both tiers)

Both codex tiers solved all 5 tasks FIRST TRY (0 auto-revisions anywhere), so the
verify gate + self-heal loop never engaged and produced no success lift.

## Honest conclusion

- For **well-specified, self-contained** tasks — the kind a hermetic benchmark
  naturally contains — even the CHEAP codex tier succeeds one-shot. JINDO's
  review/verify/self-heal machinery then adds latency/cost with **no measurable
  success lift** (matches the external review's core skepticism).
- The only discriminator here was **cost/latency**: gpt-5.4-mini passed
  everything ~20% faster on average (89.5s vs 111.0s) than gpt-5.5 — so for this
  task class the routing signal is "prefer the cheaper model," not "use a bigger
  model or add review."
- JINDO's loop value would only appear on tasks agents FAIL first-try. Those are
  hard to construct hermetically: they tend to be underspecified/novel, require
  repo-scale context, or need real iteration. Building such a failure-inducing,
  still-objectively-verifiable suite is the necessary next step to actually
  quantify the value of review + self-heal (and it costs tokens).

## What this means for routing
Prefer the cheapest tier that clears the objective verify; reserve review +
self-heal for work empirically shown to fail one-shot (not universal). Feed
per-(task_type, tier) verified-success + cost from this harness to the routing
owner rather than assuming a bigger model or more machinery helps.
