# Extreme planner and implementer matrix

## Verdict

`GPT-5.6 Sol plan -> GPT-5.4 mini implementation` is useful on some hard task
shapes, but it is not a universal replacement for Sol or a universal upgrade
over mini. The planner can supply missing invariants; it cannot reliably supply
the implementer's long-horizon execution ability. Exact task shape and repeated
verification dominate model-size intuition.

## Screen

Five hard fixtures were screened once without review. The matrix included Sol,
Terra, Luna, and GPT-5.4 mini direct runs; Sol plans handed to Terra, Luna, and
mini; and the reverse mini plan handed to Sol.

| task | Sol direct | mini direct | Sol -> mini | Luna direct | Sol -> Luna | Terra direct | Sol -> Terra | mini -> Sol |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| atomic migration | pass | pass | pass | pass | pass | pass | pass | pass |
| Python async single-flight | pass | pass | pass | pass | pass | pass | pass | pass |
| JavaScript archive security | pass | fail | pass | pass | pass | pass | pass | pass |
| Java multi-file transaction | pass | fail | fail | fail | fail | fail | fail | fail |
| C incremental parser | fail | pass | pass | fail | fail | fail | fail | fail |

The one-shot screen found three deliberately contradictory cells, so those were
rerun from fresh repositories three times with independent review on objective
passes.

## Repeated Sol and mini result

| task | Sol direct | mini direct | Sol -> mini | interpretation |
|---|---:|---:|---:|---|
| JavaScript archive security | 3/3 | 3/3 | 3/3 | all pass objectively; review separates edge quality |
| Java multi-file transaction | 0/3 | 0/3 | 2/3 | Sol planning materially rescues mini, but is unstable |
| C incremental parser | 0/3 | 2/3 | 2/3 | Sol planning adds no objective success over mini |

The screen and repeat disagreement is itself evidence: single generations are
not reliable enough to promote a combination.

## Review evidence

- Security Sol direct: 3 clean reviews, mean score 8.83.
- Security mini direct: two raw Fable reviews reported one critical defect each;
  the third parsed clean at 8.25. The two JSON envelopes were not parsed by the
  harness because braces occurred inside rationale text, so they must not be
  treated as completed clean reviews.
- Security Sol -> mini: the scheduled Fable reviews were unavailable after the
  organization monthly spend limit was reached. A fresh replacement generation
  reviewed by GPT-5.6 Terra scored 9.25 with zero critical findings.
- Java Sol -> mini: the two objective passes received clean Fable reviews with
  mean score 7.75.
- C mini direct: two objective passes received clean Fable reviews with mean
  score 8.13.
- C Sol -> mini: two objective passes received clean Fable reviews with mean
  score 9.25.

## Median runtime, tokens, and API-equivalent bounds

The CLI reports total tokens, not the input/output split. Bounds therefore use
the all-input and all-output extremes at official rates: Sol $5/$30 per million
tokens and GPT-5.4 mini $0.75/$4.50.

| task and condition | median seconds | median total tokens | cost bound |
|---|---:|---:|---:|
| Security Sol direct | 106.35 | 24,639 | $0.123-$0.739 |
| Security mini direct | 273.23 | 35,411 | $0.027-$0.159 |
| Security Sol -> mini | 284.04 | 50,314 | $0.104-$0.627 |
| Java Sol direct | 127.11 | 36,759 | $0.184-$1.103 |
| Java mini direct | 273.45 | 41,115 | $0.031-$0.185 |
| Java Sol -> mini | 405.31 | 58,753 | $0.104-$0.622 |
| C Sol direct | 108.94 | 23,568 | $0.118-$0.707 |
| C mini direct | 300.65 | 52,507 | $0.039-$0.236 |
| C Sol -> mini | 422.67 | 69,457 | $0.117-$0.700 |

On this local CLI, mini was cheaper but not faster. Sol -> mini was always the
slowest because planning and implementation were sequential. It can still cost
less than Sol direct because mini tokens are priced at 15% of Sol tokens, but
the saving disappears when the delegated run fails and must be retried.

## Decision rule

- Use Sol -> mini for well-specified constraint/security work when a strong
  hidden oracle or independent review is available.
- Do not use it as a no-review shortcut for stateful multi-file transactions;
  2/3 is an improvement over 0/3, not production reliability.
- On capability cells where mini already matches or beats Sol, such as this C
  screen, planning is unnecessary overhead unless review proves a quality gain.
- Route and calibrate by exact task shape. Neither `larger model` nor `larger
  planner` is a sufficient selection rule.

This is repeated evidence for three hard cells plus one-shot evidence for five
cells. It is not a universal claim across repositories or model versions.
