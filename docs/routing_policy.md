# Model-Routing Policy: LLM-Primary with Deterministic Fallback

`ModelRouter.select()` uses a **two-tier hybrid strategy**: it first consults an
optional LLM router (`_llm_assess`) for a structured assessment (enabled via
`llm_routing.enabled` in `jindo/config/routing_policy.json`), and falls back to a
deterministic multi-signal scorer on any failure or when disabled (the default).
Both paths assign a difficulty tier (`trivial`, `standard`, or `hard`). The result
includes `source` (`'llm'` | `'deterministic'`) and `confidence` (`float` | `None`).

This document specifies the **deterministic scorer** — the auditable, cost-free
baseline that always stands when the LLM path is unavailable. The machine-readable
policy weights and thresholds live in `jindo/config/routing_policy.json`. For the
LLM assessment schema and robustness contract, see [`docs/llm_routing.md`](llm_routing.md).

## Why route by difficulty *and* risk

Routing exists because model capability trades off against cost, latency, and —
less obviously — *attack surface*. The literature gives two independent reasons
to escalate a task to a larger model:

- **Difficulty (cost/quality).** RouteLLM shows that sending only the queries a
  weak model *can't* handle to the strong model preserves ~95% of GPT-4 quality
  at 35-85% lower cost, and that a single scalar threshold controls the
  cost/quality trade-off, calibrated on a query sample.
  ([RouteLLM blog](https://www.lmsys.org/blog/2024-07-01-routellm/),
  [RouteLLM paper, arXiv:2406.18665](https://arxiv.org/abs/2406.18665))
  FrugalGPT reaches the strong model's accuracy at up to 98% lower cost by
  cascading: query the cheap model first, score confidence, escalate only when
  confidence is low — but a badly tuned threshold "either leaks errors or
  escalates too often."
  ([FrugalGPT, arXiv:2305.05176](https://arxiv.org/pdf/2305.05176))
- **Risk (safety), which cost-only routers ignore.** For agents with tool
  access, model selection is "an implicit trust decision": the blast radius of a
  prompt-injection or a wrong answer *scales with the stakes of the task*, not
  with its token difficulty. Routing a security-sensitive task to an
  under-capable model produces "undetected failures" and "false confidence" —
  precisely the failures that are most expensive to discover late.
  ([Least Privilege for LLM Agents](https://medium.com/@michael.hannecke/least-privilege-for-llm-agents-applying-security-principles-to-model-selection-57760accb041))
  Risk-aware routers such as RACER make this explicit by calibrating routing to
  the *risk* of a cheap-model error, not only its expected quality.
  ([RACER, arXiv:2603.06616](https://arxiv.org/pdf/2603.06616),
  [RerouteGuard, arXiv:2601.21380](https://arxiv.org/pdf/2601.21380))

### Why a deterministic keyword scorer (not a learned classifier)

Difficulty can be estimated by learned classifiers *or* by cheap heuristics:
"text length, word rarity, idiomatic language, syntactic complexity ... or
leveraging the LLMs themselves"
([Dynamic Model Routing survey, arXiv:2603.04445](https://arxiv.org/pdf/2603.04445)).
We use a deterministic keyword/substring scorer as the first stage because it is
zero-cost, reproducible, auditable, and cannot itself be rerouted by adversarial
input the way a learned router can. The learned/LLM stage only nudges tiers
inside a narrow *ambiguous band* (below), keeping the common path deterministic.

## The four signals

Each signal is a lowercase substring/keyword list. A prompt is lowercased once;
a signal's **raw score is the number of *distinct* patterns from its list that
occur as substrings** (each pattern counted at most once, no matter how often it
appears). Substring matching is intentional so morphological variants are caught
by a short root (e.g. `auth` matches "authentication"/"authorize"; `encrypt`
matches "encrypted"/"encryption"; `validat` matches "validate"/"validation").

| Signal        | Weight | What it detects                                    |
|---------------|--------|----------------------------------------------------|
| `security`    | 3.0    | Security/safety sensitivity — auth, secrets, crypto, injection, access control |
| `constraints` | 1.5    | Many hard conditions/requirements — validation, retries, timeouts, concurrency, transactions |
| `scope`       | 1.2    | Breadth / cross-cutting change — refactors, multi-service, migrations, system-wide |
| `ambiguity`   | 0.8    | Under-specification — "optimize", "robust", "appropriate", "etc" |

- **`security` (weight 3.0, heaviest).** Patterns: `auth`, `password`, `token`,
  `secret`, `encrypt`, `decrypt`, `credential`, `permission`, `privilege`,
  `vulnerab`, `injection`, `sanitize`, `certificate`, `sensitive`,
  `access control`, `sql`, `csrf`, `xss`, `jwt`, `oauth`, `hash`, `tls`, `ssl`.
  A security keyword is a *risk* signal, not merely a difficulty signal: the
  least-privilege and RACER work above argues that a wrong small model on a
  security task carries an asymmetric, hard-to-detect cost. So a *single*
  security match (3.0) alone already clears `standard`, and roughly two clear
  the way toward `hard`.
- **`constraints` (weight 1.5).** Patterns: `must`, `should`, `only`,
  `validat`, `retry`, `timeout`, `transaction`, `edge case`, `concurrent`,
  `atomic`, `idempotent`, `rollback`, `constraint`, `requirement`, `invariant`,
  `race condition`, `concurren`, `thread`, `deadlock`, `rate limit`, `async`,
  `parallel`, `lock`.
  Each extra hard condition raises the chance a weak model drops one, matching
  the difficulty rationale of cascades.
- **`scope` (weight 1.2).** Patterns: `refactor`, `architecture`, `across`,
  `multiple`, `service`, `module`, `migrate`, `integrate`, `system`, `pipeline`,
  `end-to-end`, `cross-cutting`, `distributed`, `orchestrat`, `implement`,
  `endpoint`, `api`, `feature`, `component`, `function`. Breadth correlates
  with the reasoning depth larger models are better at.
- **`ambiguity` (weight 0.8, lightest).** Patterns: `somehow`, `etc`,
  `and so on`, `optimize`, `improve`, `better`, `appropriate`, `robust`,
  `flexible`, `scalable`, `clean`, `efficient`, `as needed`,
  `handle everything`, `production-grade`. Vagueness weakly suggests a task needs
  judgment, but on its own is the least reliable escalation reason, so it is
  weighted lowest.

Weights are deliberately ordered `security > constraints > scope > ambiguity`,
encoding "match model capability to task *sensitivity*, not just difficulty."

## Total-score formula (scorer contract)

The scorer MUST implement exactly this:

```
text = prompt.lower()
for each signal s in {security, constraints, scope, ambiguity}:
    raw[s]          = count of DISTINCT patterns p in s.patterns
                      such that p occurs as a substring of text
    contribution[s] = s.weight * raw[s]
total = sum of contribution[s] over the four signals

tier = "hard"     if total >= thresholds.hard        (6.0)
     = "standard" elif total >= thresholds.standard   (1.0)
     = "trivial"  otherwise
```

`thresholds.standard` (1.0) `<` `thresholds.hard` (6.0), strictly.

## Ambiguous band

`ambiguous_band.width = 1.0`. A total within `width` of *either* threshold —
i.e. in `[standard - width, standard + width]` or `[hard - width, hard + width]`
(here `[0.0, 2.0]` or `[5.0, 7.0]`) — sits close enough to a boundary that the
deterministic tier is not confident. Only for totals inside such a band may a
downstream LLM "thinker" nudge the task up or down one tier; totals comfortably
outside every band are fixed deterministically and never re-judged. This mirrors
the cascade insight that the threshold region is where escalation decisions are
genuinely uncertain, while keeping the fast path cheap and reproducible.

## Worked examples (the two database prompts)

**A. "connect to a database"** — lowercased, no pattern in any list is a
substring (`database`/`connect` are deliberately *not* patterns).

| Signal | Matches | raw | weight | contribution |
|--------|---------|-----|--------|--------------|
| security | — | 0 | 3.0 | 0.0 |
| constraints | — | 0 | 1.5 | 0.0 |
| scope | — | 0 | 1.2 | 0.0 |
| ambiguity | — | 0 | 0.8 | 0.0 |

**total = 0.0** `< 2.0 (standard)` → **`trivial`**.

**B. "connect to a database with authentication, input validation, and encrypted
credentials, handling concurrent access"**

| Signal | Distinct matches | raw | weight | contribution |
|--------|------------------|-----|--------|--------------|
| security | `auth` (authentication), `encrypt` (encrypted), `credential` (credentials) | 3 | 3.0 | 9.0 |
| constraints | `validat` (validation), `concurrent` | 2 | 1.5 | 3.0 |
| scope | — | 0 | 1.2 | 0.0 |
| ambiguity | — | 0 | 0.8 | 0.0 |

**total = 12.0** `>= 6.0 (hard)` → **`hard`**.

The heavy security weight is what carries prompt B decisively over the `hard`
threshold: its three security matches alone (9.0) already exceed 6.0, so the
policy refuses to route a task touching auth, encryption, and credentials to a
small model — the asymmetric-risk conclusion of the risk-aware routing research.

## Sources

- RouteLLM: https://www.lmsys.org/blog/2024-07-01-routellm/ and https://arxiv.org/abs/2406.18665
- FrugalGPT: https://arxiv.org/pdf/2305.05176
- Dynamic Model Routing and Cascading (survey): https://arxiv.org/pdf/2603.04445
- Least Privilege for LLM Agents (risk-based model selection): https://medium.com/@michael.hannecke/least-privilege-for-llm-agents-applying-security-principles-to-model-selection-57760accb041
- RACER (Risk-Aware Calibrated Efficient Routing): https://arxiv.org/pdf/2603.06616
- RerouteGuard (adversarial risks for LLM routing): https://arxiv.org/pdf/2601.21380
