# Model Policy for the jindo Router

Researched mid-2026. Evidence covers coding ability (SWE-bench Verified), cost,
speed/latency, and community per-difficulty usage reports. Concrete model IDs and
source URLs are cited. The machine-readable counterpart the router consumes is
[`jindo/config/models.json`](../jindo/config/models.json); this document is the
human-readable rationale and must be kept in sync with it.

## Per-model strengths / weaknesses

| Provider | Model (ID) | SWE-bench Verified | Price (in/out $/M) | Speed | Strengths | Weaknesses |
|---|---|---|---|---|---|---|
| Anthropic | Haiku 4.5 (`claude-haiku-4-5`) | ~73.3% | 1.00 / 5.00 | Fastest | Cheap/fast triage, classification, formatting, IDE-style edits; "indistinguishable from Sonnet" on shallow tasks | Falls behind on multi-file reasoning and long agent runs |
| Anthropic | Sonnet 5 (`claude-sonnet-5`) | ~82.1% (reported) | 3.00 / 15.00 | Fast | Near-Opus-tier coding/agentic quality at Sonnet cost; first Sonnet-tier model past 80% Verified; adaptive thinking + full effort range by default | Not the top scorer on the hardest agentic problems; new tokenizer uses ~30% more tokens than Sonnet 4.6 |
| Anthropic | Opus 4.8 (`claude-opus-4-8`) | ~88.6% (official) | 5.00 / 25.00 | Slower | Hardest reasoning, long-horizon multi-step planning, multi-file refactors, novel problems | Most expensive; overkill for routine work |
| Anthropic | Fable 5 (`claude-fable-5`) | frontier | premium | Slower | Current Claude capability ceiling for long, difficult software work | Conservative cyber safeguards can redirect some requests |
| OpenAI | GPT-5.6 Luna (`gpt-5.6-luna`) | current small tier | 1.00 / 6.00 | Fast | Smallest GPT-5.6 tier; passed all three local adversarial fixtures with fewer review findings than GPT-5.4 mini | Still needs verification on high-risk migration and concurrency work |
| OpenAI | GPT-5.6 Terra (`gpt-5.6-terra`) | current balanced tier | 2.50 / 15.00 | Moderate | Better local review profile than prior small/mid Codex candidates | Spark remained much faster in this local coding suite |
| OpenAI | GPT-5.6 Sol (`gpt-5.6-sol`) | current flagship | 5.00 / 30.00 | Moderate | Latest OpenAI frontier coding model | GPT-5.5 had the cleanest local review result, so remains the conservative hard default pending repeats |
| OpenAI | GPT-5.4 mini (`gpt-5.4-mini`) | (light-task tier) | ~one-third of GPT-5.4 quota | Fast | Faster, lower-cost option for lighter coding tasks and subagents; Codex delegates simple work here | Less capable; not for hard agentic coding |
| OpenAI | GPT-5.3-Codex Spark (`gpt-5.3-codex-spark`) | ~85.0% | 1.75 / 14.00 | Fast | "Most capable agentic coding model to date" — frontier coding + reasoning fused | Codex-specialized; non-coding knowledge work better on GPT-5.5; base `gpt-5.3-codex` id 400s on ChatGPT-plan accounts, use the `-spark` id |
| OpenAI | GPT-5.5 (`gpt-5.5`) | ~88.7% (reported) | (premium tier) | Slower | Strongest all-round for complex coding, computer use, research; OpenAI's default "start here" for most tasks | Verified scores de-emphasized by OpenAI (contamination concerns); pricier |
| Google | Gemini 3.5 Flash (`gemini-3.5-flash`) | high (beats 3.1 Pro on coding) | 1.50 / 9.00 | ~4x faster than peers | Near-Pro reasoning at Flash latency/cost; great for interactive dev and long agent loops | Slightly behind top Pro/frontier models on the hardest issues |
| Google | Gemini 3.1 Pro (`gemini-3.1-pro`) | ~78% (2.5 Pro baseline; 3.1 Pro higher) | 2.00 / 12.00 (≤200k) | Moderate | Strong coding + 1M context; solid mid/high tier; competitive cost | Not the single highest SWE-bench score in the field |

Notes: SWE-bench Verified figures are mid-2026 community/vendor-reported and move
between releases; treat them as ordering signals, not exact contract values. OpenAI
has de-emphasized Verified (contamination concerns) in favor of SWE-bench Pro, so the
Codex tiering leans on OpenAI's own task-fit guidance as much as the headline number.

## Routing mechanism: deterministic scoring + hybrid tie-break

The router selects agent and model in two stages:

### Stage 1: Deterministic multi-signal scoring

The task text is scored against four signals (security, constraints, scope, ambiguity) defined in [`jindo/config/routing_policy.json`](../jindo/config/routing_policy.json). Each signal contributes a weighted score based on the count of distinct patterns in its list that appear as substrings in the lowercased task text. The total score determines a tier:

- **tier = hard** if total ≥ 6.0
- **tier = standard** if total ≥ 1.0
- **tier = trivial** otherwise

This stage is pure, deterministic, and makes **no LLM call**. See [`docs/routing_policy.md`](routing_policy.md) for the complete formula, signal weights (security 3.0, constraints 1.5, scope 1.2, ambiguity 0.8), and worked examples.

### Stage 2: Ambiguous-band hybrid resolution (optional LLM thinker)

A total within `width = 1.0` of either threshold (i.e., in `[0.0, 2.0]` or `[5.0, 7.0]`) is considered *ambiguous*: small wording changes could flip the tier. For such ambiguous totals, a mockable LLM seam (`ModelRouter._llm_assess(task, base)`) may nudge the tier; outside the band, the deterministic result is trusted verbatim.

The seam is **deterministic by default**: it returns `None`, leaving the tier untouched. Production may override `_llm_assess` to consult a small model (e.g., for a tighter re-assessment), but the common path remains cheap and reproducible. This mirrors the cascade-scheduling insight: escalation *decisions* are uncertain near the threshold, not across the full range.

#### Go port: real assessor behind a double gate

The Go router (`internal/routing`, wired from `cmd/jindo-mcp/main.go`) fills the
same seam (`routing.Assessor`) with a real implementation backed by the `agy`
adapter (`internal/assess`): it asks Gemini Flash to classify the task as
exactly one of `trivial` / `standard` / `hard`, and treats any failure
(timeout, adapter error, unparseable or out-of-set answer) as "no assessment",
deterministically keeping `ScoreTask`'s tier.

The assessor is consulted only when **both** gates are open — `enabled` here
never activates assessment by itself:

1. `routing_policy.json`'s `llm_assess.enabled` is `true`, and
2. the `JINDO_LLM_ASSESS` environment variable is set (non-empty),

and only when the task's total falls inside one of `llm_assess.bands`. Both
are **off by default**, so a shared/checked-in policy file can never silently
turn assessment on — an operator must also opt in via the environment.

### Stage 3: Agent + model lookup

Once the tier is final, `ModelRouter.route_for_difficulty(difficulty, agent=None)` looks up the recommended agent and model from [`jindo/config/models.json`](../jindo/config/models.json), using the per-difficulty defaults if no agent override is supplied.

### Output enrichment

`ModelRouter.select(task, agent=None)` returns:
- `agent` — the selected agent name (e.g., "claude")
- `model` — the selected model ID (e.g., "claude-sonnet-5")
- `difficulty` — the final tier ("trivial" | "standard" | "hard")
- `scores` — dict of per-signal weighted contributions (e.g., `{"security": 3.0, "constraints": 1.5, "scope": 0.0, "ambiguity": 0.0}`)
- `reason` — human-readable deterministic explanation (e.g., "security (auth, encrypt) + constraints (validat) -> hard")

All values are JSON-serializable. The `scores` and `reason` fields enable audit trails and human verification of routing decisions.

## Recommended model per agent per tier

| Agent | trivial | standard | hard | Rationale |
|---|---|---|---|---|
| `claude` | `claude-haiku-4-5` | `claude-sonnet-5` | `claude-fable-5` | Local direct-CLI calibration: Haiku passed lease/ledger but failed the migration build; Sonnet stays balanced; Fable passed all three and replaces Opus at the ceiling. |
| `codex` | `gpt-5.6-luna` | `gpt-5.3-codex-spark` | `gpt-5.5` | Local direct-CLI calibration: Luna clearly beat GPT-5.4 mini as the small tier; Spark kept the best mid-tier latency; GPT-5.5 had the cleanest hard-task review profile. |
| `agy` | `gemini-3.5-flash` | `gemini-3.1-pro` | `gemini-3.1-pro` | Flash gives near-Pro quality at ~4x speed for cheap/fast work; Pro carries standard and hard. Google ships no separate "Opus-class" coding tier, so Pro is reused at hard. |

## Orchestration policy

Cheap/fast at the bottom, mid in the middle, most capable at the top — the
2026 routed-stack consensus ("Haiku triages, Sonnet builds, Opus reviews"):

- **trivial** — classification, formatting, commit messages, routing, single-line
  edits. Speed and price dominate; capability gaps are negligible. Use the fastest,
  cheapest tier of each agent.
- **standard** — feature implementation, bug fixes, ordinary refactors, code review.
  The balanced mid tier is the right cost/quality point.
- **hard** — treat the tier as a risk flag, not an unconditional large-model
  command. A well-specified concurrency task with a strong race/invariant harness
  may start on a calibrated small model; migration, durability, security, missing
  test oracles, and irreversible changes start on the hard model. Escalate on
  objective verification or review-gate failure.

The local calibration artifacts are in `bench/calibration/`. Exact Python, Rust,
JavaScript, Java, SQL, and C++ cells now have selected three-repeat results, while
Go remains single-repeat and HLE subject results remain 20-item single-repeat.
These justify cell-specific cascades, not claims that one model is universally
best. Always use the verify gate for stateful, concurrent, migration, security,
or irreversible work.

### default_agent_by_difficulty rationale

These pick which agent *leads* a tier when the router has no stronger signal:

- **trivial → `agy`** (Gemini 3.5 Flash): runs ~4x faster than comparable frontier
  models at lower cost — speed/throughput is the feature for high-volume trivial work.
- **standard → `claude`** (Sonnet 5): the everyday-coding default — near-Opus-tier
  quality at Sonnet cost, and the safest balanced lead for the bulk of work.
- **hard → `codex`** (GPT-5.5 / GPT-5.3-Codex): the strongest agentic-coding stack in
  this set on SWE-bench Verified (Codex ≈85%), so it leads the highest-stakes tier.

## Sources

- https://www.morphllm.com/claude-benchmarks
- https://www.anthropic.com/news/claude-sonnet-4-5
- https://www.datacamp.com/blog/claude-opus-4-5
- https://platform.claude.com/docs/en/about-claude/pricing
- https://www.metacto.com/blogs/anthropic-api-pricing-a-full-breakdown-of-costs-and-integration
- https://benchlm.ai/blog/posts/claude-api-pricing
- https://haimaker.ai/blog/claude-opus-vs-sonnet-vs-haiku/
- https://www.augmentcode.com/guides/ai-model-routing-guide
- https://www.codeant.ai/blogs/swe-bench-scores
- https://openai.com/index/introducing-gpt-5-3-codex/
- https://openai.com/index/introducing-gpt-5-5/
- https://openai.com/index/introducing-gpt-5-4-mini-and-nano/
- https://developers.openai.com/codex/models
- https://www.morphllm.com/codex-pricing
- https://tokenmix.ai/blog/gemini-2-5-pro-review
- https://llm-stats.com/blog/research/gemini-3.5-flash-launch
- https://blog.google/products/gemini/gemini-3-flash/
- https://www.metacto.com/blogs/the-true-cost-of-google-gemini-a-guide-to-api-pricing-and-integration
