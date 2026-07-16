# Benchmark evidence catalog

This directory keeps benchmark evidence separate from the executable fixtures
and the production routing policy. Raw results are appendable evidence;
capability policy is a reviewed interpretation of that evidence.

## Canonical synthesized reports

| report | scope | policy use |
|---|---|---|
| `additional-routing-report.md` | Python/Rust repeats, multilingual coding, HLE and reviewer calibration | exact cells only |
| `general-coding-routing-report.md` | Go API debugging, Python numerical code, Java multi-file refactor, JavaScript security | exact cells only |
| `language-expansion-2026-07-report.md` | C parser safety, Swift actor isolation, Bash atomic reconciliation | generated after the current campaign |
| `verification-test-generation-2026-07-report.md` | Python contract mutation tests and corrected-v2 Go concurrency/fencing tests | exact test-author cells; both remain parallel compare |

## Raw campaign directories

- `languages/`, `diverse_codegen/`, `diverse_codegen_cpp/`, `korean_codegen/`:
  earlier language and paired-prompt coding campaigns.
- `general_coding_screen/`, `general_coding_reviewed/`: software-purpose
  screening, repeats, and fresh reviews. The JavaScript v1 rows are retained
  for audit but are invalid for model promotion.
- `security_v2_screen/`, `security_v2_reviewed/`: corrected security fixture.
- `language_expansion_2026_07/`: adaptive screen, task-specific repeats,
  reviews, and manifest for C, Swift, and the invalidated Bash v1 fixture.
- `bash_portability_v2_2026_07/`: corrected macOS Bash 3.2 campaign; use this
  instead of Bash v1 for routing evidence.
- `verification_test_generation_2026_07/`: Python evidence plus invalidated Go
  v1 audit rows; do not use its Go rows for routing.
- `verification_test_generation_go_v2_2026_07/`: corrected Go test-generation
  screen, repeats, fresh runs, reviews, and adaptive manifest.
- `hle*`: subject-level noncoding results. Do not merge subjects into one score.
- `reviewers*`: labeled reviewer-defect calibration, separate from author
  performance.

Each `results.json` is the raw source. `summary.json` and `report.md` are derived
views. `inventory.json` records the CLI/model surface used for that campaign.
An adaptive campaign additionally writes `adaptive_manifest.json` and a
task-specific `reviewed/<task>/` directory.

## Amendment rules

1. Never overwrite a contradictory result to make a route look stable. Add a
   new campaign directory or repeat number.
2. Version a fixture when its contract or hidden oracle changes. Keep the old
   rows, mark them invalid for promotion, and rerun candidates from scratch.
3. Update `bench/benchmark_matrix.json` with measured/unmeasured status.
4. Update `internal/routing/config/capability_policy.json` only from repeated
   objective evidence plus fresh independent review. Keep
   `calibration_required=true` while a cell has fewer than 20 independent
   items.
5. Record model/CLI inventory, timeout/transport failures, prompt language, and
   review author exclusion. These are routing evidence, not incidental logs.
6. Regenerate or edit the synthesized report with the exact artifact paths and
   residual risks. Production routing must cite the precise capability cell,
   never a language-wide average.

## Reproducing a new adaptive campaign

```bash
python3 bench/run.py --self-test-fixtures --tasks TASK_ID
python3 bench/adaptive_run.py \
  --tasks TASK_ID \
  --models 'provider:model,...' \
  --repeats 3 --max-finalists 3 \
  --output-dir bench/calibration/CAMPAIGN_ID
```

Candidate execution in `run.py` is direct CLI and disables MCP, plugins,
skills, web search, and other agents. Review is a later, independent stage.
