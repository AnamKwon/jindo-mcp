#!/usr/bin/env python3
"""Direct-CLI adversarial calibration benchmark for jindo model routing.

The benchmark deliberately does not dispatch through jindo, MCP, plugins, or
skills.  Each candidate gets a fresh repository, the same prompt, and only its
CLI's built-in code tools.  Hidden tests decide correctness.  When more than one
candidate passes, independent read-only reviewers score the diffs; routing is
ordered by correctness, review quality, repeatability, and only then latency.

Run `python3 bench/run.py --list-models` before a campaign, then use --dry-run.
The full matrix is intentionally expensive; --models/--tasks make it resumable.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import statistics
import subprocess
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path

from adversarial_tasks import TASKS as GO_TASKS
from diverse_tasks import TASKS as DIVERSE_TASKS
from expansion_tasks import TASKS as EXPANSION_TASKS
from general_tasks import TASKS as GENERAL_TASKS
from language_tasks import TASKS as LANGUAGE_TASKS
from verification_tasks import TASKS as VERIFICATION_TASKS

TASKS = GO_TASKS + LANGUAGE_TASKS + DIVERSE_TASKS + GENERAL_TASKS + EXPANSION_TASKS + VERIFICATION_TASKS

REPO = Path(__file__).resolve().parents[1]
OUT = REPO / "bench" / "calibration"
WORK = Path(os.environ.get("JINDO_BENCH_WORK", tempfile.gettempdir())) / "jindo-routing-bench"
TIMEOUT = int(os.environ.get("JINDO_BENCH_TIMEOUT", "1800"))

# IDs are CLI-facing, not API guesses. agy inventory is refreshed live below.
STATIC_MODELS = [
    {"key": "codex:gpt-5.6-luna", "agent": "codex", "model": "gpt-5.6-luna", "effort": "high"},
    {"key": "codex:gpt-5.6-terra", "agent": "codex", "model": "gpt-5.6-terra", "effort": "high"},
    {"key": "codex:gpt-5.6-sol", "agent": "codex", "model": "gpt-5.6-sol", "effort": "high"},
    {"key": "codex:gpt-5.4-mini", "agent": "codex", "model": "gpt-5.4-mini", "effort": "high"},
    {"key": "codex:gpt-5.3-codex-spark", "agent": "codex", "model": "gpt-5.3-codex-spark", "effort": "high"},
    {"key": "codex:gpt-5.5", "agent": "codex", "model": "gpt-5.5", "effort": "high"},
    {"key": "claude:claude-sonnet-5", "agent": "claude", "model": "claude-sonnet-5", "effort": "high"},
    {"key": "claude:claude-opus-4-8", "agent": "claude", "model": "claude-opus-4-8", "effort": "high"},
    {"key": "claude:claude-fable-5", "agent": "claude", "model": "claude-fable-5", "effort": "high"},
    {"key": "claude:claude-haiku-4-5", "agent": "claude", "model": "claude-haiku-4-5", "effort": "high"},
]
REVIEWERS = ["codex:gpt-5.6-sol", "claude:claude-fable-5", "agy:Gemini 3.5 Flash (High)"]


@dataclass(frozen=True)
class Command:
    argv: list[str]
    env: dict[str, str]


def run(argv, *, cwd=None, env=None, timeout=TIMEOUT):
    return subprocess.run(argv, cwd=cwd, env=env, text=True, capture_output=True, timeout=timeout)


def cli_versions():
    result = {}
    for cli in ("codex", "claude", "agy"):
        try:
            p = run([cli, "--version"], timeout=20)
            result[cli] = (p.stdout or p.stderr).strip() if p.returncode == 0 else f"error: {p.stderr.strip()}"
        except (OSError, subprocess.TimeoutExpired) as exc:
            result[cli] = f"error: {exc}"
    return result


def agy_models():
    try:
        p = run(["agy", "models"], timeout=30)
    except (OSError, subprocess.TimeoutExpired):
        return []
    return [line.strip() for line in p.stdout.splitlines() if line.strip()] if p.returncode == 0 else []


def inventory():
    models = list(STATIC_MODELS)
    for name in agy_models():
        # agy is a multi-provider frontend; routing calibration here uses only
        # its native Gemini surface, avoiding duplicate Claude/OpenAI backends.
        if name.startswith("Gemini "):
            models.append({"key": f"agy:{name}", "agent": "agy", "model": name, "effort": "high"})
    return models


def candidate_command(candidate, prompt, cwd):
    agent, model = candidate["agent"], candidate["model"]
    env = os.environ.copy()
    env["JINDO_BENCH_MODE"] = "1"
    if agent == "codex":
        argv = ["codex", "exec", "-m", model, "-C", str(cwd), "-s", "workspace-write",
                "--ephemeral", "--ignore-user-config", "--ignore-rules", prompt]
    elif agent == "claude":
        argv = ["claude", "--safe-mode", "--disable-slash-commands", "--strict-mcp-config",
                "--mcp-config", '{"mcpServers":{}}', "--setting-sources", "", "--model", model,
                "--effort", candidate.get("effort", "high"), "--permission-mode", "acceptEdits",
                "--tools", "Bash,Read,Edit,Write,Glob,Grep", "--no-session-persistence", "-p", prompt]
    elif agent == "agy":
        anchored = (f"The only target repository is {cwd}. Begin by changing directory to that exact "
                    f"path and edit files there; do not create or use an Antigravity scratch project.\n\n{prompt}")
        argv = ["agy", "--model", model, "--mode", "accept-edits", "--sandbox",
                "--add-dir", str(cwd), "-p", anchored]
    else:
        raise ValueError(f"unknown agent: {agent}")
    return Command(argv, env)


def reviewer_command(candidate, prompt, cwd):
    cmd = candidate_command(candidate, prompt, cwd)
    argv = list(cmd.argv)
    if candidate["agent"] == "codex":
        argv[argv.index("workspace-write")] = "read-only"
    elif candidate["agent"] == "claude":
        argv[argv.index("acceptEdits")] = "plan"
        tools = argv.index("Bash,Read,Edit,Write,Glob,Grep")
        argv[tools] = "Bash,Read,Glob,Grep"
    elif candidate["agent"] == "agy":
        argv[argv.index("accept-edits")] = "plan"
    return Command(argv, cmd.env)


def write_files(root, files):
    for rel, content in files.items():
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)


def setup_repo(task, run_id):
    root = WORK / run_id
    if root.exists():
        shutil.rmtree(root)
    root.mkdir(parents=True)
    write_files(root, task["public_files"])
    run(["git", "init", "-q"], cwd=root)
    run(["git", "config", "user.email", "bench@localhost"], cwd=root)
    run(["git", "config", "user.name", "jindo-bench"], cwd=root)
    run(["git", "add", "-A"], cwd=root)
    run(["git", "commit", "-qm", "RED fixture"], cwd=root)
    return root


def verify(root, task):
    # Hidden tests are copied only after the model exits, then removed again.
    copied = []
    for rel, content in task["hidden_files"].items():
        target = root / rel
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content)
        copied.append(target)
    checks = []
    try:
        for argv in task["verify"]:
            try:
                p = run(argv, cwd=root, timeout=task.get("verify_timeout", 180))
                check = {"argv": argv, "ok": p.returncode == 0,
                         "stdout": p.stdout[-4000:], "stderr": p.stderr[-4000:]}
            except subprocess.TimeoutExpired as exc:
                check = {"argv": argv, "ok": False, "timeout": True,
                         "stdout": (exc.stdout or "")[-4000:], "stderr": (exc.stderr or "")[-4000:]}
            checks.append(check)
            if not check["ok"]:
                break
    finally:
        for path in copied:
            path.unlink(missing_ok=True)
    return all(c["ok"] for c in checks), checks


def validate_fixtures(tasks):
    """Validate fixtures: reference solutions fully; legacy Go fixtures compile."""
    failures = []
    for task in tasks:
        root = setup_repo(task, f"fixture-check-{task['id']}")
        write_files(root, task["hidden_files"])
        if task.get("reference_files"):
            starter_passed = True
            for argv in task["verify"]:
                p = run(argv, cwd=root, timeout=task.get("verify_timeout", 180))
                if p.returncode:
                    starter_passed = False
                    break
            if starter_passed:
                failures.append({"task": task["id"], "error": "RED starter unexpectedly passed hidden verification"})
            write_files(root, task["reference_files"])
            for argv in task["verify"]:
                p = run(argv, cwd=root, timeout=task.get("verify_timeout", 180))
                if p.returncode:
                    failures.append({"task": task["id"], "argv": argv, "stderr": p.stderr, "stdout": p.stdout})
                    break
            shutil.rmtree(root, ignore_errors=True)
            continue
        packages = sorted({"./" + str(Path(rel).parent) for rel in task["hidden_files"] if rel.endswith(".go")})
        for package in packages:
            p = run(["go", "test", package, "-run", "^$", "-count=1"], cwd=root, timeout=120)
            if p.returncode:
                failures.append({"task": task["id"], "package": package, "stderr": p.stderr, "stdout": p.stdout})
        shutil.rmtree(root, ignore_errors=True)
    return failures


def parse_review(text):
    matches = re.findall(r"\{[\s\S]*?\}", text)
    for raw in reversed(matches):
        try:
            obj = json.loads(raw)
        except json.JSONDecodeError:
            continue
        required = {"correctness", "invariants", "maintainability", "test_quality", "critical_findings"}
        if required <= obj.keys():
            scores = [max(0, min(10, float(obj[k]))) for k in ("correctness", "invariants", "maintainability", "test_quality")]
            obj["score"] = round(sum(scores) / len(scores), 3)
            return obj
    return {"score": 0.0, "critical_findings": 1, "parse_error": True, "raw": text[-2000:]}


def review_solution(root, task, author, reviewers, candidates):
    diff = run(["git", "diff", "--no-ext-diff", "HEAD"], cwd=root).stdout
    prompt = f"""You are a read-only benchmark judge. Do not edit files. Review the candidate diff
against the task contract and repository evidence. Passing tests is necessary but not sufficient:
look for concurrency, aliasing, determinism, rollback, compatibility, and error-path violations.
TASK:\n{task['prompt']}\nDIFF:\n{diff}\n
Return exactly one JSON object with numeric 0..10 fields correctness, invariants,
maintainability, test_quality; integer critical_findings; and concise rationale.
Do not use MCP, plugins, skills, web search, or other agents."""
    results = []
    for key in reviewers:
        if key == author:
            continue
        reviewer = candidates.get(key)
        if not reviewer:
            continue
        cmd = reviewer_command(reviewer, prompt, root)
        t0 = time.time()
        try:
            p = run(cmd.argv, cwd=root, env=cmd.env)
            parsed = parse_review(p.stdout + "\n" + p.stderr)
            results.append({"reviewer": key, "exit": p.returncode, "secs": round(time.time()-t0, 2), **parsed})
        except (OSError, subprocess.TimeoutExpired) as exc:
            results.append({"reviewer": key, "exit": -1, "score": 0.0, "critical_findings": 1, "error": str(exc)})
    return results


def aggregate(rows):
    grouped = {}
    for row in rows:
        g = grouped.setdefault((row["task"], row["model"]), [])
        g.append(row)
    summaries = []
    for (task, model), rs in grouped.items():
        passes = [r for r in rs if r["passed"]]
        review_scores = [x["score"] for r in passes for x in r.get("reviews", []) if not x.get("parse_error")]
        valid_reviews = [x for r in passes for x in r.get("reviews", []) if not x.get("parse_error")]
        critical = sum(int(x.get("critical_findings", 0)) for x in valid_reviews)
        summaries.append({
            "task": task, "model": model, "attempts": len(rs), "passes": len(passes),
            "pass_rate": round(len(passes) / len(rs), 4),
            "review_score": round(statistics.median(review_scores), 3) if review_scores else 0.0,
            "critical_findings": critical,
            "review_count": len(valid_reviews),
            "critical_per_review": round(critical / len(valid_reviews), 4) if valid_reviews else 1.0,
            "median_secs": round(statistics.median(r["secs"] for r in rs), 2),
        })
    return summaries


def proposal(summaries):
    tiers = {}
    task_by_id = {t["id"]: t for t in TASKS}
    for tier in sorted({t["difficulty"] for t in TASKS}):
        task_ids = {t["id"] for t in TASKS if t["difficulty"] == tier}
        models = sorted({s["model"] for s in summaries})
        ranked = []
        for model in models:
            ss = [s for s in summaries if s["model"] == model and s["task"] in task_ids]
            if not ss:
                continue
            ranked.append({
                "model": model,
                "objective_pass_rate": round(sum(s["passes"] for s in ss) / sum(s["attempts"] for s in ss), 4),
                "mean_review_score": round(sum(s["review_score"] for s in ss) / len(ss), 3),
                "critical_findings": sum(s["critical_findings"] for s in ss),
                "review_count": sum(s["review_count"] for s in ss),
                "critical_per_review": round(
                    sum(s["critical_findings"] for s in ss) / sum(s["review_count"] for s in ss), 4
                ) if sum(s["review_count"] for s in ss) else 1.0,
                "median_secs": round(statistics.median(s["median_secs"] for s in ss), 2),
            })
        ranked.sort(key=lambda x: (-x["objective_pass_rate"], x["critical_per_review"], -x["mean_review_score"], x["median_secs"], x["model"]))
        tiers[tier] = {"winner": ranked[0]["model"] if ranked else None, "ranking": ranked,
                       "tasks": sorted(task_ids), "ordering": "pass_rate desc, critical_per_review asc, review_score desc, latency asc"}
    return {"generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "tiers": tiers,
            "note": "Advisory calibration only; require repeated runs before changing production routing."}


def render_report(meta, summaries, prop):
    lines = ["# Direct-CLI adversarial routing calibration", "",
             "No jindo MCP, plugins, skills, or cross-agent implementation was used.", "",
             f"CLI versions: `{json.dumps(meta['cli_versions'], ensure_ascii=False)}`", "",
             "## Results", "", "| task | model | pass | review | critical/review | median sec |", "|---|---|---:|---:|---:|---:|"]
    for s in sorted(summaries, key=lambda x: (x["task"], -x["pass_rate"], -x["review_score"], x["model"])):
        lines.append(f"| {s['task']} | {s['model']} | {s['passes']}/{s['attempts']} | {s['review_score']:.3f} | {s['critical_per_review']:.3f} | {s['median_secs']:.2f} |")
    lines += ["", "## Routing proposal", ""]
    for tier, data in prop["tiers"].items():
        lines += [f"### {tier}", "", f"Winner: `{data['winner']}`", ""]
        for i, row in enumerate(data["ranking"], 1):
            lines.append(f"{i}. `{row['model']}` — pass {row['objective_pass_rate']:.1%}, review {row['mean_review_score']:.3f}, critical/review {row['critical_per_review']:.3f}, {row['median_secs']:.2f}s")
        lines.append("")
    return "\n".join(lines)


def selected(items, csv, field):
    if not csv:
        return items
    wanted = set(csv.split(","))
    missing = wanted - {x[field] for x in items}
    if missing:
        raise SystemExit(f"unknown {field}: {', '.join(sorted(missing))}")
    return [x for x in items if x[field] in wanted]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--list-models", action="store_true")
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--self-test-fixtures", action="store_true")
    ap.add_argument("--models", help="comma-separated inventory keys")
    ap.add_argument("--tasks", help="comma-separated task ids")
    ap.add_argument("--reviewers", default=",".join(REVIEWERS))
    ap.add_argument("--repeats", type=int, default=3)
    ap.add_argument("--output-dir", type=Path, default=OUT,
                    help="campaign-specific artifact directory (default: bench/calibration)")
    ap.add_argument("--skip-review", action="store_true")
    ap.add_argument("--resume", action="store_true")
    args = ap.parse_args()
    models = inventory()
    meta = {"cli_versions": cli_versions(), "models": models, "agy_discovered": agy_models()}
    if args.list_models:
        print(json.dumps(meta, indent=2, ensure_ascii=False)); return
    models = selected(models, args.models, "key")
    tasks = selected(TASKS, args.tasks, "id")
    if args.repeats < 1:
        raise SystemExit("--repeats must be >= 1")
    if args.self_test_fixtures:
        failures = validate_fixtures(tasks)
        print(json.dumps(failures, indent=2, ensure_ascii=False))
        raise SystemExit(1 if failures else 0)
    print(f"matrix: {len(tasks)} tasks x {len(models)} models x {args.repeats} repeats")
    if args.dry_run:
        for task in tasks:
            for model in models:
                print(task["id"], model["key"], candidate_command(model, "<prompt>", Path("<workdir>")).argv)
        return
    out = args.output_dir.resolve()
    out.mkdir(parents=True, exist_ok=True); WORK.mkdir(parents=True, exist_ok=True)
    raw_path = out / "results.json"
    rows = json.loads(raw_path.read_text()) if args.resume and raw_path.exists() else []
    # A bad candidate solution commonly exits the CLI successfully and fails the
    # hidden oracle; that is a completed benchmark outcome. Transport, timeout,
    # and quota failures are retryable and replaced on --resume.
    done = {(r["task"], r["model"], r["repeat"]) for r in rows if r.get("exit") == 0}
    candidates = {m["key"]: m for m in inventory()}
    reviewers = args.reviewers.split(",") if args.reviewers else []
    for task in tasks:
        for model in models:
            for repeat in range(args.repeats):
                identity = (task["id"], model["key"], repeat)
                if identity in done:
                    continue
                digest = hashlib.sha256("|".join(map(str, identity)).encode()).hexdigest()[:10]
                root = setup_repo(task, f"{task['id']}-{digest}")
                prompt = task["prompt"] + "\n\nDo not use MCP, plugins, skills, web search, or other agents. Inspect the repository, implement the smallest coherent fix, and run the visible tests."
                cmd = candidate_command(model, prompt, root); t0 = time.time()
                try:
                    p = run(cmd.argv, cwd=root, env=cmd.env)
                    exit_code, output, error = p.returncode, (p.stdout + "\n" + p.stderr)[-12000:], None
                except (OSError, subprocess.TimeoutExpired) as exc:
                    exit_code, output, error = -1, "", str(exc)
                passed, checks = verify(root, task)
                row = {"task": task["id"], "difficulty": task["difficulty"], "model": model["key"],
                       "language": task.get("language"), "task_type": task.get("task_type"),
                       "prompt_language": task.get("prompt_language", "english"),
                       "repeat": repeat, "passed": passed, "exit": exit_code, "secs": round(time.time()-t0, 2),
                       "checks": checks, "output": output, "error": error, "reviews": []}
                if passed and not args.skip_review:
                    row["reviews"] = review_solution(root, task, model["key"], reviewers, candidates)
                rows = [old for old in rows if (old["task"], old["model"], old["repeat"]) != identity]
                rows.append(row); raw_path.write_text(json.dumps(rows, indent=2, ensure_ascii=False))
                print(f"{task['id']} x {model['key']} #{repeat}: pass={passed} reviews={len(row['reviews'])}", flush=True)
                shutil.rmtree(root, ignore_errors=True)
    summaries = aggregate(rows); prop = proposal(summaries)
    (out / "summary.json").write_text(json.dumps(summaries, indent=2, ensure_ascii=False))
    (out / "routing_proposal.json").write_text(json.dumps(prop, indent=2, ensure_ascii=False))
    (out / "report.md").write_text(render_report(meta, summaries, prop) + "\n")
    (out / "inventory.json").write_text(json.dumps(meta, indent=2, ensure_ascii=False))
    print(f"wrote {out / 'report.md'}")


if __name__ == "__main__":
    main()
