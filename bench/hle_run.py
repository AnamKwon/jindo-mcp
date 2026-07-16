#!/usr/bin/env python3
"""Direct-CLI, image-free HLE calibration stratified by subject.

The runner batches questions to avoid measuring CLI startup overhead as subject
ability. It uses the local HLE parquet's objective multiple-choice answers and
never invokes jindo, MCP, skills, web search, or a judge model.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import re
import shutil
import tempfile
import time
from collections import defaultdict
from pathlib import Path

import run as coding_bench

REPO = Path(__file__).resolve().parents[1]
DEFAULT_DATASET = Path("/Users/anamkwon/hle/hle_eval/data/test-00000-of-00001.parquet")
OUT = REPO / "bench" / "calibration" / "hle"
WORK = Path(tempfile.gettempdir()) / "jindo-hle-routing-bench"

SUBJECTS = {
    "Math": "mathematics",
    "Biology/Medicine": "biology",
    "Physics": "physics",
    "Chemistry": "chemistry",
    "Computer Science/AI": "computer_science_theory",
    "Humanities/Social Science": "history_humanities",
}


def load_items(dataset: Path, per_domain: int, seed: str = "jindo-hle-v1") -> list[dict]:
    import pandas as pd

    frame = pd.read_parquet(dataset)
    image_free = frame["image"].isna() | (frame["image"].astype(str).str.len() == 0)
    frame = frame[(frame["answer_type"] == "multipleChoice") & image_free]
    items = []
    for category, domain in SUBJECTS.items():
        rows = frame[frame["category"] == category].to_dict("records")
        rows.sort(key=lambda row: hashlib.sha256(f"{seed}|{row['id']}".encode()).hexdigest())
        if len(rows) < per_domain:
            raise ValueError(f"{category}: need {per_domain} items, found {len(rows)}")
        for row in rows[:per_domain]:
            items.append({
                "id": str(row["id"]), "domain": domain,
                "question": str(row["question"]), "answer": str(row["answer"]),
            })
    return items


def prompt_for(items: list[dict]) -> str:
    questions = [{"id": item["id"], "question": item["question"]} for item in items]
    return """Answer every question independently. Do not use MCP, plugins, skills,
web search, files, or other agents. Return exactly one JSON object and no prose:
{"answers":[{"id":"question id","answer":"choice letter or exact True/False"}]}
Keep every id unchanged and include each exactly once. Questions:
""" + json.dumps(questions, ensure_ascii=False)


def parse_answers(text: str) -> dict[str, str]:
    decoder = json.JSONDecoder()
    for match in re.finditer(r"\{", text):
        try:
            value, _ = decoder.raw_decode(text[match.start():])
        except json.JSONDecodeError:
            continue
        if not isinstance(value, dict) or not isinstance(value.get("answers"), list):
            continue
        answers: dict[str, str] = {}
        valid = True
        for item in value["answers"]:
            if not isinstance(item, dict) or "id" not in item or "answer" not in item:
                valid = False
                break
            key = str(item["id"])
            if key in answers:
                valid = False
                break
            answers[key] = str(item["answer"])
        if valid:
            return answers
    return {}


def normalize_answer(value: str) -> str:
    value = value.strip()
    choice = re.fullmatch(r"([A-Ea-e])(?:[.)])?", value)
    if choice:
        return choice.group(1).upper()
    if value.lower() in {"true", "false"}:
        return value.lower().title()
    return " ".join(value.split()).casefold()


def score_batch(items: list[dict], answers: dict[str, str]) -> list[dict]:
    return [{
        "id": item["id"], "domain": item["domain"],
        "expected": item["answer"], "answer": answers.get(item["id"], ""),
        "correct": normalize_answer(answers.get(item["id"], "")) == normalize_answer(item["answer"]),
    } for item in items]


def batches(items: list[dict], size: int) -> list[list[dict]]:
    return [items[i:i + size] for i in range(0, len(items), size)]


def summarize(rows: list[dict]) -> list[dict]:
    grouped: dict[tuple[str, str], list[bool]] = defaultdict(list)
    for row in rows:
        for item in row.get("items", []):
            grouped[(row["model"], item["domain"])].append(bool(item["correct"]))
    output = []
    for (model, domain), values in sorted(grouped.items()):
        output.append({
            "model": model, "domain": domain, "attempts": len(values),
            "correct": sum(values), "accuracy": round(sum(values) / len(values), 4),
        })
    return output


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dataset", type=Path, default=DEFAULT_DATASET)
    parser.add_argument("--items-per-domain", type=int, default=5)
    parser.add_argument("--batch-size", type=int, default=10)
    parser.add_argument("--models", help="comma-separated inventory keys")
    parser.add_argument("--repeats", type=int, default=1)
    parser.add_argument("--timeout", type=int, default=600,
                        help="per model-batch wall-clock limit in seconds")
    parser.add_argument("--output-dir", type=Path, default=OUT,
                        help="campaign-specific artifact directory")
    parser.add_argument("--resume", action="store_true")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    if args.items_per_domain < 1 or args.batch_size < 1 or args.repeats < 1 or args.timeout < 1:
        parser.error("items-per-domain, batch-size, repeats, and timeout must be >= 1")

    items = load_items(args.dataset, args.items_per_domain)
    models = coding_bench.selected(coding_bench.inventory(), args.models, "key")
    item_batches = batches(items, args.batch_size)
    print(f"matrix: {len(models)} models x {len(item_batches)} batches x {args.repeats} repeats; {len(items)} items")
    if args.dry_run:
        print(json.dumps({"models": [m["key"] for m in models], "items": [i["id"] for i in items]}, indent=2))
        return

    out = args.output_dir.resolve()
    out.mkdir(parents=True, exist_ok=True)
    WORK.mkdir(parents=True, exist_ok=True)
    raw_path = out / "results.json"
    rows = json.loads(raw_path.read_text()) if args.resume and raw_path.exists() else []
    # Transport/CLI failures are retryable on --resume and replaced in place;
    # exit=0 format omissions remain real instruction-following outcomes.
    done = {(row["model"], row["repeat"], row["batch"]) for row in rows if row.get("exit") == 0}
    for model in models:
        for repeat in range(args.repeats):
            for batch_index, batch in enumerate(item_batches):
                identity = (model["key"], repeat, batch_index)
                if identity in done:
                    continue
                root = WORK / hashlib.sha256("|".join(map(str, identity)).encode()).hexdigest()[:12]
                root.mkdir(parents=True, exist_ok=True)
                # Codex refuses even read-only execution outside a trusted git
                # worktree. The empty repository is only a CLI trust anchor;
                # questions and answers remain in the prompt/result artifacts.
                if not (root / ".git").exists():
                    coding_bench.run(["git", "init", "-q"], cwd=root, timeout=20)
                cmd = coding_bench.reviewer_command(model, prompt_for(batch), root)
                started = time.time()
                try:
                    proc = coding_bench.run(cmd.argv, cwd=root, env=cmd.env, timeout=args.timeout)
                    output = proc.stdout + "\n" + proc.stderr
                    answers = parse_answers(output)
                    error = None
                    exit_code = proc.returncode
                except Exception as exc:  # persisted as a failed batch; campaign remains resumable
                    output, answers, error, exit_code = "", {}, str(exc), -1
                row = {
                    "model": model["key"], "repeat": repeat, "batch": batch_index,
                    "seconds": round(time.time() - started, 2), "exit": exit_code,
                    "parse_ok": set(answers) == {item["id"] for item in batch},
                    "items": score_batch(batch, answers), "error": error,
                    "output": output[-12000:],
                }
                rows = [old for old in rows if (old["model"], old["repeat"], old["batch"]) != identity]
                rows.append(row)
                raw_path.write_text(json.dumps(rows, indent=2, ensure_ascii=False))
                correct = sum(item["correct"] for item in row["items"])
                print(f"{model['key']} #{repeat} batch {batch_index}: {correct}/{len(batch)} parse={row['parse_ok']}", flush=True)
                shutil.rmtree(root, ignore_errors=True)
    summary = summarize(rows)
    (out / "summary.json").write_text(json.dumps(summary, indent=2, ensure_ascii=False))
    (out / "items.json").write_text(json.dumps(items, indent=2, ensure_ascii=False))
    print(f"wrote {out / 'summary.json'}")


if __name__ == "__main__":
    main()
