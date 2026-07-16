# Harder Sol-to-mini delegation calibration

## Verdict

The harder rerun supports a narrow conclusion: a strong planner can rescue a
small implementer on some invariant-dense tasks, but it is not a general
substitute for the strong model and it is worse for latency when run
sequentially.

The `atomic_config_migration_v2` fixture reproduced the useful case:
`gpt-5.4-mini` direct failed every run, while `gpt-5.6-sol -> gpt-5.4-mini`
matched Sol direct at 2/3 objective passes. The `javascript_secure_archive_plan_v3`
fixture did not: every condition failed 0/3.

## Fixture theory

`atomic_config_migration_v2` is a harder successor to the earlier migration
fixture. The domain invariant is not just field renaming; it is a strict JSON
schema transition plus crash-safe, symlink-safe, replacement-aware file update.
The hidden oracle includes duplicate-key rejection, exact unknown-byte
preservation, no-op V2 compatibility, temp cleanup, fsync/rename behavior,
permission preservation, and deterministic replacement-hook races.

`javascript_secure_archive_plan_v3` is a harder successor to the earlier archive
security fixture. The invariant is a portable lexical namespace independent of
the host filesystem. The hidden oracle adds Unicode NFC/case collisions, Windows
reserved names, trailing dot/space handling, hardlink and symlink ordering,
prototype-safe input handling, exact output shape, and purity.

## Commands

```sh
python3 bench/delegation_run.py \
  --tasks atomic_config_migration_v2 \
  --conditions large_direct,small_direct,large_plan_small \
  --large-model codex:gpt-5.6-sol \
  --small-model codex:gpt-5.4-mini \
  --reviewers codex:gpt-5.6-terra \
  --repeats 3 \
  --output-dir bench/calibration/delegation_harder_v2/migration

python3 bench/delegation_run.py \
  --tasks javascript_secure_archive_plan_v3 \
  --conditions large_direct,small_direct,large_plan_small \
  --large-model codex:gpt-5.6-sol \
  --small-model codex:gpt-5.4-mini \
  --reviewers codex:gpt-5.6-terra \
  --repeats 3 \
  --output-dir bench/calibration/delegation_harder_v2/archive
```

## Results

| task | condition | pass | median sec | median total tokens/run | cost bound, 3 runs |
|---|---:|---:|---:|---:|---:|
| atomic_config_migration_v2 | Sol direct | 2/3 | 176.36 | 42,415 | $0.5871-$3.5228 |
| atomic_config_migration_v2 | mini direct | 0/3 | 480.80 | 49,555 | $0.1160-$0.6959 |
| atomic_config_migration_v2 | Sol -> mini | 2/3 | 710.26 | 123,823 | $0.5506-$3.3034 |
| javascript_secure_archive_plan_v3 | Sol direct | 0/3 | 152.53 | 28,032 | $0.4102-$2.4611 |
| javascript_secure_archive_plan_v3 | mini direct | 0/3 | 454.08 | 42,650 | $0.0930-$0.5577 |
| javascript_secure_archive_plan_v3 | Sol -> mini | 0/3 | 916.36 | 99,332 | $0.4407-$2.6443 |

Cost is a bound, not an exact invoice. The harness did not produce `cost_usd`
because the CLI output exposed total tokens but not reliable input/output splits.
The table above is a separate derived estimate from planner/implementation stage
`total_tokens`, using all-input and all-output extremes per model. Review token
usage was not exposed in the stored review objects and is not included.

## Interpretation

`Sol -> mini` is useful when the bottleneck is omitted invariants and the small
model can still execute the implementation once those invariants are explicit.
That appears true for the harder migration task.

`Sol -> mini` is not useful when the bottleneck is long-horizon implementation
control under many interacting lexical/security constraints. That appears true
for the harder archive task. Sol itself also failed this fixture, so this is a
fixture-level difficulty signal rather than only a delegation failure.

Sequential delegation was consistently slow. In the migration run, Sol direct
and Sol-to-mini had the same pass rate, but delegation had about 4.0x median
latency. In the archive run, delegation failed and was about 6.0x slower than
Sol direct.

## Faster operating pattern

The fastest practical shape is not "always ask Sol to write a full plan for
mini." It is a cascade:

1. Run the cheapest candidate that is calibrated for the task shape.
2. Verify with an objective oracle.
3. Escalate only failures to Sol planning or Sol direct.
4. Review only objective passes or stable finalists.

For lower tail latency, run the cheap implementation and a bounded strong-model
plan speculatively in parallel. If the cheap implementation passes, discard the
plan. If it fails, the plan is already available for the retry. This spends more
planning tokens on some successful cheap runs, but avoids the full sequential
sum on hard failures.

The planner output should be compressed to invariants, touched files, and
acceptance tests. The long plans in this run often exceeded the useful signal
and made the mini implementation stage much slower.

Prompt caching can help if this is moved from CLI orchestration to direct API
calls: keep static repo instructions, fixture contract, and reviewer rubric as
the exact prefix, then put only the variable task/run data at the end. OpenAI's
prompt caching guide states that exact prefix matches are required, caching is
enabled for prompts of at least 1024 tokens, and cache usage is reported through
cached/write token fields.

Background mode is useful for robustness of long jobs and polling, but it does
not make the model compute itself faster. It should be treated as an operational
reliability feature, not a latency optimizer.

## Relation to prior run

The previous extreme matrix found mixed behavior:

- JavaScript archive security v2: all three conditions passed 3/3, with review
  quality separating candidates.
- Java multi-file transaction: Sol and mini direct failed 0/3, while
  Sol-to-mini passed 2/3.
- C incremental parser: mini direct and Sol-to-mini both passed 2/3, so planning
  added no objective benefit.

The harder rerun preserves that shape: delegation is a task-specific tool, not a
global model policy.

## Residual risk

Each harder cell has only three repeats. This is enough to reject "always better"
and "always cheaper/faster" claims, but not enough to promote a production
default. Promotion should require more fixtures per task family, direct API token
usage with input/output/cache splits, and a shorter per-stage timeout so tail
latency is measured instead of tolerated.
