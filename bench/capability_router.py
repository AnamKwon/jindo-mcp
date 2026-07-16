#!/usr/bin/env python3
"""Evidence retrieval and reviewer planning for host-owned model routing.

This does not call or select a model. Exact cells expose a benchmark prior;
unmeasured cells expose the full catalog and related observations so the host
can form and verify a task-specific routing hypothesis.
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


def _candidate_evidence(policy: dict, candidates: list[dict]) -> list[dict]:
    model_evidence = policy.get("model_evidence", {})
    return [
        {
            "candidate": candidate,
            "evidence": model_evidence.get(candidate["model"], {
                "observed_strengths": [],
                "cautions": ["no model-specific local benchmark summary is available"],
                "operational_profile": "unknown",
            }),
        }
        for candidate in candidates
    ]


def _analogous_evidence(policy: dict, *, domain: str, task_type: str,
                         language: str | None, prompt_language: str | None) -> list[dict]:
    result = []
    for item in policy["measured_routes"]:
        if item["domain"] != domain:
            continue
        item_prompt = item.get("prompt_language") or None
        prompt_matches = item_prompt == prompt_language or (item_prompt is None and prompt_language in (None, "english"))
        is_exact = ((item["language"] or None) == language
                    and item["task_type"] == task_type
                    and prompt_matches)
        if is_exact:
            continue
        shared = ["domain"]
        if item["language"] and item["language"] == language:
            shared.append("language")
        if item["task_type"] == task_type:
            shared.append("task_type")
        if item.get("prompt_language") and item.get("prompt_language") == prompt_language:
            shared.append("prompt_language")
        if domain == "coding" and len(shared) == 1:
            continue
        result.append({
            "domain": item["domain"],
            "language": item["language"] or None,
            "prompt_language": item.get("prompt_language") or None,
            "task_type": item["task_type"],
            "shared_dimensions": shared,
            "evidence_status": item.get("evidence_status", "measured_single_repeat"),
            "reason": item.get("reason", "Local direct-CLI calibration in the displayed capability cell."),
            "candidates": item["candidates"],
            "required_oracle": item.get("required_oracle", []),
            "transfer_warning": "Analogous evidence is context for host judgment, not permission to transfer its winner.",
        })
    return result


def route(*, domain: str, task_type: str, language: str | None,
          risk: str, oracle: str, prompt_language: str | None = None,
          signals: dict | None = None,
          policy: dict | None = None) -> dict:
    policy = policy or load_policy()
    matches = [
        item for item in policy["measured_routes"]
        if item["domain"] == domain and (item["language"] or None) == language
        and item["task_type"] == task_type
        and ((item.get("prompt_language") or None) == prompt_language
             or (not item.get("prompt_language") and prompt_language in (None, "english")))
    ]
    measured = next((item for item in matches if item.get("prompt_language") == prompt_language), None)
    measured = measured or next(iter(matches), None)

    if measured:
        selected = measured
        evidence_status = measured.get("evidence_status", "measured_single_repeat")
        calibration_required = measured.get("calibration_required", False)
        multi_model = measured.get("mode", "cascade") == "parallel_compare"
        reason = measured.get("reason", "An exact domain-language-task cell exists in the local direct-CLI calibration.")
    elif domain == "coding":
        selected = {"candidates": []}
        evidence_status = "unmeasured_language_task_or_prompt_cell"
        evidence_gap = "No exact programming-language, task-type, and prompt-language benchmark cell."
        calibration_required = True
        multi_model = False
        reason = "The host must choose from the available catalog using concrete task signals, analogous evidence, and a task-local verification probe."
    elif domain in NONCODING_DOMAINS:
        selected = {"candidates": []}
        evidence_status = "unmeasured_subject_reasoning_or_prompt_cell"
        evidence_gap = "No exact subject, reasoning-form, and prompt-language benchmark cell."
        calibration_required = True
        multi_model = False
        reason = "The host must choose from the available catalog using concrete task signals, analogous evidence, and a task-local verification probe."
    else:
        raise ValueError(f"unsupported domain: {domain}")

    mode = measured.get("mode", "cascade") if measured else "host_decides"
    candidates = selected["candidates"]
    return {
        "domain": domain,
        "language": language,
        "prompt_language": prompt_language,
        "task_type": task_type,
        "risk": risk,
        "oracle": oracle,
        "signals": signals or {},
        "mode": mode,
        "exact_match": measured is not None,
        "evidence_status": evidence_status,
        "evidence_gap": "" if measured else evidence_gap,
        "calibration_required": calibration_required,
        "reason": reason,
        "candidates": candidates,
        "candidate_evidence": _candidate_evidence(policy, candidates),
        "eligible_models": _candidate_evidence(policy, policy["model_catalog"]),
        "analogous_evidence": _analogous_evidence(
            policy, domain=domain, task_type=task_type,
            language=language, prompt_language=prompt_language,
        ),
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
