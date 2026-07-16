# General coding capability routing calibration

Date: 2026-07-15 (Asia/Seoul)

This campaign extends language-semantic fixtures with four software-work
purposes: API debugging, exact numerical code, security validation, and a
multi-file transactional refactor. Candidate implementations used the installed
Codex, Claude Code, and agy CLIs directly. Candidate prompts disabled MCP,
plugins, skills, web search, and other agents.

## Method

1. Validate every fixture as RED: the starter must fail the hidden oracle and
   the reference implementation must pass.
2. Screen representative large, medium, and small models once without review.
3. Retain task-specific, provider-diverse objective passes and repeat them to
   three attempts.
4. Generate a fresh patch for stable finalists and review it independently.
5. Rank by objective correctness, critical defects per valid review, review
   score, repeatability, then latency. Model size is not a ranking input.

`bench/adaptive_run.py` now automates this escalation without recreating a full
task-by-model matrix after screening.

## Results and routes

| capability cell | repeated candidates | fresh review | route |
|---|---|---|---|
| Go API retry debugging | Terra 3/3, Flash Low 3/3, Sonnet 3/3 | critical defects: 0, 1, 2 | Terra → Flash Low → Sonnet |
| Python exact apportionment | Flash Low 3/3, Luna 3/3, Sonnet 3/3 | all clean; Flash Low had the strongest median and lowest latency | Flash Low → Luna → Sonnet |
| Java multi-file optimistic transfer | Flash Low 3/3 screen repeats, Luna 2/3, Sonnet 1/3 | first fresh Flash run failed; next passed with three clean reviews | Flash Low → Luna → Sonnet, calibration required |
| JavaScript archive path security v2 | Terra 3/3, Opus 3/3, Flash Low 3/3 | critical defects: 0, 0, 2; Terra had the stronger clean review median | Terra → Opus → Flash Low |

The Java leader is 4/5 across all independent generations, so the host must not
treat it as a guaranteed single-model solution. It remains a first attempt
behind objective verification and review gates.

The JavaScript v1 fixture is not promotion evidence. Its reviewers exposed
ambiguous root, target, and hierarchy semantics plus missing oracle coverage.
The contract and hidden tests were corrected as v2 and every candidate was run
again from scratch. This is an intended failure-induced benchmark outcome: a
reviewer can invalidate the benchmark itself, not merely lower a model score.

## Host routing rules

- Match the exact `domain × language × prompt language × task type × risk ×
  oracle` cell. Never copy a winner to an unrelated task in the same language.
- Use the ordered route as a cascade only when the repository supplies the
  required objective oracle and independent review. Escalate on either failure.
- A small model may lead an extreme fixture. Flash Low leads exact Python
  numerical work and the unstable Java multi-file cell because observed
  correctness, review, and repeatability outrank parameter tier.
- A passing hidden test is not the final tie-break. Go and JavaScript both had
  objective ties that review changed materially.
- Treat timeout or non-zero CLI completion as an operational failure even when
  the partially produced code passes hidden tests. This excluded Sonnet from
  the stable JavaScript v2 cohort after a 300-second timeout.
- Do not rerun this benchmark on every routed request. Recalibrate only for a
  new capability cell, a material model/CLI version change, or unexplained
  production failures. Ordinary requests execute repository verification and
  review against the stored ladder.

## Remaining coverage risk

Each new cell still has only one hard fixture, not the matrix promotion target
of 20 items. Shell, Swift, frontend state, build tooling, performance,
distributed systems, API upgrades, data pipelines, and test generation remain
unmeasured or under-measured. The present routes are local direct-CLI priors and
must keep `calibration_required=true`.

Artifacts:

- `general_coding_screen/`: initial screens and repeated candidates, including
  the discarded JavaScript v1 evidence.
- `general_coding_reviewed/`: fresh reviewed Go, Python, and Java patches.
- `security_v2_screen/`: corrected security screens and three-repeat cohort.
- `security_v2_reviewed/`: corrected security fresh patches and reviews.
