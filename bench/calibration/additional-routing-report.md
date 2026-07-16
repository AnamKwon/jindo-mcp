# Additional routing calibration — 2026-07-13

All candidate and reviewer calls used the Codex, Claude, or agy CLI directly.
The prompts prohibited MCP, plugins, skills, web search, and other agents. Code
answers were accepted only by hidden executable tests; HLE used exact answers;
reviewers used hidden defect labels and known-good cases.

## Repeated code-generation results

| capability cell | first route | repeated evidence | important counterexample |
|---|---|---|---|
| Python async single-flight cache | Gemini 3.5 Flash Low | 3/3, median 34.17s | Spark 0/1; Haiku 0/3 |
| Rust optimistic atomic store | Gemini 3.5 Flash Low | 3/3, median 32.26s | Spark/Luna/Terra/5.4-mini 0/1; Haiku 2/3 |
| JavaScript bounded keyed scheduler | GPT-5.6 Terra | 3/3, median 155.83s | Spark and Opus 2/3; every screened Gemini variant 0/1 |
| Java deterministic dependency planner | Spark | 3/3, median 26.28s | Flash Low 3/3 at 32.79s with clean reviews |
| SQL bitemporal ledger report | Gemini 3.5 Flash Low | 3/3, median 34.64s with clean reviews | Haiku 3/3 at 42.02s; Spark 2/3 |
| C++ RAII reentrant observer registry | Opus | Spark and Opus both 3/3; review selected Opus | calibrated review found two critical latent defects in Spark and none in Opus |

The result rejects both global rules “hard means largest” and “small means
cheapest.” Small models lead Python, Rust, Java, and SQL exact cells, JavaScript
needs Terra for repeatability, and C++ escalates to Opus after latent-defect
review. Spark is excellent on Java and passed C++ tests yet failed the Python/
Rust fixtures, so even a language-agnostic task type such as “concurrency” is
too broad.

Cross-model patch review changed the C++ order despite identical hidden-test
pass rates: Spark's reviewed patch retained unsubscribed callbacks/resources and
changed mutable callback-state semantics, while Opus had no critical finding.
Rust review also moved GPT-5.5 ahead of Sonnet because the reviewed Sonnet patch
used an unchecked version increment. Java and SQL reviews did not overturn their
objective winners.

The reviewed Flash Low reruns supported its Python/Rust first position: Python
received 8.25/8.50 with no critical findings versus Sonnet 7.50/7.75 and GPT-5.5
7.25/6.75; Rust received 7.50/8.25, with the only critical note being the same
constructor-precondition concern that also applied to the reference/API shape.

## Reviewer calibration

The expanded reviewer set has 20 cases and 17 expected defect labels covering
Python cancellation, generation fencing, failure retry, lock scope and TTL, and
Rust rollback, overflow, conflict order, deletion, snapshots, self-transfer,
empty commits and poisoned locks. Three repeats per model produced:

- Perfect recall, precision, verdict accuracy, and zero known-good false
  positives: Terra, Sol, Luna, GPT-5.5, Flash Medium, Flash High, Fable,
  Sonnet, and Gemini Pro High.
- Incomplete label recall: Pro Low 0.9804; Flash Low and Spark 0.9608; Haiku,
  5.4-mini, and Opus 0.9412.
- Evidence-ordered provider-diverse review pool: Terra, Flash Medium, Fable.

Reviewer author exclusion and two-provider review remain mandatory for high-risk,
oracle-free, and parallel-comparison routes. This review ranking is scoped to the
seeded Python/Rust contracts, not HLE or every programming language.

## Paired prompt-language transfer

Java and SQL used byte-identical repositories and hidden tests with only the
English instruction translated into Korean. Over three repeats:

- Java kept the same winner: Spark passed 3/3 in both languages and had the
  lowest median in both (English 26.28s, Korean 16.79s).
- SQL changed winner inside the paired cohort: English Haiku passed 3/3 while
  Spark passed 2/3; Korean Haiku fell to 1/3 while Spark passed 3/3 with a
  15.54s median. Flash Low later passed the English fixture 3/3 with a 34.64s
  median and clean reviews, so it leads the generic English route but has no
  paired Korean measurement.

The router therefore accepts `prompt_language` and has a Korean SQL override.
It does not infer that every Korean prompt should use Spark; Java demonstrates
stable transfer while SQL demonstrates a task-specific language interaction.

## Image-free HLE multiple choice

A 15-model, 5-item-per-subject screen selected candidates. The expanded run has
20 items per subject for Terra and Flash Medium:

| subject | Terra | Flash Medium | provisional order |
|---|---:|---:|---|
| mathematics | 7/20 | 6/20 | Terra, Flash Medium |
| biology | 3/20 | 8/20 | Flash Medium, Terra |
| physics | 5/20 | 6/20 | Flash Medium, Terra |
| chemistry | 2/20 | 5/20 | Flash Medium, Terra |
| computer science theory | 8/20 | 4/20 | Terra, Flash Medium |
| history/humanities | 8/20 | 8/20 | tie |

Terra returned all 12 batches with complete parseable JSON. Flash Medium had two
timeouts and two exit-zero answer-set omissions; those missing answers count as
incorrect. Claude Fable repeatedly omitted answers and Opus exceeded the 5-minute
batch limit, so neither becomes the default despite useful screen scores. Every
subject remains `parallel_compare` because the 20-item run has only one repeat.

## Operational failures and incomplete cells

- GPT-5.4-mini exceeded 27 minutes on an HLE batch and was stopped.
- The agy subscription quota was exhausted during expanded HLE and later
  Java/SQL calls. Exit-nonzero rows are retryable on `--resume` and are not model
  correctness evidence.
- Japanese/mixed-language transfer, image reasoning, frontend, security, shell,
  numerical/dataframe, and multi-file refactor remain in the calibration
  backlog.

## Artifacts

- `languages/results.json`: Python/Rust raw attempts and hidden-test output.
- `diverse_codegen/results.json`: JavaScript/Java/SQL attempts.
- `diverse_codegen_cpp/results.json`: C++ attempts.
- `korean_codegen/results.json`: Korean paired Java/SQL attempts.
- `reviewed_codegen/results.json`: rerun passing patches with calibrated Terra/Fable review tie-breaks.
- `reviewers_20case/results.json`: labeled reviewer predictions and scores.
- `hle/results.json`: 15-model subject screen.
- `hle_full_b10/results.json`: expanded HLE attempts, including transport state.

Policy promotion is encoded in
`internal/routing/config/capability_policy.json`. Repeated exact code cells use a
verified cascade; single-repeat HLE cells use provider-diverse parallel compare.
