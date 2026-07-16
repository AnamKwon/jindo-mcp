#!/usr/bin/env python3
"""Direct-CLI reviewer calibration on labeled good and defective code cases."""
from __future__ import annotations

import argparse
import hashlib
import json
import re
import shutil
import statistics
import tempfile
import time
from collections import defaultdict
from pathlib import Path

import run as coding_bench

REPO = Path(__file__).resolve().parents[1]
OUT = REPO / "bench" / "calibration" / "reviewers"
WORK = Path(tempfile.gettempdir()) / "jindo-reviewer-routing-bench"

LABELS = {
    "cancellation_propagation": "one cancelled waiter cancels a shared async load",
    "stale_repopulation": "an in-flight load can repopulate after invalidate",
    "inflight_identity_race": "an older load can remove a newer in-flight task for the same key",
    "ttl_origin_error": "expiry is measured before loader completion instead of after it",
    "loader_failure_cached": "a loader exception is retained so later calls cannot retry",
    "cross_key_serialization": "an unrelated key is blocked behind another key's loader",
    "invalid_ttl_side_effect": "an invalid ttl invokes user loader code before rejecting the call",
    "lock_held_across_loader": "the cache lock is held while awaiting user loader code",
    "non_atomic_rollback": "a failed multi-operation commit leaves partial mutations",
    "integer_overflow": "unchecked arithmetic can wrap or panic instead of returning Invalid",
    "lock_poison_panic": "a poisoned lock is unwrapped and panics despite a no-panic contract",
    "empty_commit_version_bump": "an empty valid commit increments the version instead of being a no-op",
    "late_conflict_check": "state is mutated before validating the optimistic base version",
    "missing_delete_validation": "deleting a missing key succeeds instead of returning Invalid",
    "snapshot_aliasing": "a snapshot shares mutable storage with later commits",
    "version_overflow": "version increment can overflow instead of returning Invalid atomically",
    "self_transfer_allowed": "a transfer permits identical source and destination accounts",
}

CASES = [
    {
        "id": "python_cancel_mutant", "language": "python",
        "expected": ["cancellation_propagation"],
        "contract": "Same-key callers share one asyncio.Task. Cancelling one waiter must not cancel that shared load for other waiters.",
        "candidate": '''async def get(self, key, ttl, loader):
    async with self._lock:
        task = self._inflight.get(key)
        if task is None:
            task = asyncio.create_task(self._load(key, ttl, loader))
            self._inflight[key] = task
    return await task''',
    },
    {
        "id": "python_generation_mutant", "language": "python",
        "expected": ["stale_repopulation"],
        "contract": "invalidate removes the cache and an older in-flight load may return to current waiters but must not repopulate afterward.",
        "candidate": '''async def _load(self, key, ttl, loader):
    value = await loader()
    async with self._lock:
        self._values[key] = (value, self._clock() + ttl)
        self._inflight.pop(key, None)
    return value

async def invalidate(self, key):
    async with self._lock:
        self._values.pop(key, None)''',
    },
    {
        "id": "python_identity_mutant", "language": "python",
        "expected": ["inflight_identity_race"],
        "contract": "invalidate detaches an old in-flight task without cancelling it, allowing a fresh load. Old completion must not remove the newer task's registry entry.",
        "candidate": '''async def _load(self, key, loader):
    try:
        return await loader()
    finally:
        async with self._lock:
            self._inflight.pop(key, None)

async def invalidate(self, key):
    async with self._lock:
        self._values.pop(key, None)
        self._inflight.pop(key, None)''',
    },
    {
        "id": "python_ttl_mutant", "language": "python",
        "expected": ["ttl_origin_error"],
        "contract": "A successful value expires ttl seconds after loader completion according to the injected clock.",
        "candidate": '''async def get(self, key, ttl, loader):
    started = self._clock()
    value = await loader()
    async with self._lock:
        self._values[key] = (value, started + ttl)
    return value''',
    },
    {
        "id": "python_known_good", "language": "python", "expected": [],
        "contract": "Same-key callers share a task; cancellation is waiter-local; invalidate advances a generation so old loads cannot cache.",
        "candidate": '''async def get(self, key, ttl, loader):
    async with self._lock:
        task = self._inflight.get(key)
        if task is None:
            generation = self._generation.get(key, 0)
            task = asyncio.create_task(self._load(key, generation, ttl, loader))
            self._inflight[key] = task
    return await asyncio.shield(task)

async def invalidate(self, key):
    async with self._lock:
        self._generation[key] = self._generation.get(key, 0) + 1
        self._values.pop(key, None)

async def _load(self, key, generation, ttl, loader):
    value = await loader()
    async with self._lock:
        if self._generation.get(key, 0) == generation:
            self._values[key] = (value, self._clock() + ttl)
        self._inflight.pop(key, None)
    return value''',
    },
    {
        "id": "python_failure_cache_mutant", "language": "python",
        "expected": ["loader_failure_cached"],
        "contract": "A loader failure is observed by current waiters but is never cached; a later get creates a fresh loader task.",
        "candidate": '''async def get(self, key, ttl, loader):
    async with self._lock:
        task = self._inflight.get(key)
        if task is None:
            task = asyncio.create_task(loader())
            self._inflight[key] = task
    return await asyncio.shield(task)''',
    },
    {
        "id": "python_cross_key_mutant", "language": "python",
        "expected": ["cross_key_serialization", "lock_held_across_loader"],
        "contract": "Different keys load independently, and no cache lock is held while awaiting user loader code.",
        "candidate": '''async def get(self, key, ttl, loader):
    async with self._lock:
        cached = self._values.get(key)
        if cached is not None:
            return cached[0]
        value = await loader()
        self._values[key] = (value, self._clock() + ttl)
        return value''',
    },
    {
        "id": "python_invalid_ttl_mutant", "language": "python",
        "expected": ["invalid_ttl_side_effect"],
        "contract": "ttl must be positive and an invalid call must reject before invoking loader.",
        "candidate": '''async def get(self, key, ttl, loader):
    value = await loader()
    if ttl <= 0:
        raise ValueError("ttl must be positive")
    return value''',
    },
    {
        "id": "python_expiry_known_good", "language": "python", "expected": [],
        "contract": "Expiry is ttl after successful loader completion according to the injected clock.",
        "candidate": '''async def _load(self, key, ttl, loader):
    value = await loader()
    completed = self._clock()
    async with self._lock:
        self._values[key] = (value, completed + ttl)
    return value''',
    },
    {
        "id": "rust_rollback_mutant", "language": "rust",
        "expected": ["non_atomic_rollback"],
        "contract": "Every ordered operation commits atomically. Any later invalid operation returns Invalid with all values and version unchanged.",
        "candidate": '''for op in ops {
    match op {
        Op::Put(k, v) if *v >= 0 => { state.values.insert(k.clone(), *v); }
        Op::Transfer(from, to, n) if *n > 0 => {
            let a = state.values.get_mut(from).ok_or(CommitError::Invalid)?;
            if *a < *n { return Err(CommitError::Invalid); }
            *a -= *n;
            *state.values.get_mut(to).ok_or(CommitError::Invalid)? += *n;
        }
        _ => return Err(CommitError::Invalid),
    }
}
state.version += 1;''',
    },
    {
        "id": "rust_overflow_mutant", "language": "rust",
        "expected": ["integer_overflow"],
        "contract": "Transfer uses checked arithmetic. Overflow returns Invalid and the entire candidate transaction is discarded.",
        "candidate": '''let mut next = state.values.clone();
let from_value = next[from];
let to_value = next[to];
if from_value < amount { return Err(CommitError::Invalid); }
next.insert(from.clone(), from_value - amount);
next.insert(to.clone(), to_value + amount);
state.values = next;''',
    },
    {
        "id": "rust_poison_mutant", "language": "rust",
        "expected": ["lock_poison_panic"],
        "contract": "commit must return Invalid rather than panic when the internal RwLock has been poisoned.",
        "candidate": '''pub fn commit(&self, base: u64, ops: &[Op]) -> Result<u64, CommitError> {
    let mut state = self.state.write().unwrap();
    if state.version != base { return Err(CommitError::Conflict); }
    apply_atomically(&mut state, ops)
}''',
    },
    {
        "id": "rust_known_good", "language": "rust", "expected": [],
        "contract": "Transfer requires existing distinct accounts, positive amount, sufficient funds, checked arithmetic, and atomic candidate-state commit.",
        "candidate": '''if from == to || *amount <= 0 { return Err(CommitError::Invalid); }
let mut next = state.values.clone();
let from_value = *next.get(from).ok_or(CommitError::Invalid)?;
let to_value = *next.get(to).ok_or(CommitError::Invalid)?;
let new_from = from_value.checked_sub(*amount).filter(|v| *v >= 0).ok_or(CommitError::Invalid)?;
let new_to = to_value.checked_add(*amount).ok_or(CommitError::Invalid)?;
next.insert(from.clone(), new_from);
next.insert(to.clone(), new_to);
let version = state.version.checked_add(1).ok_or(CommitError::Invalid)?;
state.values = next; state.version = version;''',
    },
    {
        "id": "rust_empty_commit_mutant", "language": "rust",
        "expected": ["empty_commit_version_bump"],
        "contract": "After validating base_version, an empty commit is a no-op returning the current version.",
        "candidate": '''if state.version != base { return Err(CommitError::Conflict); }
state.version = state.version.checked_add(1).ok_or(CommitError::Invalid)?;
if ops.is_empty() { return Ok(state.version); }''',
    },
    {
        "id": "rust_late_conflict_mutant", "language": "rust",
        "expected": ["late_conflict_check"],
        "contract": "An inexact base_version returns Conflict without any mutation.",
        "candidate": '''let mut next = state.values.clone();
apply(&mut next, ops)?;
state.values = next;
if state.version != base { return Err(CommitError::Conflict); }
state.version += 1;''',
    },
    {
        "id": "rust_delete_mutant", "language": "rust",
        "expected": ["missing_delete_validation"],
        "contract": "Delete requires an existing key; a missing key makes the whole commit Invalid.",
        "candidate": '''match op {
    Op::Delete(key) => { next.remove(key); }
    _ => apply_other(&mut next, op)?,
}''',
    },
    {
        "id": "rust_snapshot_mutant", "language": "rust",
        "expected": ["snapshot_aliasing"],
        "contract": "Snapshot is an isolated clone whose values cannot change during later commits.",
        "candidate": '''pub struct Snapshot {
    pub version: u64,
    pub values: Arc<RwLock<HashMap<String, i64>>>,
}
pub fn snapshot(&self) -> Snapshot {
    Snapshot { version: self.version(), values: Arc::clone(&self.values) }
}''',
    },
    {
        "id": "rust_version_overflow_mutant", "language": "rust",
        "expected": ["version_overflow"],
        "contract": "A non-empty commit increments version exactly once with checked arithmetic; overflow is Invalid without mutation.",
        "candidate": '''let mut next = state.values.clone();
apply(&mut next, ops)?;
state.values = next;
state.version += 1;
Ok(state.version)''',
    },
    {
        "id": "rust_self_transfer_mutant", "language": "rust",
        "expected": ["self_transfer_allowed"],
        "contract": "Transfer requires distinct existing accounts and a positive amount.",
        "candidate": '''let from_value = *next.get(from).ok_or(CommitError::Invalid)?;
let to_value = *next.get(to).ok_or(CommitError::Invalid)?;
if amount <= 0 || from_value < amount { return Err(CommitError::Invalid); }
next.insert(from.clone(), from_value - amount);
next.insert(to.clone(), to_value + amount);''',
    },
    {
        "id": "rust_empty_known_good", "language": "rust", "expected": [],
        "contract": "Base version is checked first; an empty valid commit returns the unchanged current version.",
        "candidate": '''if state.version != base { return Err(CommitError::Conflict); }
if ops.is_empty() { return Ok(state.version); }
let next_version = state.version.checked_add(1).ok_or(CommitError::Invalid)?;''',
    },
]


def prompt_for(cases: list[dict]) -> str:
    public = [{k: case[k] for k in ("id", "language", "contract", "candidate")} for case in cases]
    labels = [{"label": key, "meaning": value} for key, value in LABELS.items()]
    return """Act as a read-only code reviewer. Do not use MCP, plugins, skills,
web search, files, or other agents. Review each independent case only against its
stated contract. Some candidates are completely correct; avoid speculative findings.
Use only the supplied defect labels. Return exactly one JSON object and no prose:
{"reviews":[{"id":"case id","verdict":"approved or defective","labels":["label"]}]}
Every id must appear exactly once. Allowed labels and cases:
""" + json.dumps({"labels": labels, "cases": public}, ensure_ascii=False)


def parse_reviews(text: str) -> dict[str, dict]:
    decoder = json.JSONDecoder()
    for match in re.finditer(r"\{", text):
        try:
            value, _ = decoder.raw_decode(text[match.start():])
        except json.JSONDecodeError:
            continue
        if not isinstance(value, dict) or not isinstance(value.get("reviews"), list):
            continue
        output = {}
        valid = True
        for review in value["reviews"]:
            if not isinstance(review, dict) or not {"id", "verdict", "labels"} <= review.keys():
                valid = False; break
            key = str(review["id"])
            labels = review["labels"]
            if key in output or not isinstance(labels, list):
                valid = False; break
            output[key] = {"verdict": str(review["verdict"]), "labels": [str(x) for x in labels]}
        if valid:
            return output
    return {}


def score_reviews(cases: list[dict], reviews: dict[str, dict]) -> dict:
    expected_labels = set(); reported_labels = set(); correct_verdicts = 0; good_cases = 0; false_positive_cases = 0
    for case in cases:
        expected = set(case["expected"])
        review = reviews.get(case["id"], {"verdict": "", "labels": []})
        reported = set(review["labels"]) & set(LABELS)
        expected_labels |= {(case["id"], label) for label in expected}
        reported_labels |= {(case["id"], label) for label in reported}
        predicted_defective = review["verdict"].lower() == "defective"
        if predicted_defective == bool(expected):
            correct_verdicts += 1
        if not expected:
            good_cases += 1
            if predicted_defective or reported:
                false_positive_cases += 1
    true_positive = len(expected_labels & reported_labels)
    precision = true_positive / len(reported_labels) if reported_labels else (1.0 if not expected_labels else 0.0)
    recall = true_positive / len(expected_labels) if expected_labels else 1.0
    return {
        "verdict_accuracy": round(correct_verdicts / len(cases), 4),
        "label_precision": round(precision, 4), "critical_recall": round(recall, 4),
        "false_positive_rate": round(false_positive_cases / good_cases, 4),
        "true_positive_labels": true_positive, "expected_labels": len(expected_labels),
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--models", help="comma-separated inventory keys")
    parser.add_argument("--repeats", type=int, default=1)
    parser.add_argument("--output-dir", type=Path, default=OUT,
                        help="campaign-specific artifact directory")
    parser.add_argument("--resume", action="store_true")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    models = coding_bench.selected(coding_bench.inventory(), args.models, "key")
    if args.repeats < 1: parser.error("repeats must be >= 1")
    print(f"matrix: {len(models)} reviewer models x {args.repeats} repeats; {len(CASES)} labeled cases")
    if args.dry_run:
        print(json.dumps([m["key"] for m in models], indent=2)); return
    out = args.output_dir.resolve()
    out.mkdir(parents=True, exist_ok=True); WORK.mkdir(parents=True, exist_ok=True)
    path = out / "results.json"
    rows = json.loads(path.read_text()) if args.resume and path.exists() else []
    done = {(row["model"], row["repeat"]) for row in rows}
    for model in models:
        for repeat in range(args.repeats):
            identity = (model["key"], repeat)
            if identity in done: continue
            root = WORK / hashlib.sha256("|".join(map(str, identity)).encode()).hexdigest()[:12]
            root.mkdir(parents=True, exist_ok=True)
            if not (root / ".git").exists(): coding_bench.run(["git", "init", "-q"], cwd=root, timeout=20)
            cmd = coding_bench.reviewer_command(model, prompt_for(CASES), root); started = time.time()
            try:
                proc = coding_bench.run(cmd.argv, cwd=root, env=cmd.env)
                output = proc.stdout + "\n" + proc.stderr; error = None; exit_code = proc.returncode
            except Exception as exc:
                output, error, exit_code = "", str(exc), -1
            reviews = parse_reviews(output)
            metrics = score_reviews(CASES, reviews)
            row = {"model": model["key"], "repeat": repeat, "seconds": round(time.time()-started, 2),
                   "exit": exit_code, "parse_ok": set(reviews) == {c["id"] for c in CASES},
                   "reviews": reviews, "metrics": metrics, "error": error, "output": output[-12000:]}
            rows.append(row); path.write_text(json.dumps(rows, indent=2, ensure_ascii=False))
            print(f"{model['key']} #{repeat}: recall={metrics['critical_recall']:.2f} precision={metrics['label_precision']:.2f} fp={metrics['false_positive_rate']:.2f} parse={row['parse_ok']}", flush=True)
            shutil.rmtree(root, ignore_errors=True)
    grouped = defaultdict(list)
    timings = defaultdict(list)
    for row in rows:
        grouped[row["model"]].append(row["metrics"])
        timings[row["model"]].append(row["seconds"])
    summary = []
    for model, metrics in sorted(grouped.items()):
        summary.append({"model": model, "repeats": len(metrics), **{
            key: round(sum(m[key] for m in metrics) / len(metrics), 4)
            for key in ("verdict_accuracy", "label_precision", "critical_recall", "false_positive_rate")
        }, "median_seconds": round(statistics.median(timings[model]), 2)})
    (out / "summary.json").write_text(json.dumps(summary, indent=2, ensure_ascii=False))


if __name__ == "__main__": main()
