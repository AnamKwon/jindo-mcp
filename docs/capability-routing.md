# Capability-aware routing and calibration coverage

The host routes on a capability cell, not on prompt difficulty alone:

`domain × programming language × prompt language × task type × risk × oracle quality × concrete task signals`

The MCP-native decision-support surface is `route_capability`; it returns exact
and analogous evidence, the full eligible catalog, concrete task signals, and
uncertainty without choosing a model. `dispatch` and
`dispatch_multi` require the same `capability` object in their MCP schemas and
enforce that the host explicitly records its model choice. The server retains a
capability-free path only for backward-compatible non-MCP callers.
The versioned source is `internal/routing/config/capability_policy.json`. The
offline `bench/capability_router.py` reads that same source.

## What is currently known

The July 2026 local direct-CLI campaign now has three evidence levels:

- Go has one run per model on three hard editing fixtures. Those observations
  remain provisional and require repository verification.
- Python async single-flight caching and Rust optimistic atomic storage have
  three repeats for the leading cross-provider candidates. Gemini 3.5 Flash Low
  passed both fixtures 3/3 and was the fastest repeated candidate; Sonnet and
  GPT-5.5 also passed 3/3. Haiku failed Python 0/3 and was unstable on Rust at
  2/3. This is direct evidence that a small model can be the best first attempt
  on a difficult but well-specified task with a strong oracle.
- Four additional code-generation cells now have repeated leaders: Terra is the
  only 3/3 repeated winner on the JavaScript keyed scheduler; Spark is 3/3 and
  fastest on Java dependency waves and passed C++ RAII/reentrant observers 3/3;
  Flash Low and Haiku are both 3/3 on SQL, with Flash Low faster and clean in
  calibrated review. Calibrated patch review moved C++ from Spark to Opus after
  finding two latent critical defects in Spark's passing patch. Spark was only
  2/3 on JavaScript and SQL despite leading Java, demonstrating why a
  language-wide global winner is still too coarse.
- A software-purpose campaign adds hard API debugging, exact numerical code,
  security path validation, and a four-file transactional refactor. Go retry
  debugging observed Terra, Flash Low, and Sonnet at 3/3, while
  review found 0, 1, and 2 critical defects respectively. Python exact
  apportionment observed Flash Low, Luna, and Sonnet all at 3/3 and a
  fresh clean review, with Flash Low fastest and highest reviewed. The Java
  multi-file transaction evidence favors Flash Low, but remains explicitly
  unstable: it passed 4/5 independent generations, versus Luna 2/3 and Sonnet
  1/3. These cells show why task purpose and review quality must refine the
  language label.
- The security fixture was versioned after reviewers found ambiguities in its
  first contract; v1 is excluded from model ranking. On the corrected v2,
  Terra, Opus, and Flash Low all passed 3/3 plus a fresh run. Terra and Opus had
  zero reviewed critical defects, while Terra's reviewed median was stronger;
  Flash Low's passing patch still had two latent validation defects. The route
  therefore favors Terra and Opus on reviewed quality rather than latency alone.
- New local-toolchain cells cover C11 incremental parsing, Swift 6 actor
  isolation, and macOS Bash 3.2 reconciliation. Swift has clean repeated review
  evidence for Terra and Flash High. C and Bash remain uncertain because their
  3/3 leaders failed a fresh generation. Bash v1 was invalidated after review
  found contract/oracle gaps and was rerun as a stricter v2; only v2 is routing
  evidence. Detailed raw paths and amendment rules live under
  `bench/calibration/`.
- Reviewer calibration uses 20 labeled good/defective Python and Rust cases over
  three repeats. Terra, Sol, Luna, GPT-5.5, Flash Medium/High, Fable, Sonnet and
  Gemini Pro High achieved 100% labeled-defect recall and precision with zero
  known-good false positives. The provider-diverse production order is Terra,
  Flash Medium, then Fable; author exclusion still applies.

Image-free HLE multiple-choice screening is stored separately by subject. Terra
and Flash Medium now have 20 items in six subjects: Terra leads mathematics and
CS theory, Flash Medium leads biology, chemistry, and physics, and history ties.
Because this is one repeat and Flash also had transport/format failures, all
subject routes remain `parallel_compare` and require calibration.

Paired English/Korean code prompts are measured for Java dependency planning and
SQL bitemporal reporting. Java retained Spark as the 3/3 winner. SQL did not:
in the paired cohort Haiku moved from English 3/3 to Korean 1/3 while Spark moved
from English 2/3 to Korean 3/3. A later Flash Low English repetition became the
generic SQL first choice, but it was not part of the Korean paired cohort.
`prompt_language=korean` therefore selects a specific SQL override; Japanese,
mixed-language, and unmeasured language/task pairs remain unmeasured.

## Host flow

Call `route_capability` with an explicit cell:

```json
{
  "capability": {
    "domain": "coding",
    "language": "go",
    "prompt_language": "english",
    "task_type": "concurrency_fencing",
    "risk": "high",
    "oracle": "deterministic",
    "signals": {
      "ambiguity": "high",
      "change_scope": "multi_file",
      "context_size": "medium",
      "reversibility": "limited",
      "required_strengths": ["memory-model reasoning", "repository navigation"],
      "failure_modes": ["stale owner mutation", "shallow race-test confidence"]
    }
  }
}
```

Read `exact_match`, `evidence_gap`, `candidate_evidence`, `eligible_models`,
`analogous_evidence`, `reason`, `required_oracle`, and `host_selection` against
the concrete request. Exact candidates carry direct evidence for that cell. On
an unmeasured cell, empty `candidates` means there is no direct benchmark prior;
the full choice surface is `eligible_models` and `mode=host_decides`. Analogous
cells explain possible transfer hypotheses but explicitly forbid copying their
winner. Decide whether one model is enough, a
small task-local probe is needed, or independent comparison is safer. Then pass
the same capability plus explicit `model` and
`selection_reason` to `dispatch`, or explicit `models` and `selection_reason`
to `dispatch_multi`. There is no automatic first-candidate pin or automatic
fan-out. Calibrated coding dispatches are also rejected without objective
`verify` commands and `review:true`; host discretion changes selection, not the
acceptance gates. A model outside the exact benchmark candidates is permitted
when the host explains the task-specific reason. The result records separately
whether the choice was inside the direct benchmark prior and inside the eligible
catalog.

## Offline examples

Measured Go cell with a deterministic oracle:

```bash
python3 bench/capability_router.py \
  --domain coding --language go --task-type concurrency_fencing \
  --risk high --oracle deterministic
```

Unmeasured Rust cell:

```bash
python3 bench/capability_router.py \
  --domain coding --language rust --task-type concurrency_fencing \
  --risk high --oracle deterministic
```

Unmeasured HLE-like biology cell:

```bash
python3 bench/capability_router.py \
  --domain biology --task-type short_answer \
  --risk high --oracle exact_answer
```

`cascade` is direct-cell evidence that a bounded single-model attempt may be
reasonable; `parallel_compare` is direct-cell evidence that independent answers
were safer in calibration. `host_decides` means no exact cell exists. None is an
execution command. The host chooses after considering task
ambiguity, risk, oracle strength, candidate tradeoffs, and operational budget,
but must record why and still pass the objective/review gates.

For `host_decides`, follow the returned `unmeasured_workflow`: interpret the
real task, form plausible hypotheses from every eligible model (including small
models), run the smallest representative probe only if it can change the
choice, apply the real oracle and independent review, and record whether the
result supported the routing hypothesis. This is host reasoning over evidence,
not a hidden weighted score or a difficulty-to-size mapping.

`eligible_models` is the evidence policy catalog filtered by available agent
CLIs, not a claim that no newer model exists. Use `models_refresh` when the
installed inventory may have changed. Newly discovered models are returned as
`unmeasured_new_model` assessment requests with no proposed tier or effort; the
host can include them in a reasoned task-local probe before adding benchmark
evidence to the policy.

## Coverage that should be added

Programming-language results must be stratified rather than copied from Go:

- Python: async cancellation, decorators/descriptors, numerical/dataframe edge
  cases, packaging and dependency resolution.
- TypeScript/JavaScript: structural typing, conditional generics, event-loop
  ordering, React state and build-tool boundaries.
- Rust: ownership/lifetimes, unsafe invariants, async Send/Sync, trait coherence.
- C/C++: memory lifetime, undefined behavior, templates, ABI and concurrency.
- Java/Kotlin: generics variance, JVM concurrency, transactions, build systems.
- SQL/shell: isolation and query plans; quoting, process status and portability.

Noncoding coverage should retain both subject and reasoning form:

- mathematics: algebra, number theory, geometry, combinatorics, probability,
  exact calculation and proof;
- physics and chemistry: derivation, units, spatial/molecular reasoning and
  numerical precision;
- biology and medicine: molecular/cellular, genetics, evolution/ecology,
  systems and evidence interpretation;
- theoretical computer science, law, history/humanities, social science and
  general knowledge;
- charts, tables, diagrams and image-grounded questions;
- Korean, English, Japanese and mixed-language prompts.

The policy file also lists API upgrades, performance, distributed systems,
frontend, and data pipelines because the current fixtures do not cover those
failure modes. Test generation now has two hard cells, but neither earned a
single-model default: Python contract/Decimal mutation testing had no 3/3
finalist, while corrected-v2 Go concurrency/fencing testing left review gaps or
a timed-out fresh run. The host therefore uses provider-diverse
`parallel_compare`, runs generated tests against both the correct implementation
and seeded mutants, and keeps test author, implementation author, and reviewer
as distinct roles. This is useful routing evidence but still far below broad
capability coverage.

The complete campaign contract lives in `bench/benchmark_matrix.json`. Inspect
or validate it with `python3 bench/matrix.py` and
`python3 bench/matrix.py --validate`.

For new cells, `bench/adaptive_run.py` controls cost: screen all requested
models once without review, retain provider-diverse objective passes, repeat
only those candidates, and review only stable finalists. It writes a manifest
that separates screening, repeatability, and fresh-review evidence.

## Reviewer calibration

Reviewer routing needs a separate labeled benchmark. For every subject or code
cell, retain correct answers plus seeded mutations such as subtle contract
violations, plausible false-positive traps, concurrency/security defects,
numeric edge cases, citation errors, and correct-but-unusual solutions. Score:

- critical-defect recall and precision;
- false-positive rate on known-good answers;
- severity and location accuracy;
- consistency across repeats;
- ability to prefer the objectively better answer when multiple answers pass a
  shallow check.

The current reviewer pool is evidence-ordered and provider-diverse: Terra,
Gemini 3.5 Flash Medium, then Fable. Jindo still excludes the answer author and
requires two independent reviewers for high-risk, oracle-free, or parallel
routes. The 20-case benchmark covers the seeded Python/Rust contracts only, so
objective tests, exact answers, and reproducible calculations remain the primary
gate and other review domains still require calibration.
