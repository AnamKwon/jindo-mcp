# Per-Model Capability Profiles

Refreshed 2026-07-01. This document is the human-readable rationale behind
[`jindo/config/model_profiles.json`](../jindo/config/model_profiles.json), the
machine-readable per-model profile the router/eval layer consumes. It must be kept in
sync with that JSON and with [`jindo/config/models.json`](../jindo/config/models.json),
which owns the authoritative (agent, tier) → model-id mapping.

Related docs:
- [`docs/model_policy.md`](model_policy.md) — tier → agent recommendation and
  orchestration policy (which agent/model to pick per difficulty).
- [`docs/routing_policy.md`](routing_policy.md) — the deterministic scoring formula and
  tie-break that decides the tier in the first place.
- [`docs/llm_routing.md`](llm_routing.md) — the LLM-routing design overview.

## Confirmed available models (CLI probe, 2026-07-01)

These are the models the installed CLIs actually accept, confirmed by live probing (not
inferred from vendor pages):

| Agent | CLI | Model flag | Accepted models |
|---|---|---|---|
| `claude` | Claude Code 2.1.191 | `--model` | aliases `opus` / `sonnet` plus a `fable` alias, and full names e.g. `claude-opus-4-8`. A `fable` alias is listed by the CLI, but **Fable 5 (`claude-fable-5`) is currently blocked/unavailable**, so the top usable Claude model is **Opus 4.8**. Usable lineup now: **Opus 4.8** (`claude-opus-4-8`), **Sonnet 5** (`claude-sonnet-5` — the `sonnet` alias now resolves here), **Haiku 4.5** (`claude-haiku-4-5`). |
| `codex` | codex-cli 0.142.1 | `--model` | default configured `gpt-5.5` (in `~/.codex/config.toml`). Lineup: `gpt-5.5`, `gpt-5.3-codex-spark`, `gpt-5.4-mini`. **Note:** the bare `gpt-5.3-codex` id 400s on this ChatGPT-plan account ("not supported when using Codex with a ChatGPT account") — `-spark` is the id actually served. |
| `agy` (Gemini) | gemini CLI 0.46.0 | `-m` | `gemini-3.1-pro`, `gemini-3.5-flash`. |

Probe evidence: the `claude` CLI `--model` accepts the aliases `opus`/`sonnet` and a
`fable` alias, and full names such as `claude-opus-4-8`; the `codex` CLI default is
`gpt-5.5` (confirmed in `~/.codex/config.toml`); the `gemini` CLI selects with `-m`.

**A `fable` alias is listed by the Claude CLI, but Fable 5 (`claude-fable-5`) is
currently blocked/unavailable**, so the top usable Claude model is **Opus 4.8**. The
usable Claude lineup is Opus 4.8, Sonnet 5, and Haiku 4.5 — all wired into
`jindo/config/models.json` routing.

## How this differs from model_policy.md

`model_policy.md` answers *"for a given difficulty tier, which agent/model should lead?"*
This document answers *"what is each individual model actually good and bad at, and how
capable is it relative to the others?"* The JSON adds a normalized `capability_score`
(0..1) used for internal consistency checks and ordering, not as a contract benchmark.

## Verification and uncertainty flags

These are 2026 model IDs. Where a vendor publishes a number on its official model
page/announcement, it is marked **official**. Where the SWE-bench Verified figure is
only available from a leaderboard or a cross-vendor comparison table (not the model's own
announcement), it is marked **reported** — treat it as an ordering signal, not a contract.
Where a number is not verifiable for the exact model id, its benchmark is `null` in the
JSON (not fabricated):

- `gpt-5.4-mini` — SWE-bench Verified not published (Pro only). **null**.
- `gemini-3.5-flash` — SWE-bench Verified not published cleanly for this id (vendor
  reports Terminal-Bench / agentic suite; Flash-line Verified numbers vary by variant). **null**.

Reported (not on the official page): `claude-opus-4-8` SWE-bench Pro/Terminal figures
alongside its official Verified; `gpt-5.3-codex-spark` Verified (borrowed from the base `gpt-5.3-codex` line since it's the same tier); `gpt-5.5` Verified (OpenAI
reports #1 but via leaderboard framing). `gemini-3.1-pro` Verified is a cross-vendor
comparison figure; the id is `gemini-3.1-pro`, matching the published "Gemini 3.1 Pro"
naming (earlier ambiguity resolved) — treat the number as an ordering signal only.

## Mapping (from models.json)

| Agent | trivial | standard | hard |
|---|---|---|---|
| `claude` | `claude-haiku-4-5` | `claude-sonnet-5` | `claude-opus-4-8` |
| `codex` | `gpt-5.4-mini` | `gpt-5.3-codex-spark` | `gpt-5.5` |
| `agy` | `gemini-3.5-flash` | `gemini-3.1-pro` | `gemini-3.1-pro` |

`gemini-3.1-pro` occupies **both** agy standard and agy hard. It is recorded once in the
JSON with `tier: "hard"` (the higher of the two slots), and consumers resolve both agy
standard and agy hard to this single profile. Google ships no separate Opus-class coding
tier, so Pro is reused at the top.

## Capability scores (normalized 0..1, higher = more capable)

Scores are internally consistent: within an agent, hard > standard > trivial. Across
agents they broadly track coding/reasoning strength — the three flagships cluster high;
the mini/flash/haiku models sit lower. Opus 4.8 sits at 0.95, the top of the available
field.

| Model | Agent | Tier | capability_score |
|---|---|---|---|
| `claude-opus-4-8` | claude | hard | **0.95** |
| `gpt-5.5` | codex | hard | **0.93** |
| `gpt-5.3-codex-spark` | codex | standard | 0.88 |
| `gemini-3.1-pro` | agy | hard (also standard) | 0.84 |
| `claude-sonnet-5` | claude | standard | 0.87 |
| `gemini-3.5-flash` | agy | trivial | 0.66 |
| `claude-haiku-4-5` | claude | trivial | 0.62 |
| `gpt-5.4-mini` | codex | trivial | 0.58 |

---

## Model profiles

### claude-haiku-4-5 (claude · trivial)
- **SWE-bench Verified:** 73.3% (**official**; averaged over 50 trials, full 500-problem
  set, 128K thinking budget, no test-time compute).
- **Pricing:** ~$1 / $5 per M tokens. **Speed:** fastest tier.
- **Strengths:** cheap/fast triage, classification, formatting, single-line/IDE edits;
  Sonnet-4-class quality on shallow tasks at ~1/3 cost and >2x speed; ideal high-volume subagent.
- **Weaknesses:** falls behind on multi-file reasoning and long agent runs; not for the hardest problems.
- Sources: https://www.anthropic.com/news/claude-haiku-4-5 · https://www.morphllm.com/claude-benchmarks

### claude-sonnet-5 (claude · standard)
- **SWE-bench Verified:** 82.1% (**reported**; secondary trackers cluster 82.1-85.2%, no
  official Anthropic launch page indexed as of 2026-07 — treat as an ordering signal, not
  an exact figure). First Sonnet-tier model to clear 80% on Verified. **SWE-bench Pro:** 63.2%
  (up from Sonnet 4.6's 58.1%).
- **Pricing:** ~$3 / $15 per M tokens ($2 / $10 intro through 2026-08-31); 1M context, 128K max output.
- **Strengths:** near-Opus-tier quality on coding/agentic work at Sonnet cost; largest
  generational jump in the Sonnet line; adaptive thinking on by default; full
  low/medium/high/xhigh/max effort range (first Sonnet with `xhigh`); high-res (2576px) vision.
- **Weaknesses:** not the top scorer on the hardest agentic problems, defers to Opus 4.8;
  new tokenizer uses ~30% more tokens than Sonnet 4.6 for the same text — re-baseline
  token/cost budgets when migrating.
- Sources: https://www.buildfastwithai.com/blogs/claude-sonnet-5-review-benchmarks-pricing-2026 · https://www.marktechpost.com/2026/06/30/anthropic-claude-sonnet-5-vs-sonnet-4-6-vs-opus-4-8-agentic-coding-benchmarks-api-pricing-and-cost-performance-tradeoffs-compared/

### claude-opus-4-8 (claude · hard)
- **SWE-bench Verified:** 88.6% (**official**; Anthropic launch announcement 2026-05-28,
  up from 87.6% on Opus 4.7). **SWE-bench Pro:** 69.2% (field-leading among GA models).
  **Terminal-Bench 2.1:** 74.6%. **GPQA Diamond:** 93.6%.
- **Pricing:** ~$5 / $25 per M tokens (fast mode higher).
- **Strengths:** hardest reasoning, long-horizon multi-step planning, multi-file refactors,
  novel problems; leading SWE-bench Pro; strong GDPval economic-value tasks and knowledge work.
- **Weaknesses:** expensive tier; slower/lower throughput than the trivial and standard tiers.
- Sources: https://llm-stats.com/blog/research/claude-opus-4-8-launch · https://www.vellum.ai/blog/claude-opus-4-8-benchmarks-explained

### gpt-5.4-mini (codex · trivial)
- **SWE-bench Verified:** *not published* → **null** in JSON. **SWE-bench Pro:** 54.4%;
  **OSWorld-Verified:** ~72.1%; **GPQA:** ~88%.
- **Speed:** ~2x faster than GPT-5.4; strong performance-per-latency; ~6x lower cost than standard tier.
- **Strengths:** targeted edits, codebase navigation, front-end generation, low-latency
  debugging loops; cost-efficient Codex subagent delegate.
- **Weaknesses:** weaker long-context and pure-reasoning benchmarks; not for hard agentic coding.
- Sources: https://openai.com/index/introducing-gpt-5-4-mini-and-nano/ · https://www.digitalapplied.com/blog/gpt-5-4-mini-free-tier-54-swe-bench-pro-performance

### gpt-5.3-codex-spark (codex · standard)
- **SWE-bench Verified:** 85.0% (**reported** — leaderboard #3 for the base `gpt-5.3-codex`
  line; official page emphasizes SWE-bench Pro on its own scaffold). **SWE-bench Pro:** 56.8%.
- **Strengths:** state-of-the-art agentic coding and terminal tasks; solves with fewer
  tokens than prior models; first OpenAI model trained to identify software vulnerabilities.
- **Weaknesses:** Codex-specialized — non-coding knowledge work better on GPT-5.5; the base
  `gpt-5.3-codex` id is API/Team-plan only — live-probed 2026-07 on this ChatGPT-plan account,
  it returns HTTP 400 ("not supported when using Codex with a ChatGPT account"); the `-spark`
  suffix is the id actually served and must be used.
- Sources: https://openai.com/index/introducing-gpt-5-3-codex/ · https://www.marc0.dev/en/leaderboard

### gpt-5.5 (codex · hard)
- **SWE-bench Verified:** 88.7% (**reported** — OpenAI-reported #1 on the leaderboard,
  released 2026-04-23). **SWE-bench Pro:** 58.6%; **Terminal-Bench 2.0:** 82.7%; **GPQA
  Diamond:** ~92.8%.
- **Pricing:** premium tier.
- **Strengths:** strongest all-round OpenAI agentic coding, computer use, knowledge work;
  OpenAI's default "start here" for the most complex work; top-of-field Verified.
- **Weaknesses:** premium price; latency only matched (not improved) vs 5.4; SWE-bench Pro
  (58.6%) trails Opus 4.8 (69.2%).
- Sources: https://openai.com/index/introducing-gpt-5-5/ · https://www.marc0.dev/en/leaderboard

### gemini-3.5-flash (agy · trivial)
- **SWE-bench Verified:** *not published cleanly for this id* → **null** in JSON.
  **Terminal-Bench 2.1:** 76.2%; **MCP Atlas:** 83.6%; **CharXiv Reasoning:** 84.2%.
- **Speed/Pricing:** ~4x faster output than comparable frontier models; low cost.
- **Strengths:** outperforms the prior Pro tier on the coding/agentic suite at Flash cost;
  strong multi-step tool use and long-horizon execution; great for interactive dev and long agent loops.
- **Weaknesses:** trails Pro on the densest reasoning (HLE, ARC-AGI-2); coding sits
  between prior Pro and GPT-5.5 depending on the benchmark.
- Sources: https://blog.google/products-and-platforms/products/gemini/gemini-3-flash/ · https://deepmind.google/models/model-cards/gemini-3-5-flash/ · https://llm-stats.com/blog/research/gemini-3.5-flash-launch

### gemini-3.1-pro (agy · standard **and** hard)
- **SWE-bench Verified:** 76.2% (**reported** — cross-vendor comparison; the id is
  `gemini-3.1-pro`, matching the published "Gemini 3.1 Pro" naming, so treat the number as an
  ordering signal only).
- **Strengths:** strong coding with 1M-token context at competitive cost; broad multimodal,
  math, science, agentic capability. Carries both agy standard and agy hard.
- **Weaknesses:** not the single highest Verified score (mid-70s cluster); reused across two
  tiers, so not tuned specifically for the hardest tier.
- Sources: https://blog.google/innovation-and-ai/models-and-research/gemini-models/gemini-3-5/ · https://www.vellum.ai/blog/google-gemini-3-benchmarks
