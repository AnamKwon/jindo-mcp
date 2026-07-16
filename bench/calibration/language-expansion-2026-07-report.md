# C, Swift, and Shell routing calibration

Date: 2026-07-15 (Asia/Seoul)

This report is the maintained interpretation of two raw direct-CLI campaigns:

- `language_expansion_2026_07/`: C, Swift, and the discarded Bash v1 fixture.
- `bash_portability_v2_2026_07/`: corrected Bash 3.2 fixture.

Candidate implementations used Codex, Claude Code, and agy directly with MCP,
plugins, skills, web search, and other agents disabled. Every fixture was first
validated as RED: the starter failed and the reference implementation passed.

## Tested capability cells

| language | task | hard invariants |
|---|---|---|
| C11 | incremental binary frame decoder | arbitrary chunking, embedded NUL, zero frames, allocation bound, sticky parse/callback failure, idempotent destruction, ASan/UBSan |
| Swift 6 | actor-isolated atomic ledger | strict concurrency, atomic batch rollback, idempotent transfer IDs, checked Int64 arithmetic, value snapshots, concurrent serializability |
| Bash 3.2 | atomic environment reconciliation | literal metacharacters/CR, exact grammar, duplicate detection, bytewise key ordering, symlink rejection, permissions, atomic failure, path quoting |

## C results

The nine-model screen had eight objective passes; Gemini 3.1 Pro High timed out
and failed. Adaptive repeats selected the fastest passing model from each
provider.

| model | repeated result | fresh reviewed generation |
|---|---:|---|
| GPT-5.3 Codex Spark | 3/3 | failed objective oracle; no review |
| Claude Haiku 4.5 | 3/3 | failed objective oracle; no review |
| Gemini 3.5 Flash Low | 2/3 | not eligible |

Decision: no default promotion. Route this exact cell as `parallel_compare` with
Spark, Haiku, and Flash Low and require the sanitizer-backed oracle. The fresh
generation failures show that 3/3 alone was not enough evidence.

## Swift results

| model | repeated result | fresh review |
|---|---:|---|
| GPT-5.6 Terra | 3/3 plus fresh pass | 0 critical findings; scores 7.75, 10 |
| Gemini 3.5 Flash High | 3/3 plus fresh pass | 0 critical findings; scores 7.25, 7.5, 10 |
| Claude Haiku 4.5 | 1/3 | not eligible |

Decision: `Terra → Flash High → Opus`. Terra and Flash High were both clean;
Terra is first because review quality precedes latency. Opus is an un-repeated
passing fallback, so the whole cell remains `calibration_required=true`.

## Bash fixture invalidation and v2

Bash v1 initially produced 3/3 results for Spark, Luna, and Flash Low. Fresh
review then found 9 critical findings in Spark and 5 in Flash Low, including:

- sorting complete `KEY=VALUE` lines instead of keys;
- misclassifying DESIRED when CURRENT is empty;
- silently stripping carriage returns from literal values;
- accepting grammar forms not specified by the contract;
- signal/temp cleanup defects;
- Bash 4-only syntax despite the intended macOS surface.

Those findings invalidated v1 as routing evidence. The fixture was renamed v2,
made explicitly compatible with macOS `/bin/bash` 3.2, and expanded with empty
input, prefix-key ordering, CR preservation, exact line grammar, and filenames
containing `=`. All candidates were then rerun from scratch.

| v2 model | repeated result | operational note |
|---|---:|---|
| GPT-5.3 Codex Spark | 3/3 | fresh generation failed before review |
| Claude Haiku 4.5 | 1/3 | unstable |
| Gemini 3.5 Flash Low | 1/3 | unstable |
| GPT-5.6 Luna | 1/1 screen | not repeated in provider shortlist |
| GPT-5.6 Terra | 1/1 screen | not repeated in provider shortlist |
| Claude Opus 4.8 | code passed but exceeded 300-second limit | operational failure |
| Gemini 3.1 Pro High | 0/1 | failed |

Decision: no default promotion. The exact shell cell is `parallel_compare` and
must run the Bash 3.2 portability and atomicity oracle. v1 artifacts remain for
audit only and must never be combined with v2 pass rates.

## Routing and maintenance implications

- Model size is not a stable proxy: Haiku screened strongly on C and Swift but
  became unstable on repeated/fresh generations; Spark led C and Bash repeats
  but failed both fresh generations.
- A fresh, independently reviewed generation is a promotion gate, not a report
  decoration. C and Bash deliberately remain comparison routes.
- Compiler/runtime versions are part of the cell. Homebrew Bash success cannot
  be silently transferred to macOS Bash 3.2.
- Reviewers may invalidate a fixture. When that happens, version the task,
  retain old evidence for audit, and rerun from scratch.
- These are still one-fixture cells, below the 20-item promotion target. Routes
  are local priors and keep `calibration_required=true`.

## Artifact map

- Fixtures: `bench/expansion_tasks.py`
- C/Swift raw screen and repeats:
  `language_expansion_2026_07/screen_and_repeats/results.json`
- C/Swift fresh runs and reviews:
  `language_expansion_2026_07/reviewed/<task>/results.json`
- Bash v2 raw screen and repeats:
  `bash_portability_v2_2026_07/screen_and_repeats/results.json`
- Bash v2 fresh run:
  `bash_portability_v2_2026_07/reviewed/bash_atomic_env_reconciler_v2/results.json`
- Runtime routing interpretation:
  `internal/routing/config/capability_policy.json`
