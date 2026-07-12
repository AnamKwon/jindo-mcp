# jindo benchmark — pilot report

PILOT: small suite, claude+codex only (agy quota-exhausted). Directional, not conclusive.

## KPI by config

| config | verified pass | avg secs | avg calls |
|---|---|---|---|
| claude_solo | 4/4 (100%) | 33.6 | 1.0 |
| codex_solo | 4/4 (100%) | 16.2 | 1.0 |
| jindo_auto | 4/4 (100%) | 23.8 | 1.0 |
| jindo_review | 4/4 (100%) | 201.4 | 3.0 |

## verified success by task_type × solo agent

| task_type | claude | codex |
|---|---|---|
| implementation | 2/2 | 2/2 |
| debugging | 1/1 | 1/1 |
| test_generation | 1/1 | 1/1 |

## routing proposal (data for the routing owner; NOT auto-applied)

```json
{
  "implementation": {
    "best_agent": "codex",
    "verified": "2/2",
    "avg_secs": 17.9
  },
  "debugging": {
    "best_agent": "codex",
    "verified": "1/1",
    "avg_secs": 12.2
  },
  "test_generation": {
    "best_agent": "codex",
    "verified": "1/1",
    "avg_secs": 17.0
  }
}
```

Consume via internal/routing (owner) — this harness never edits the router.
