#!/usr/bin/env python3
"""Evidence-bounded task and reviewer routing for the host.

This does not call a model. It turns explicit task metadata into a candidate
ladder and review strategy while keeping unmeasured capability cells visible.
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path

POLICY_PATH = Path(__file__).resolve().parents[1] / "internal" / "routing" / "config" / "capability_policy.json"
NONCODING_DOMAINS = {
    "mathematics", "physics", "chemistry", "biology", "computer_science_theory",
    "medicine", "law", "history_humanities", "social_science", "general_knowledge",
}


def load_policy(path: Path = POLICY_PATH) -> dict:
    return json.loads(path.read_text())


def _review_strategy(policy: dict, *, risk: str, oracle: str, multi_model: bool) -> dict:
    count = 2 if risk == "high" or oracle == "none" else 1
    if multi_model:
        count = max(count, 2)
    return {
        "status": policy["review_policy"]["status"],
        "minimum_independent_reviewers": count,
        "pool": policy["review_policy"]["pool"],
        "exclude_answer_author": True,
        "acceptance": "objective_oracle_and_review_agree" if oracle != "none" else "two_reviews_agree_then_human_check",
    }


def route(*, domain: str, task_type: str, language: str | None,
          risk: str, oracle: str, prompt_language: str | None = None,
          policy: dict | None = None) -> dict:
    policy = policy or load_policy()
    matches = [
        item for item in policy["measured_routes"]
        if item["domain"] == domain and (item["language"] or None) == language
        and item["task_type"] == task_type
        and (not item.get("prompt_language") or item.get("prompt_language") == prompt_language)
    ]
    measured = next((item for item in matches if item.get("prompt_language")), None)
    measured = measured or next(iter(matches), None)

    if measured:
        selected = measured
        evidence_status = measured.get("evidence_status", "measured_single_repeat")
        calibration_required = measured.get("calibration_required", False)
        multi_model = measured.get("mode", "cascade") == "parallel_compare"
        reason = measured.get("reason", "An exact domain-language-task cell exists in the local direct-CLI calibration.")
    elif domain == "coding" and task_type == "mechanical":
        selected = policy["fallbacks"]["coding_mechanical"]
        evidence_status = "tier_prior_only"
        calibration_required = language not in (None, "go")
        multi_model = False
        reason = "Mechanical work uses the cheap verified cascade, but it is not language-specific evidence."
    elif domain == "coding":
        selected = policy["fallbacks"]["coding_unmeasured"]
        evidence_status = "unmeasured_language_or_task_type"
        calibration_required = True
        multi_model = risk == "high" or oracle == "none"
        reason = "No exact language and task-type calibration cell exists; use a general coding prior and verify."
    elif domain in NONCODING_DOMAINS:
        selected = policy["fallbacks"]["noncoding_unmeasured"]
        evidence_status = "unmeasured_domain"
        calibration_required = True
        multi_model = True
        reason = "Current local results contain no HLE-like subject evidence; candidates are provider-diverse, not ranked by subject strength."
    else:
        raise ValueError(f"unsupported domain: {domain}")

    if oracle == "none":
        multi_model = True
    mode = "parallel_compare" if multi_model else "cascade"
    candidates = selected["candidates"]
    model_evidence = policy.get("model_evidence", {})
    return {
        "domain": domain,
        "language": language,
        "prompt_language": prompt_language,
        "task_type": task_type,
        "risk": risk,
        "oracle": oracle,
        "mode": mode,
        "evidence_status": evidence_status,
        "calibration_required": calibration_required,
        "reason": reason,
        "candidates": candidates,
        "candidate_evidence": [
            {
                "candidate": candidate,
                "evidence": model_evidence.get(candidate["model"], {
                    "observed_strengths": [],
                    "cautions": ["no model-specific local benchmark summary is available"],
                    "operational_profile": "unknown",
                }),
            }
            for candidate in candidates
        ],
        "required_oracle": selected.get("required_oracle", []),
        "review": _review_strategy(policy, risk=risk, oracle=oracle, multi_model=multi_model),
        "host_selection": policy["host_selection"],
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--domain", required=True)
    parser.add_argument("--task-type", required=True)
    parser.add_argument("--language")
    parser.add_argument("--prompt-language", choices=("english", "korean", "japanese", "multilingual_mixed"))
    parser.add_argument("--risk", choices=("low", "normal", "high"), default="normal")
    parser.add_argument("--oracle", choices=("deterministic", "exact_answer", "judge", "none"), default="none")
    args = parser.parse_args()
    try:
        result = route(domain=args.domain, task_type=args.task_type, language=args.language,
                       risk=args.risk, oracle=args.oracle, prompt_language=args.prompt_language)
    except ValueError as exc:
        parser.error(str(exc))
    print(json.dumps(result, indent=2, ensure_ascii=False))


if __name__ == "__main__":
    main()
