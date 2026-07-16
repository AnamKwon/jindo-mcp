# Verification and test-generation routing calibration

Date: 2026-07-15 (Asia/Seoul)

This report measures test authorship as a capability separate from implementation
and code review. Candidate test suites were generated through the installed
Codex, Claude Code, and agy CLIs directly. MCP, plugins, skills, web search, and
other agents were disabled. The live surfaces were Codex CLI 0.144.4, Claude
Code 2.1.210, and agy 1.1.2. The screen included GPT-5.6 Luna/Terra/Sol,
GPT-5.3 Codex Spark, GPT-5.5, Claude Haiku/Sonnet/Opus, Gemini 3.5 Flash
Low/High, and Gemini 3.1 Pro High.

## Scoring contract

A candidate could edit only the test file. Its tests had to pass the unchanged
correct implementation and fail every seeded product mutant. The adaptive run
screened 11 models once, repeated only a fast passing candidate per provider,
and created a fresh independently reviewed generation only for 3/3 finalists.
A cascade promotion required 3/3, a fresh CLI success, a fresh objective pass,
parseable independent reviews, and zero critical findings.

| cell | mutation and runtime gates |
|---|---|
| Python contract/Decimal reconciliation | exact type checks, duplicate conflict, newest revision, delete semantics, deterministic ID order, Decimal precision; behavioral `unittest` only |
| Go lease concurrency/fencing v2 | token/owner/expiry boundary, exact release authority, snapshot alias and value fidelity, lock removal; `go test -race` |

Both fixtures passed RED/reference self-validation before candidate execution.

## Python contract mutation results

Only three models passed the 11-model screen: GPT-5.6 Luna, GPT-5.5, and
Gemini 3.5 Flash High. Their adaptive repeats were:

| finalist | pass rate | median seconds | fresh review |
|---|---:|---:|---|
| Gemini 3.5 Flash High | 2/3 | 46.16 | ineligible |
| GPT-5.5 | 2/3 | 94.51 | ineligible |
| GPT-5.6 Luna | 1/3 | 79.42 | ineligible |

Decision: `parallel_compare` in that order, always followed by the correct-plus-
mutants oracle. No model is a stable default. Luna's first pass shows that a
small tier can solve the hard cell, while its later failures show why a single
pass cannot promote it.

## Go fixture review, invalidation, and v2

The first Go campaign is audit evidence only. Reviewers found that its prompt
said fencing tokens increased forever while the reference implementation
discarded token history after Release. They also identified a missing snapshot
value-fidelity mutation. The prompt was aligned to the actual reference
contract, a `snapshot_value_lost` mutant was added, the reference suite was
strengthened, and all models were rerun from scratch in v2.

The corrected v2 screen produced passes across sizes: Luna, Terra, Spark, Opus,
Flash Low, Flash High, and Gemini 3.1 Pro High passed once; Sol, GPT-5.5,
Sonnet, and Haiku failed. Adaptive finalists were:

| finalist | repeat result | median seconds | fresh gate and review |
|---|---:|---:|---|
| Claude Opus 4.8 | 3/3 | 219.66 | patch passed the oracle, but the CLI timed out at 300s; 2 review-critical gaps |
| Gemini 3.5 Flash Low | 3/3 | 49.11 | fresh CLI and oracle passed; 6 review-critical gaps |
| GPT-5.3 Codex Spark | 2/3 | 72.78 | ineligible for fresh review |

Opus reviewers identified missing exact expiry-value and expiry-boundary
assertions. Flash Low reviewers additionally found weak one-way snapshot
independence, failed-Release state-preservation, and concurrent postcondition
coverage. Some reviewers disagreed on severity, so objective mutation gates
remain primary and the conservative summed critical count blocks promotion.

Decision: `parallel_compare` as Opus, Flash Low, Spark. Review quality puts Opus
first, but its fresh timeout prevents a cascade. Flash Low demonstrates that a
small model can be both much faster and repeatedly correct on an extreme cell,
yet review found enough unseeded gaps that speed cannot override quality.

## Host routing rules derived from this campaign

1. Classify test work by language and failure mode, not merely `test_generation`
   or nominal difficulty. Python contract precision and Go race/fencing produced
   different winners.
2. Keep test author, implementation author, and reviewer roles separate. A
   model's implementation or review ranking is not its test-author ranking.
3. Execute generated tests against an unchanged correct implementation and a
   hidden mutation set. Test count, coverage percentage, and a green reference
   run alone are insufficient.
4. For either measured cell, dispatch the provider-diverse candidates in
   `parallel_compare`; accept only a candidate whose suite passes the current
   project tests, kills task-relevant mutants, and receives independent review.
5. Re-run the exact acceptance tests on every routing attempt. Benchmark results
   select candidates; they never replace verification on the user's repository.
6. Version and invalidate a fixture when review exposes a contract/oracle gap.
   Never combine v1 and v2 pass rates.

## Limits and next cells

These are one-fixture cells, below the matrix target of 20 independent items.
Mutation sets are necessarily incomplete and could be overfit if disclosed.
Needed follow-ups include property-based test generation, integration/API
contract tests, database migration verification, security regression tests,
frontend interaction tests, performance benchmarks, and flaky-test diagnosis
across additional languages and prompt languages.

## Artifact map

- Fixtures: `bench/verification_tasks.py`
- Python raw screen/repeats:
  `verification_test_generation_2026_07/screen_and_repeats/results.json`
- Invalid Go v1 audit artifacts:
  `verification_test_generation_2026_07/reviewed/go_concurrency_fencing_test_generation/results.json`
- Corrected Go v2 screen/repeats:
  `verification_test_generation_go_v2_2026_07/screen_and_repeats/results.json`
- Corrected Go v2 fresh runs/reviews:
  `verification_test_generation_go_v2_2026_07/reviewed/go_concurrency_fencing_test_generation/results.json`
- Runtime policy: `internal/routing/config/capability_policy.json`
