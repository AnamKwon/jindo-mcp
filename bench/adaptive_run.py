#!/usr/bin/env python3
"""Run a token-bounded, evidence-escalating coding calibration campaign.

The campaign screens every requested model once without reviewers, repeats only
provider-diverse passing candidates, then reviews only candidates that remain
perfect across the requested repeats.  It delegates execution to run.py so the
fixture, CLI isolation, hidden-oracle, and review contracts stay in one place.
"""
from __future__ import annotations

import argparse
import json
import statistics
import subprocess
import sys
from pathlib import Path


HERE = Path(__file__).resolve().parent
RUNNER = HERE / "run.py"


def provider(model: str) -> str:
    return model.split(":", 1)[0]


def select_finalists(rows: list[dict], task: str, limit: int) -> list[str]:
    """Prefer one fast objective pass per provider, then fill by latency."""
    passing = [
        row for row in rows
        if row["task"] == task and row.get("passed") and row.get("exit") == 0
    ]
    fastest: dict[str, dict] = {}
    for row in passing:
        current = fastest.get(row["model"])
        if current is None or row["secs"] < current["secs"]:
            fastest[row["model"]] = row
    ordered = sorted(fastest.values(), key=lambda row: (row["secs"], row["model"]))
    selected: list[str] = []
    for agent in ("codex", "claude", "agy"):
        match = next((row for row in ordered if provider(row["model"]) == agent), None)
        if match and len(selected) < limit:
            selected.append(match["model"])
    for row in ordered:
        if len(selected) >= limit:
            break
        if row["model"] not in selected:
            selected.append(row["model"])
    return selected


def stable_finalists(rows: list[dict], task: str, models: list[str], repeats: int) -> list[str]:
    stable = []
    for model in models:
        attempts = [row for row in rows if row["task"] == task and row["model"] == model]
        repeat_ids = {row["repeat"] for row in attempts if row.get("passed") and row.get("exit") == 0}
        if len(attempts) == repeats and repeat_ids == set(range(repeats)):
            stable.append(model)
    return stable


def run_stage(args: list[str]) -> None:
    subprocess.run([sys.executable, str(RUNNER), *args], cwd=HERE.parent, check=True)


def load_rows(path: Path) -> list[dict]:
    return json.loads(path.read_text()) if path.exists() else []


def reviewed_outcomes(rows: list[dict]) -> list[dict]:
    outcomes = []
    for row in rows:
        reviews = [review for review in row.get("reviews", []) if not review.get("parse_error")]
        critical = sum(int(review.get("critical_findings", 0)) for review in reviews)
        outcomes.append({
            "model": row["model"],
            "fresh_pass": bool(row.get("passed") and row.get("exit") == 0),
            "review_count": len(reviews),
            "critical_findings": critical,
            "promotion_eligible": bool(row.get("passed") and row.get("exit") == 0 and reviews and critical == 0),
        })
    return outcomes


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--tasks", required=True, help="comma-separated task ids")
    parser.add_argument("--models", required=True, help="comma-separated inventory keys")
    parser.add_argument("--repeats", type=int, default=3)
    parser.add_argument("--max-finalists", type=int, default=4)
    parser.add_argument("--reviewers", default="codex:gpt-5.6-terra,claude:claude-fable-5,agy:Gemini 3.5 Flash (Medium)")
    parser.add_argument("--output-dir", type=Path, required=True)
    args = parser.parse_args()
    if args.repeats < 2:
        parser.error("--repeats must be >= 2")
    if args.max_finalists < 1:
        parser.error("--max-finalists must be >= 1")

    tasks = args.tasks.split(",")
    screen = args.output_dir.resolve() / "screen_and_repeats"
    reviewed = args.output_dir.resolve() / "reviewed"
    common = ["--tasks", args.tasks, "--output-dir", str(screen), "--resume"]
    run_stage([*common, "--models", args.models, "--repeats", "1", "--skip-review"])

    rows = load_rows(screen / "results.json")
    selected = {task: select_finalists(rows, task, args.max_finalists) for task in tasks}
    # Repeat each task's own shortlist. Taking the union here would recreate a
    # needless task x finalist cross-product and erase the screening savings.
    for task in tasks:
        if selected[task]:
            run_stage([
                "--tasks", task, "--output-dir", str(screen), "--resume",
                "--models", ",".join(selected[task]), "--repeats", str(args.repeats), "--skip-review",
            ])

    rows = load_rows(screen / "results.json")
    stable = {task: stable_finalists(rows, task, selected[task], args.repeats) for task in tasks}
    for task in tasks:
        if not stable[task]:
            continue
        run_stage([
            "--tasks", task, "--models", ",".join(stable[task]), "--repeats", "1",
            "--reviewers", args.reviewers, "--output-dir", str(reviewed / task), "--resume",
        ])

    summary = {}
    for task in tasks:
        task_rows = [row for row in rows if row["task"] == task and row["model"] in selected[task]]
        review_path = reviewed / task / "results.json"
        summary[task] = {
            "selected_after_screen": selected[task],
            "stable_after_repeats": stable[task],
            "median_seconds": {
                model: statistics.median(row["secs"] for row in task_rows if row["model"] == model)
                for model in selected[task]
            },
            "review_results": str(review_path),
            "reviewed_outcomes": reviewed_outcomes(load_rows(review_path)),
        }
    args.output_dir.mkdir(parents=True, exist_ok=True)
    (args.output_dir / "adaptive_manifest.json").write_text(json.dumps(summary, indent=2, ensure_ascii=False) + "\n")


if __name__ == "__main__":
    main()
