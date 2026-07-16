#!/usr/bin/env python3
"""Direct-CLI delegation benchmark harness.

Compares:
1) large model coding directly
2) small model coding directly
3) large model planning (read-only) + independent small model implementation

Reuses ``bench.run`` helpers for inventory, fixture setup, command execution,
verification, and review scoring while avoiding JINDO/MCP and plugins.
"""
from __future__ import annotations

import argparse
import importlib.util
import json
import os
import re
import shutil
import statistics
import subprocess
import time
from pathlib import Path

import run as base_run

try:
    from delegation_hard_tasks import TASKS as HARD_TASKS
except ModuleNotFoundError:
    _spec = importlib.util.spec_from_file_location(
        "delegation_hard_tasks",
        str(Path(__file__).with_name("delegation_hard_tasks.py")),
    )
    if _spec is None or _spec.loader is None:
        raise SystemExit("failed to load delegation_hard_tasks fixture module")
    _module = importlib.util.module_from_spec(_spec)
    _spec.loader.exec_module(_module)
    HARD_TASKS = _module.TASKS

TASKS = sorted(base_run.TASKS + HARD_TASKS, key=lambda task: task["id"])

REPO = Path(__file__).resolve().parents[1]
OUT = REPO / "bench" / "calibration" / "delegation"
TIMEOUT = int(os.environ.get("JINDO_BENCH_TIMEOUT", "1800"))
RAW_OUTPUT_LIMIT = 12_000
REPEATS_DEFAULT = 3
CONDITIONS = ("large_direct", "small_direct", "large_plan_small")

# USD cost per 1M tokens; override with --price-table.
DEFAULT_PRICE_TABLE = {
    "codex:gpt-5.6-sol": {"input_per_million": 5.0, "output_per_million": 30.0},
    "codex:gpt-5.6-terra": {"input_per_million": 2.5, "output_per_million": 15.0},
    "codex:gpt-5.6-luna": {"input_per_million": 1.0, "output_per_million": 6.0},
    "codex:gpt-5.5": {"input_per_million": 5.0, "output_per_million": 30.0},
    "codex:gpt-5.4-mini": {"input_per_million": 0.75, "output_per_million": 4.5},
    "codex:gpt-5.3-codex-spark": {"input_per_million": 0.6, "output_per_million": 2.4},
    "claude:claude-opus-4-8": {"input_per_million": 3.0, "output_per_million": 15.0},
    "claude:claude-sonnet-5": {"input_per_million": 3.0, "output_per_million": 15.0},
    "claude:claude-fable-5": {"input_per_million": 3.0, "output_per_million": 15.0},
    "claude:claude-haiku-4-5": {"input_per_million": 0.8, "output_per_million": 4.0},
}

TOKEN_RE_TEXT = [
    ("input_tokens", re.compile(r"\b(?:input|prompt)\s*tokens?(?:\s+used)?\b[:=\s]+([\d,]+)", re.I)),
    ("output_tokens", re.compile(r"\b(?:output|completion)\s*tokens?(?:\s+used)?\b[:=\s]+([\d,]+)", re.I)),
    ("total_tokens", re.compile(r"\b(?:total\s+)?tokens\s+used\b[:=\s]+([\d,]+)", re.I)),
    ("total_tokens", re.compile(r"\b([\d,]+)\s+(?:tokens|token)\s+used\b", re.I)),
]



def bounded_output(text: str, limit: int = RAW_OUTPUT_LIMIT) -> str:
    if text is None:
        return ""
    return text[-limit:]


def to_int(raw: str | None) -> int | None:
    if raw is None:
        return None
    try:
        return int(raw.replace(",", ""))
    except ValueError:
        return None


def parse_token_usage(text: str) -> dict:
    """Parse token usage counts from CLI output.

    CLI tools print running/partial usage lines during a session and a final
    trailing summary at the end; only the last parseable occurrence of each
    field is trusted, since that is the authoritative total. The raw text is
    not duplicated here -- it is already retained on the stage result.
    """
    usage: dict[str, int | None] = {
        "input_tokens": None,
        "output_tokens": None,
        "total_tokens": None,
    }
    parse_error = False
    try:
        if text:
            best_pos: dict[str, int] = {}
            for key, regex in TOKEN_RE_TEXT:
                for match in regex.finditer(text):
                    value = to_int(match.group(1))
                    if value is not None and match.end() > best_pos.get(key, -1):
                        usage[key] = value
                        best_pos[key] = match.end()
    except Exception:
        parse_error = True

    if parse_error:
        coverage = "error"
    elif usage["input_tokens"] is not None and usage["output_tokens"] is not None:
        coverage = "exact"
    elif usage["total_tokens"] is not None:
        coverage = "total_only" if usage["input_tokens"] is None and usage["output_tokens"] is None else "partial"
    elif usage["input_tokens"] is not None or usage["output_tokens"] is not None:
        coverage = "partial"
    else:
        coverage = "none"

    return {
        "input_tokens": usage["input_tokens"],
        "output_tokens": usage["output_tokens"],
        "total_tokens": usage["total_tokens"],
        "coverage": coverage,
        "parse_error": parse_error,
    }


def estimate_cost(token_usage: dict | None, price: dict | None) -> tuple[float | None, str]:
    """Estimate USD from parsed tokens and per-model price.

    ``None`` is returned if required fields are unavailable to avoid over-claiming.
    """
    if not token_usage or not price:
        return None, "missing_usage_or_price"

    in_price = price.get("input_per_million")
    out_price = price.get("output_per_million")
    if not isinstance(in_price, (int, float)) or not isinstance(out_price, (int, float)):
        return None, "invalid_price"

    input_tokens = token_usage.get("input_tokens")
    output_tokens = token_usage.get("output_tokens")
    total_tokens = token_usage.get("total_tokens")

    if input_tokens is not None and output_tokens is not None:
        cost = (input_tokens * in_price + output_tokens * out_price) / 1_000_000
        return round(cost, 6), "exact"

    if total_tokens is not None and in_price == out_price:
        return round((total_tokens * in_price) / 1_000_000, 6), "uniform_price_with_total"

    if total_tokens is not None:
        return None, "missing_input_or_output_breakdown"

    return None, "missing_tokens"


def resolve_price(model_key: str, model_name: str, table: dict[str, dict]) -> dict | None:
    return table.get(model_key) or table.get(model_name)


def selected(items: list[dict], csv: str | None, field: str) -> list[dict]:
    if not csv:
        return items
    wanted = set(csv.split(","))
    known = {item[field] for item in items}
    missing = wanted - known
    if missing:
        raise SystemExit(f"unknown {field}: {', '.join(sorted(missing))}")
    return [item for item in items if item[field] in wanted]


def build_conditions(raw: str | None) -> list[str]:
    if not raw:
        return list(CONDITIONS)
    selected_conditions = [value.strip() for value in raw.split(",") if value.strip()]
    unknown = [value for value in selected_conditions if value not in CONDITIONS]
    if unknown:
        raise ValueError(f"unknown conditions: {', '.join(sorted(unknown))}")
    return selected_conditions


def candidate_map(candidates: list[dict]) -> dict[str, dict]:
    return {entry["key"]: entry for entry in candidates}


def default_candidate_pair(candidates: list[dict], large: str | None, small: str | None) -> tuple[dict, dict]:
    lookup = candidate_map(candidates)
    large_key = large or "codex:gpt-5.6-sol"
    small_key = small or "codex:gpt-5.6-luna"
    if large_key not in lookup:
        raise SystemExit(f"unknown large model key: {large_key}")
    if small_key not in lookup:
        raise SystemExit(f"unknown small model key: {small_key}")
    return lookup[large_key], lookup[small_key]


def planner_command(candidate: dict, task_prompt: str, cwd: Path):
    prompt = (
        "Read-only planning session. Do not edit files.\n"
        "Inspect the local repository context and provide a concrete implementation plan.\n\n"
        f"Task:\n{task_prompt}\n\n"
        f"Repository path: {cwd}. Do not use any scratch project."
    )
    cmd = base_run.reviewer_command(candidate, prompt, cwd)
    if candidate["agent"] == "agy":
        # candidate_command's agy anchor tells the agent to "edit files there",
        # which contradicts a read-only planner even after reviewer_command
        # flips --mode to plan; replace the prompt outright instead of the
        # accept-edits phrasing.
        argv = list(cmd.argv)
        prompt_index = argv.index("-p") + 1
        argv[prompt_index] = (
            f"Read-only planning session. The only repository to inspect is {cwd}; "
            "do not create or use an Antigravity scratch project. Do not edit, create, "
            "or delete any files -- only read and analyze.\n\n"
            f"Task:\n{task_prompt}\n\n"
            "Provide a concrete implementation plan without making any changes."
        )
        cmd = base_run.Command(argv, cmd.env)
    return cmd


def implementer_prompt(task_prompt: str, planner_output: str | None) -> str:
    if not planner_output:
        return task_prompt
    return (
        f"Original task:\n{task_prompt}\n\n"
        "Implementation plan from independent planner:\n"
        f"{planner_output}\n\n"
        "Implement exactly from this plan, and keep changes minimal."
    )


def run_stage(
    candidate: dict,
    prompt: str,
    cwd: Path,
    price_table: dict[str, dict],
    is_planner: bool = False,
) -> dict:
    cmd = planner_command(candidate, prompt, cwd) if is_planner else base_run.candidate_command(candidate, prompt, cwd)
    start = time.time()
    timed_out = False
    try:
        proc = base_run.run(cmd.argv, cwd=cwd, env=cmd.env, timeout=TIMEOUT)
        exit_code = proc.returncode
        output = bounded_output((proc.stdout or "") + "\n" + (proc.stderr or ""))
        error = None
    except (OSError, subprocess.TimeoutExpired) as exc:
        exit_code = -1
        timed_out = isinstance(exc, subprocess.TimeoutExpired)
        if timed_out:
            output = bounded_output((exc.stdout or "") + "\n" + (exc.stderr or ""))
        else:
            output = ""
        error = str(exc)

    tokens = parse_token_usage(output)
    price = resolve_price(candidate["key"], candidate["model"], price_table)
    cost_usd, cost_coverage = estimate_cost(tokens, price)

    return {
        "model": candidate["key"],
        "exit": exit_code,
        "timed_out": timed_out,
        "secs": round(time.time() - start, 2),
        "output": output,
        "error": error,
        "tokens": tokens,
        "cost_usd": cost_usd,
        "cost_coverage": cost_coverage,
    }


def planner_failure_reason(stage: dict) -> str | None:
    """Return why a planner stage cannot be trusted as an independent plan, or None if it can."""
    if stage.get("timed_out"):
        return "planner_timeout"
    if stage["exit"] != 0:
        return "planner_exit_nonzero"
    if not (stage.get("output") or "").strip():
        return "planner_empty_output"
    if stage.get("tokens", {}).get("parse_error"):
        return "planner_token_parse_error"
    return None


def run_condition(
    task: dict,
    condition: str,
    large: dict,
    small: dict,
    run_id: str,
    repeat: int,
    reviewers: list[str],
    all_candidates: dict[str, dict],
    args,
    price_table: dict[str, dict],
) -> dict:
    root = base_run.setup_repo(task, run_id)
    impl_candidate = large if condition == "large_direct" else small

    row = {
        "task": task["id"],
        "task_type": task.get("task_type"),
        "difficulty": task.get("difficulty"),
        "language": task.get("language"),
        "condition": condition,
        "repeat": repeat,
        "planner": None,
        "implementation": None,
        "passed": False,
        "invalid": False,
        "invalid_reason": None,
        "exit": -1,
        "secs": 0.0,
        "output": "",
        "checks": [],
        "reviews": [],
        "total_cost_usd": None,
    }

    try:
        planner_output = None
        if condition == "large_plan_small":
            row["planner"] = run_stage(large, task["prompt"], root, price_table, is_planner=True)
            failure_reason = planner_failure_reason(row["planner"])
            if failure_reason:
                # An unavailable plan must not silently fall back to a bare
                # task prompt (that would covertly become small_direct while
                # still being counted as large_plan_small).
                row["invalid"] = True
                row["invalid_reason"] = failure_reason
                row["secs"] = row["planner"]["secs"]
                row["checks"] = [{"argv": [], "ok": False, "stderr": f"planner unavailable: {failure_reason}", "stdout": ""}]
                return row
            planner_output = row["planner"].get("output")

        row["implementation"] = run_stage(
            impl_candidate,
            implementer_prompt(task["prompt"], planner_output),
            root,
            price_table,
            is_planner=False,
        )

        row["exit"] = row["implementation"]["exit"]
        row["output"] = row["implementation"]["output"]

        row["secs"] = round(
            (row["planner"]["secs"] if row["planner"] else 0.0) + row["implementation"]["secs"],
            2,
        )

        if row["implementation"]["exit"] == 0:
            row["passed"], row["checks"] = base_run.verify(root, task)
        else:
            row["passed"] = False
            row["checks"] = [{"argv": [], "ok": False, "stderr": "implementation command failed", "stdout": ""}]

        impl_cost = row["implementation"].get("cost_usd")
        planner_cost = row["planner"].get("cost_usd") if row["planner"] else 0.0
        if impl_cost is None:
            row["total_cost_usd"] = None
        elif row["planner"] and row["planner"].get("cost_usd") is None:
            row["total_cost_usd"] = None
        else:
            row["total_cost_usd"] = round((planner_cost or 0.0) + impl_cost, 6)

        if row["passed"] and not args.skip_review:
            row["reviews"] = base_run.review_solution(
                root,
                task,
                row["implementation"]["model"],
                reviewers,
                all_candidates,
            )
    finally:
        shutil.rmtree(root, ignore_errors=True)

    return row


def summarize(rows: list[dict]) -> dict:
    by_condition: dict[str, list[dict]] = {}
    for row in rows:
        by_condition.setdefault(row["condition"], []).append(row)

    summaries = []
    for condition, entries in sorted(by_condition.items()):
        # Rows where the planner was unavailable never ran an independent
        # implementation and must not count toward quality/cost comparison.
        valid_entries = [r for r in entries if not r.get("invalid")]
        invalid_entries = [r for r in entries if r.get("invalid")]

        attempts = len(valid_entries)
        passes = [r for r in valid_entries if r["passed"]]
        valid_reviews = [r for p in passes for r in p.get("reviews", []) if not r.get("parse_error")]
        critical_findings = sum(int(r.get("critical_findings", 0)) for r in valid_reviews)

        success_costs = [r["total_cost_usd"] for r in passes if r.get("total_cost_usd") is not None]
        entries_with_planner = [e for e in valid_entries if e.get("planner")]

        summaries.append({
            "condition": condition,
            "attempts": attempts,
            "passes": len(passes),
            "pass_rate": round(len(passes) / attempts, 4) if attempts else 0.0,
            "invalid_count": len(invalid_entries),
            "invalid_reasons": sorted({r["invalid_reason"] for r in invalid_entries if r.get("invalid_reason")}),
            "median_secs": round(statistics.median([r["secs"] for r in valid_entries]), 2) if valid_entries else 0.0,
            "median_implementer_secs": round(
                statistics.median([r["implementation"]["secs"] for r in valid_entries]) if valid_entries else 0.0,
                2,
            ),
            "median_planner_secs": round(
                statistics.median([r["planner"]["secs"] for r in entries_with_planner]) if entries_with_planner else 0.0,
                2,
            ),
            "critical_findings": critical_findings,
            "review_count": len(valid_reviews),
            "critical_per_review": round(critical_findings / len(valid_reviews), 4) if valid_reviews else 1.0,
            "median_cost_usd": round(statistics.median(success_costs), 6) if success_costs else None,
            "cost_coverage": {
                "costed_success_runs": len(success_costs),
                "success_runs": len(passes),
            },
        })

    return {
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "conditions": summaries,
    }


def aggregate_task_rows(rows: list[dict]) -> list[dict]:
    task_rows: dict[tuple[str, str], list[dict]] = {}
    for row in rows:
        task_rows.setdefault((row["task"], row["condition"]), []).append(row)

    output = []
    for (task, condition), entries in sorted(task_rows.items()):
        valid_entries = [row for row in entries if not row.get("invalid")]
        invalid_entries = [row for row in entries if row.get("invalid")]
        passes = [row for row in valid_entries if row["passed"]]
        output.append({
            "task": task,
            "condition": condition,
            "attempts": len(valid_entries),
            "passes": len(passes),
            "pass_rate": round(len(passes) / len(valid_entries), 4) if valid_entries else 0.0,
            "invalid_count": len(invalid_entries),
        })
    return output


def render_report(meta: dict, summary: dict, task_rows: list[dict]) -> str:
    lines = [
        "# Direct-CLI delegation benchmark",
        "",
        "No JINDO/MCP, plugins, skills, web search, or other agents.",
        "",
        "## Aggregate by condition",
        "| condition | pass | invalid | critical/review | median total sec | median impl sec | median plan sec | median cost/run (USD, successful) |",
        "|---|---:|---:|---:|---:|---:|---:|---:|",
    ]
    for row in sorted(summary["conditions"], key=lambda x: x["condition"]):
        cost = "null" if row["median_cost_usd"] is None else str(row["median_cost_usd"])
        lines.append(
            f"| {row['condition']} | {row['passes']}/{row['attempts']} | {row['invalid_count']} | {row['critical_per_review']:.3f} "
            f"| {row['median_secs']:.2f} | {row['median_implementer_secs']:.2f} | {row['median_planner_secs']:.2f} | {cost} |"
        )

    lines += [
        "",
        "## Pass rate by task and condition",
        "| task | condition | pass | invalid |",
        "|---|---|---:|---:|",
    ]
    for row in task_rows:
        lines.append(f"| {row['task']} | {row['condition']} | {row['passes']}/{row['attempts']} | {row['invalid_count']} |")

    lines += [
        "",
        "CLI versions:",
        f"{json.dumps(meta['cli_versions'], ensure_ascii=False)}",
    ]
    return "\n".join(lines)


def parse_price_table(path: Path | None) -> dict[str, dict]:
    if path is None:
        return dict(DEFAULT_PRICE_TABLE)
    data = json.loads(path.read_text())
    if not isinstance(data, dict):
        raise SystemExit("--price-table must contain a JSON object")
    table: dict[str, dict] = {}
    for key, value in data.items():
        if not isinstance(value, dict):
            continue
        input_price = value.get("input_per_million")
        output_price = value.get("output_per_million")
        if isinstance(input_price, (int, float)) and isinstance(output_price, (int, float)):
            table[str(key)] = {
                "input_per_million": float(input_price),
                "output_per_million": float(output_price),
            }
    return table


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--list-models", action="store_true")
    parser.add_argument("--list-conditions", action="store_true")
    parser.add_argument("--tasks", help="comma-separated task ids")
    parser.add_argument("--conditions", help="comma-separated conditions")
    parser.add_argument("--large-model", help="candidate key for large model")
    parser.add_argument("--small-model", help="candidate key for small model")
    parser.add_argument("--reviewers", default=",".join(base_run.REVIEWERS))
    parser.add_argument("--repeats", type=int, default=REPEATS_DEFAULT)
    parser.add_argument("--output-dir", type=Path, default=OUT)
    parser.add_argument("--resume", action="store_true")
    parser.add_argument("--skip-review", action="store_true")
    parser.add_argument("--price-table", type=Path, default=None)
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--self-test-fixtures", action="store_true")
    args = parser.parse_args()

    candidates = base_run.inventory()
    candidates_by_key = candidate_map(candidates)
    meta = {
        "cli_versions": base_run.cli_versions(),
        "models": candidates,
        "agy_discovered": base_run.agy_models(),
    }

    if args.list_models:
        print(json.dumps(meta, indent=2, ensure_ascii=False))
        return
    if args.list_conditions:
        print(json.dumps(list(CONDITIONS), indent=2, ensure_ascii=False))
        return

    conditions = build_conditions(args.conditions)
    tasks = selected(TASKS, args.tasks, "id")
    if args.repeats < 1:
        raise SystemExit("--repeats must be at least 1")

    if args.self_test_fixtures:
        failures = base_run.validate_fixtures(tasks)
        print(json.dumps(failures, indent=2, ensure_ascii=False))
        raise SystemExit(1 if failures else 0)

    large, small = default_candidate_pair(candidates, args.large_model, args.small_model)
    price_table = parse_price_table(args.price_table)
    reviewers = [item for item in args.reviewers.split(",") if item]

    out = args.output_dir.resolve()
    out.mkdir(parents=True, exist_ok=True)
    base_run.WORK.mkdir(parents=True, exist_ok=True)

    results_path = out / "results.json"
    rows = json.loads(results_path.read_text()) if args.resume and results_path.exists() else []
    done = {(row["task"], row["condition"], row["repeat"]) for row in rows if row.get("exit") == 0}

    if args.dry_run:
        for task in tasks:
            for condition in conditions:
                for repeat in range(args.repeats):
                    payload = {
                        "task": task["id"],
                        "condition": condition,
                        "repeat": repeat,
                        "planner": condition == "large_plan_small",
                        "implementer": True,
                    }
                    print(json.dumps(payload))
        return

    for task in tasks:
        for condition in conditions:
            for repeat in range(args.repeats):
                identity = (task["id"], condition, repeat)
                if identity in done:
                    continue
                pair_tag = re.sub(r"[^A-Za-z0-9_.-]+", "_", f"{large['key']}-{small['key']}")
                run_id = f"{task['id']}-{condition}-{pair_tag}-{repeat}"
                row = run_condition(
                    task,
                    condition,
                    large=large,
                    small=small,
                    run_id=run_id,
                    repeat=repeat,
                    reviewers=reviewers,
                    all_candidates=candidates_by_key,
                    args=args,
                    price_table=price_table,
                )
                rows = [entry for entry in rows if (entry["task"], entry["condition"], entry["repeat"]) != identity]
                rows.append(row)
                results_path.write_text(json.dumps(rows, indent=2, ensure_ascii=False))
                print(
                    f"{task['id']} x {condition} #{repeat}: pass={row['passed']} "
                    f"invalid={row['invalid_reason'] if row['invalid'] else False} reviews={len(row['reviews'])}",
                    flush=True,
                )

    summary = summarize(rows)
    task_rows = aggregate_task_rows(rows)
    (out / "summary.json").write_text(json.dumps({"summary": summary, "task_rows": task_rows}, indent=2, ensure_ascii=False))
    (out / "report.md").write_text(render_report(meta, summary, task_rows) + "\n")
    (out / "inventory.json").write_text(json.dumps(meta, indent=2, ensure_ascii=False))
    print(f"wrote {out / 'report.md'}")


if __name__ == "__main__":
    main()
