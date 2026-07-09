# LLM Routing: FuGu findings, literature, and how our design differs

This note records what Sakana AI's FuGu reports say about *training* a router,
surveys the broader LLM-routing literature, and states precisely why our
`ModelRouter._llm_assess` seam (`jindo/router.py`) is **not** a trained router.
Every external claim carries a URL. Points that the sources leave thin are
flagged as uncertain rather than invented.

## FuGu findings

Sakana AI's FuGu is a *learned orchestrator*: instead of training one larger
model, it trains a small model to route/compose a pool of frontier agents
(GPT-5.5, Gemini-3.1-Pro, Claude Opus 4.8, etc.) per query.

- **Two model families.** "Jindo" is the fast single-agent router that picks the
  single most capable agent for an input; "Jindo-Ultra" is the multi-agent
  orchestrator that composes a workflow (topology + per-worker instructions).
  See the release post and landing page.
  <https://sakana.ai/jindo-release/>, <https://sakana.ai/jindo/>
- **How Jindo (single-agent router) is trained.** Per the technical report, a
  **two-stage, offline** pipeline:
  1. Supervised fine-tuning on single-step tasks. Input = query + each worker
     model's output on that query; the target is a *soft* distribution over
     which worker to pick, trained with a KL-divergence loss against a softmax
     of measured per-worker rewards.
  2. Evolutionary optimization (sep-CMA-ES) on end-to-end multi-turn tasks,
     maximizing a binary terminal reward (task completed or not); it predicts a
     single worker per conversation turn.
  Source: <https://arxiv.org/html/2606.21228v1> (report on alphaXiv:
  <https://www.alphaxiv.org/overview/2606.21228>).
- **How Jindo-Ultra (orchestrator) is trained.** Reinforcement learning (GRPO)
  on mixed public data plus expert-designed end-to-end environments; it emits a
  natural-language workflow (subtasks, worker assignments, communication
  topology). Reward = format correctness + outcome correctness (1.0 correct,
  0.5 partial). Same source as above.
- **Grounding papers.** FuGu is grounded in two ICLR 2026 papers, TRINITY
  (adaptive Thinker/Worker/Verifier roles) and the Conductor (RL-trained
  natural-language coordination). <https://sakana.ai/jindo/>
- **Uncertainty flag.** The landing/blog pages give little training detail; the
  concrete SFT+CMA-ES / GRPO description above comes from the arXiv report text.
  Exact hyperparameters, reward-normalization details, and the precise input
  featurization are **not** fully pinned down by the sources fetched here and
  should be treated as approximate. Community reimplementations
  (<https://github.com/SakanaAI/jindo>, <https://github.com/trotsky1997/OpenJindo>)
  may differ from the paper.

**Takeaway for us:** FuGu *learns* its routing policy offline from worker-reward
data and task outcomes. That is the design we are deliberately *not* copying.

## LLM-routing literature

- **RouteLLM — learning to route from preference data.** Trains router models
  that pick between a strong and a weak LLM at inference to cut cost without
  losing quality; uses human preference data + augmentation, and transfers
  across model pairs. <https://arxiv.org/abs/2406.18665>
- **kNN can beat learned routers.** A study arguing that simple predictive
  models (kNN over embeddings) often match or beat complex learned routers,
  i.e. heavyweight training is not always justified.
  <https://arxiv.org/pdf/2505.12601>
- **LLM-as-a-judge / classifier routing.** An LLM (or a trained classifier)
  scores/classifies a query and emits a decision plus reasoning; for reliable
  parsing the judge should produce **structured output**.
  <https://langfuse.com/docs/evaluation/evaluation-methods/llm-as-a-judge>
- **Confidence-based routing.** Route using a confidence score in [0,1] — e.g.
  CARGO (confidence-aware routing) and confidence-token routing — often via a
  trained regressor/classifier stage. <https://arxiv.org/pdf/2509.14899>,
  <https://arxiv.org/pdf/2410.13284>
- **Structured-output routing.** Contextual evaluation-prompt routing improves
  reliability of LLM-based structured evaluation and reduces hallucination.
  See the LLM-as-a-judge structured-output study:
  <https://pmc.ncbi.nlm.nih.gov/articles/PMC12319771/>

## How our design differs

We do **not** train a router. There is no offline dataset of worker rewards, no
SFT/RL/CMA-ES stage, and no learned weights. The deterministic tier decision
lives in `jindo/scoring.py` (`score_task`), driven entirely by the
pattern/weight/threshold policy in `jindo/config/routing_policy.json`.

Our optional, *prompt-time* refinement is the `ModelRouter._llm_assess` seam in
`jindo/router.py`. At call time — and only when the deterministic total sits in
the ambiguous band around a threshold (`_in_ambiguous_band`) — we prompt a small
CLI model (e.g. Claude Haiku or Gemini Flash) for a **structured per-signal
assessment**, then fall back to the deterministic scorer whenever the LLM is
unavailable or its output is invalid.

Contrast:

| Axis            | FuGu                                   | Ours                                             |
|-----------------|----------------------------------------|--------------------------------------------------|
| When decided    | Trained offline, applied at inference  | Prompt-time only, inside the ambiguous band      |
| Router artifact | Learned weights (SFT + CMA-ES / GRPO)  | No learned artifact; a prompt + JSON contract    |
| Signals         | Worker rewards, task outcomes          | The four `scoring.py` signals, re-assessed by LLM |
| Failure mode    | Serve the trained policy               | Deterministic fallback (`scoring.py`)            |
| Output          | Worker choice / workflow topology      | Structured tier assessment (JSON below)          |

The LLM here is an advisory tie-breaker over a deterministic, auditable
baseline — closest in spirit to the *LLM-as-judge / structured-output* line of
work above, not to trained cost/quality routers like RouteLLM or FuGu.

## Assessment JSON schema

`_llm_assess` expects the CLI model to emit exactly this object on stdout:

```json
{
  "scores": {
    "security": 0,
    "constraints": 0,
    "scope": 0,
    "ambiguity": 0
  },
  "tier": "trivial",
  "confidence": 0.0,
  "reason": ""
}
```

Field contract:

- `scores` — one numeric entry per deterministic signal, mirroring
  `jindo/scoring.py` / `routing_policy.json`: **`security`, `constraints`,
  `scope`, `ambiguity`**. These parallel the scorer's per-signal weighted
  contributions.
- `tier` — one of `"trivial"`, `"standard"`, `"hard"` (the same tier vocabulary
  the scorer emits). This is the only field `_apply_hybrid` acts on today: a
  valid `tier` replaces the deterministic tier and the `reason` is appended to
  the result's reason string.
- `confidence` — float in `[0, 1]`, the model's self-reported confidence.
- `reason` — short human-readable justification.

### Robustness contract

`_llm_assess` returns `None` (=> deterministic fallback, tier untouched) on
**any** of:

- a nonzero CLI exit code, or a missing/timed-out CLI invocation;
- output that does not parse as JSON;
- parsed JSON missing the `tier` key, or whose `tier` is not one of
  `trivial | standard | hard`.

Because `_apply_hybrid` only trusts an assessment whose `tier` is a valid
difficulty, a `None` or malformed result leaves the deterministic `scoring.py`
tier in place. The deterministic scorer is thus always the ground truth; the LLM
can only nudge the tier inside the ambiguous band, never crash or silently
corrupt routing.

### Concrete example

Given the task "add OAuth token refresh with a race-free mutex", a valid
assessment might be:

```json
{
  "scores": {
    "security": 3,
    "constraints": 2,
    "scope": 1,
    "ambiguity": 1
  },
  "tier": "hard",
  "confidence": 0.82,
  "reason": "security-sensitive token handling plus concurrency (mutex) => hard"
}
```

Here `_apply_hybrid` (if the deterministic total is in the ambiguous band) would
set `tier = "hard"` and append the `reason`. If instead the CLI returned, say,
`{"scores": {...}, "confidence": 0.4}` (no `tier`) or non-JSON text or a nonzero
exit, `_llm_assess` returns `None` and the deterministic tier stands.
