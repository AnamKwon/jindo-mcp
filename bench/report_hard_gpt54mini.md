# jindo benchmark — hard suite (codex-only)

CODEX-ONLY run (claude/agy tokens exhausted): measures whether JINDO's objective
verify gate + self-heal (auto-revision) lifts a single agent's verified success on
HARD tasks vs raw codex. Not a cross-agent routing comparison.

## KPI by config

| config | verified pass | avg secs | avg revisions |
|---|---|---|---|
| codex_raw | 5/5 (100%) | 89.5 | 0.0 |
| codex_verify | 5/5 (100%) | 85.9 | 0.0 |

## per-task verified pass

| task | codex_raw | codex_verify |
|---|---|---|
| lru_ttl | ✓ | ✓ |
| expr_eval | ✓ | ✓ |
| merge_intervals | ✓ | ✓ |
| fix_race | ✓ | ✓ |
| topo_sort | ✓ | ✓ |

## JINDO lift (verify+self-heal vs raw): +0 percentage points
Positive => the objective gate + auto-revision recovered failures a single raw dispatch missed.
